package telegramstore

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/tgdrive/teldrive/v2/internal/cache"
)

const (
	// defaultUploadThreads is the number of parallel upload connections used
	// when UploadRequest.Threads is not positive.
	defaultUploadThreads = 4
	// telegramUploadPart is the part size of an upload, 512 KiB, which keeps a
	// single upload request within the size Telegram accepts for a document.
	telegramUploadPart = 512 * 1024
	// telegramReadChunk is the 1 MiB window Telegram serves per upload.getFile
	// call; a read plan must never cross one of its boundaries.
	telegramReadChunk = 1024 * 1024
	// telegramReadAlign is the 4 KiB granularity Telegram requires for the
	// offset and limit of an upload.getFile request.
	telegramReadAlign = 4 * 1024
	// defaultTelegramReadBuffers is the number of prefetched chunks a range
	// reader buffers when no option or pool setting overrides it.
	defaultTelegramReadBuffers = 32
	// defaultTelegramReadParallel is the number of concurrent chunk fetches per
	// stream, and the number of connections requested for a download session,
	// when no option or pool setting overrides it.
	defaultTelegramReadParallel = 4
	// defaultTelegramReadTimeout bounds one upload.getFile attempt; an attempt
	// that times out is retried while attempts remain.
	defaultTelegramReadTimeout = 30 * time.Second
	// defaultTelegramReadAttempts is how many times one chunk fetch is attempted
	// before the stream fails with the last error.
	defaultTelegramReadAttempts = 3
	// deleteBatchSize is the largest number of message IDs one
	// channels.deleteMessages call may carry.
	deleteBatchSize = 100
)

// Runner owns Telegram authentication, client lifetime, bot selection, retry,
// rate-limit, and flood-wait middleware. The callback is invoked only while the
// underlying gotd client is running.
type Runner interface {
	// Run starts the user's Telegram client, invokes fn with the running API,
	// and stops the client when fn returns, so authentication, updates,
	// reconnects, flood waits, and connection teardown share one lifetime. fn
	// runs at most once and must not be retained: its context is cancelled as
	// soon as the client stops. Implementations reject a non-positive user ID or
	// a nil callback with ErrInvalidRequest, report a client that never became
	// usable as ErrClientUnavailable, and wrap failures with the operation name.
	Run(ctx context.Context, userID int64, operation Operation, fn func(context.Context, *tg.Client) error) error
}

// BotProvider resolves the upload bots that must be members of newly created
// storage channels. Returning an empty list is valid for user-only deployments.
type BotProvider interface {
	// ChannelBots returns the accounts that must be promoted to channel admin
	// when a storage channel is created, usually the upload bots of the user.
	// It runs inside the management session that is creating the channel, so the
	// returned accounts must be usable with that API. An empty list is valid for
	// user-only deployments; an error aborts the creation.
	ChannelBots(ctx context.Context, userID int64, api *tg.Client) ([]tg.InputUserClass, error)
}

// GotdStorage is the production Storage implementation. Every call runs on a
// session of the target user through the configured Runner, so the storage
// keeps no per-user state and is safe for concurrent use.
type GotdStorage struct {
	// runner executes all Telegram calls and owns authentication and the client
	// lifetime. A nil runner makes every method return ErrInvalidRequest.
	runner Runner
	// botProvider, when set, supplies the bots that are made channel admins
	// after CreateChannel; nil creates user-only channels.
	botProvider BotProvider
	// downloadPool, when set, makes OpenDownloadSession lease a shared client;
	// nil gives every session its own private client.
	downloadPool *DownloadClientPool
	// downloadReadBuffers is the number of prefetched chunks per download
	// stream; it starts at defaultTelegramReadBuffers.
	downloadReadBuffers int
	// downloadReadParallel is the number of concurrent chunk fetches per stream
	// and the number of connections requested per download session; it starts at
	// defaultTelegramReadParallel.
	downloadReadParallel int
	// globalCache caches document locations so repeated reads of one message do
	// not resolve it again; nil disables caching.
	globalCache cache.Cacher
}

// GotdStorageOption adjusts a GotdStorage inside NewGotdStorage. Options run
// in the order given and nil options are skipped, so an unconditionally built
// option slice is safe to pass.
type GotdStorageOption func(*GotdStorage)

// WithBotProvider makes CreateChannel promote the bots returned by provider to
// channel admins before it returns, deleting the freshly created channel when
// that fails. A nil provider disables bot provisioning.
func WithBotProvider(provider BotProvider) GotdStorageOption {
	return func(storage *GotdStorage) { storage.botProvider = provider }
}

// WithDownloadClientPool routes OpenDownloadSession through pool, so sessions
// share pooled clients instead of each starting a private one. A nil pool
// keeps the private per-session behavior.
func WithDownloadClientPool(pool *DownloadClientPool) GotdStorageOption {
	return func(storage *GotdStorage) { storage.downloadPool = pool }
}

// WithDownloadReadBuffers sets the number of prefetched chunks per download
// stream. Values below one are ignored, so the default stays in place.
func WithDownloadReadBuffers(buffers int) GotdStorageOption {
	return func(storage *GotdStorage) {
		if buffers > 0 {
			storage.downloadReadBuffers = buffers
		}
	}
}

// WithDownloadReadParallel sets the number of concurrent chunk fetches per
// stream, which is also the connection count requested for a download session.
// Values below one are ignored, so the default stays in place.
func WithDownloadReadParallel(parallel int) GotdStorageOption {
	return func(storage *GotdStorage) {
		if parallel > 0 {
			storage.downloadReadParallel = parallel
		}
	}
}

// NewGotdStorage returns storage that runs Telegram calls through runner and
// caches document locations in c, which may be nil. Read concurrency starts at
// the package defaults and is changed by the given options; runner is not
// validated here, so a nil runner fails later with ErrInvalidRequest instead of
// at construction time.
func NewGotdStorage(runner Runner, c cache.Cacher, options ...GotdStorageOption) *GotdStorage {
	storage := &GotdStorage{runner: runner, globalCache: c, downloadReadBuffers: defaultTelegramReadBuffers, downloadReadParallel: defaultTelegramReadParallel}
	for _, option := range options {
		if option != nil {
			option(storage)
		}
	}
	return storage
}

// Upload sends request.Reader as one document and publishes it in the target
// channel. Every call creates a new message, so a retry after a partial failure
// can leave an orphan document behind. It returns ErrInvalidRequest for a
// malformed request, ErrSizeMismatch when Telegram stored a different size
// (returning the published part alongside the error, so the caller can delete
// it), and ErrMessageNotFound when the publish response carries no channel
// message. The reader is not closed, and Telegram errors are wrapped with the
// operation name.
func (s *GotdStorage) Upload(ctx context.Context, request UploadRequest) (StoredPart, error) {
	if s.runner == nil || request.UserID <= 0 || request.ChannelID == 0 || request.Reader == nil || request.Size < 0 || strings.TrimSpace(request.Name) == "" {
		return StoredPart{}, ErrInvalidRequest
	}
	threads := request.Threads
	if threads <= 0 {
		threads = defaultUploadThreads
	}

	var stored StoredPart
	err := s.runUpload(ctx, request.UserID, threads, func(runCtx context.Context, api *tg.Client) error {
		channel, err := inputChannel(runCtx, api, request.ChannelID)
		if err != nil {
			return err
		}
		uploaded, err := uploader.NewUploader(api).
			WithThreads(threads).
			WithPartSize(telegramUploadPart).
			Upload(runCtx, uploader.NewUpload(request.Name, request.Reader, request.Size))
		if err != nil {
			return fmt.Errorf("upload Telegram document bytes: %w", err)
		}

		document := message.UploadedDocument(uploaded).Filename(request.Name).ForceFile(true)
		response, err := message.NewSender(api).
			To(&tg.InputPeerChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash}).
			Media(runCtx, document)
		if err != nil {
			return fmt.Errorf("publish Telegram document message: %w", err)
		}
		messageID, documentSize, err := uploadedMessage(response)
		if err != nil {
			return err
		}
		if documentSize != request.Size {
			// Record the published message before failing: the caller can only
			// compensate for a document it can name.
			stored = StoredPart{ChannelID: request.ChannelID, MessageID: messageID, Size: documentSize}
			return fmt.Errorf("%w: got %d, want %d", ErrSizeMismatch, documentSize, request.Size)
		}
		stored = StoredPart{ChannelID: request.ChannelID, MessageID: messageID, Size: documentSize}
		return nil
	})
	if err != nil {
		return stored, err
	}
	if stored.MessageID == 0 {
		return StoredPart{}, ErrMessageNotFound
	}
	return stored, nil
}

// runUpload runs one upload callback on an upload session, spread over threads
// connections when the runner supports pooling. The operation is OperationUpload,
// so a session that may not upload is never selected for it.
func (s *GotdStorage) runUpload(ctx context.Context, userID int64, threads int, fn func(context.Context, *tg.Client) error) error {
	return runWithConnections(ctx, s.runner, userID, OperationUpload, threads, fn)
}

// validMetadataRequest reports whether request is well formed for a document
// lookup. Only a positive user ID, a non-zero channel ID, and a non-negative
// message ID can resolve: a zero message ID is still looked up and reported as
// a document lookup error, so it is accepted here. GotdStorage.Metadata and
// gotdDownloadSession.Metadata share this predicate, so the storage path and the
// session path reject the same requests with ErrInvalidRequest.
func validMetadataRequest(request MetadataRequest) bool {
	return request.UserID > 0 && request.ChannelID != 0 && request.MessageID >= 0
}

// validRangeRequest reports whether request is well formed for a range read. A
// message ID must be positive, the offset must not be negative, and the length
// is either -1, meaning "through the end of the document", or zero and above.
// GotdStorage.OpenRange and gotdDownloadSession.OpenRange share this predicate,
// so the storage path and the session path reject the same requests with
// ErrInvalidRequest.
func validRangeRequest(request RangeRequest) bool {
	return request.UserID > 0 && request.ChannelID != 0 && request.MessageID > 0 && request.Offset >= 0 && request.Length >= -1
}

// Metadata resolves the stored size of a document without transferring it,
// serving repeated lookups of the same message from the shared location cache.
// It runs on a single download session. A nil runner, non-positive user ID, zero
// channel ID, or negative message ID returns ErrInvalidRequest, while a message
// ID of zero is looked up and reported as a document lookup error. The returned
// part repeats the requested channel and message IDs and carries the size
// Telegram reported.
func (s *GotdStorage) Metadata(ctx context.Context, request MetadataRequest) (StoredPart, error) {
	if s.runner == nil || !validMetadataRequest(request) {
		return StoredPart{}, ErrInvalidRequest
	}
	var stored StoredPart
	err := s.runner.Run(ctx, request.UserID, OperationDownload, func(runCtx context.Context, api *tg.Client) error {
		_, size, err := fetchDocumentLocation(runCtx, api, request.ChannelID, request.MessageID, s.globalCache)
		if err != nil {
			return err
		}
		stored = StoredPart{ChannelID: request.ChannelID, MessageID: request.MessageID, Size: size}
		return nil
	})
	if err != nil {
		return StoredPart{}, err
	}
	return stored, nil
}

// OpenRange streams a byte range of a stored document. It returns the reader
// immediately and resolves the document location on a background download
// session, so lookup and download failures surface while reading rather than
// here; a range that starts past the end of the document reads as io.EOF without
// transferring anything. The reader belongs to the caller and must be closed to
// cancel the background download. A nil runner, non-positive user or message ID,
// zero channel ID, negative offset, or length below -1 returns
// ErrInvalidRequest.
func (s *GotdStorage) OpenRange(ctx context.Context, request RangeRequest) (io.ReadCloser, error) {
	if s.runner == nil || !validRangeRequest(request) {
		return nil, ErrInvalidRequest
	}
	streamCtx, cancel := context.WithCancel(ctx)
	reader := newTelegramRangeReader(streamCtx, cancel, s.downloadReadBuffers, s.downloadReadParallel)
	go func() {
		err := runWithConnections(streamCtx, s.runner, request.UserID, OperationDownload, s.downloadReadParallel, func(runCtx context.Context, api *tg.Client) error {
			location, documentSize, err := fetchDocumentLocation(runCtx, api, request.ChannelID, request.MessageID, s.globalCache)
			if err != nil {
				return err
			}
			refresh := func(refreshCtx context.Context) (*tg.InputDocumentFileLocation, error) {
				refreshed, _, refreshErr := refreshDocumentLocation(refreshCtx, api, request.ChannelID, request.MessageID, s.globalCache)
				return refreshed, refreshErr
			}
			return fillRangeWithLocation(runCtx, api, request, reader, location, documentSize, refresh)
		})
		reader.finish(err)
	}()
	return reader, nil
}

// fillRangeWithLocation clamps the requested range to the document and starts
// the read pipeline. A negative length, or one that runs past the end, is
// shortened to the remaining bytes; a range that starts past the end reports
// io.EOF without reading anything.
func fillRangeWithLocation(ctx context.Context, api *tg.Client, request RangeRequest, reader *telegramRangeReader, location *tg.InputDocumentFileLocation, documentSize int64, refresh func(context.Context) (*tg.InputDocumentFileLocation, error)) error {
	if request.Offset > documentSize {
		return io.EOF
	}
	remaining := request.Length
	if remaining < 0 || request.Offset+remaining > documentSize {
		remaining = documentSize - request.Offset
	}
	return reader.fill(ctx, api, location, request.Offset, remaining, refresh)
}

// gotdDownloadSession is one download session over a running client. A private
// session owns the client it started, while a pooled session borrows one through
// clientFn and returns it through closeFn. api, err, and clientID are guarded by
// mu; the channels and hooks are fixed before the session is handed out.
type gotdDownloadSession struct {
	// ctx is the session lifetime; cancelling it stops a private client run.
	ctx context.Context
	// cancel cancels ctx and is called by Close for a private session.
	cancel context.CancelFunc
	// ready is closed once the private client is usable, or once its run ended
	// without becoming usable.
	ready chan struct{}
	// done is closed when the private client run returned; OpenDownloadSession
	// and Close wait on it.
	done chan struct{}
	// api is the running client of a private session, nil until ready closes.
	// Guarded by mu.
	api *tg.Client
	// err is the private client run error, guarded by mu. It is reported instead
	// of ErrClientUnavailable when the session never became usable.
	err error
	// clientFn, when set, resolves the client of a pooled session on every use,
	// which is how a released or restarted pool slot is detected.
	clientFn func() (*tg.Client, error)
	// closeFn, when set, releases the pooled lease instead of stopping a private
	// client.
	closeFn func() error
	// close guards Close, so releasing the session twice is harmless.
	close sync.Once
	// closed reports that Close ran; a closed session never hands out a client.
	// Guarded by mu, so a concurrent Close and client() cannot race.
	closed bool
	// mu guards api, err, closed, and clientID.
	mu sync.Mutex
	// downloadReadBuffers is the number of prefetched chunks per range reader
	// opened by this session.
	downloadReadBuffers int
	// downloadReadParallel is the number of concurrent chunk fetches per range
	// reader, and the connection count requested for a private session.
	downloadReadParallel int
	// clientID is the Telegram account ID that scopes the document location
	// cache. It is zero until a private client is running; a pooled session is
	// given the ID of its slot.
	clientID int64

	// globalCache is the shared document location cache; nil disables caching.
	globalCache cache.Cacher
}

// cachedDocumentLocation is the cached value of one document lookup: the file
// location together with the document size, keyed per Telegram account, channel,
// and message.
type cachedDocumentLocation struct {
	// Location is the file reference Telegram returned. File references expire, so
	// a reader refreshes the location when a download reports
	// FILE_REFERENCE_EXPIRED.
	Location *tg.InputDocumentFileLocation `msgpack:"location"`
	// Size is the document size in bytes as Telegram reported it.
	Size int64 `msgpack:"size"`
}

// fetchDocumentLocation resolves a document message to its file location and
// size, caching the result for four hours per Telegram account, channel, and
// message when ctx carries a client ID and a cache is configured; without either
// it falls back to a plain lookup. A cache backend failure is reported as a
// lookup error, and a failed write back to the cache is ignored.
func fetchDocumentLocation(ctx context.Context, api *tg.Client, channelID, messageID int64, c cache.Cacher) (*tg.InputDocumentFileLocation, int64, error) {
	clientID, ok := ClientID(ctx)
	if c == nil || !ok {
		return documentLocation(ctx, api, channelID, messageID)
	}
	gk := cache.Key("telegram", "document", clientID, channelID, messageID)
	result, err := cache.Fetch(ctx, c, gk, 4*time.Hour, func() (cachedDocumentLocation, error) {
		loc, size, err := documentLocation(ctx, api, channelID, messageID)
		if err != nil {
			return cachedDocumentLocation{}, err
		}
		return cachedDocumentLocation{Location: loc, Size: size}, nil
	})
	if err != nil {
		return nil, 0, err
	}
	return result.Location, result.Size, nil
}

// refreshDocumentLocation drops the cached location of a message and resolves it
// again, which is how an expired file reference is replaced. Dropping a missing
// entry is not an error.
func refreshDocumentLocation(ctx context.Context, api *tg.Client, channelID, messageID int64, c cache.Cacher) (*tg.InputDocumentFileLocation, int64, error) {
	clientID, ok := ClientID(ctx)
	if c != nil && ok {
		_ = c.Delete(ctx, cache.Key("telegram", "document", clientID, channelID, messageID))
	}
	return fetchDocumentLocation(ctx, api, channelID, messageID, c)
}

// OpenDownloadSession returns a session bound to the user's download client,
// leasing one from the configured pool when there is a pool and otherwise
// starting a private client and waiting until it is authenticated. It returns
// ErrInvalidRequest for a nil or unconfigured storage or a non-positive user ID,
// ErrClientUnavailable when a private client stops before becoming usable, the
// pool's own error for a pooled session, and ctx.Err() when ctx ends first. The
// caller owns the session and must close it after the readers it opened.
func (s *GotdStorage) OpenDownloadSession(ctx context.Context, userID int64) (DownloadSession, error) {
	if s == nil || s.runner == nil || userID <= 0 {
		return nil, ErrInvalidRequest
	}
	if s.downloadPool != nil {
		return s.downloadPool.OpenDownloadSession(ctx, userID)
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	session := &gotdDownloadSession{
		ctx: sessionCtx, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{}),
		downloadReadBuffers:  s.downloadReadBuffers,
		downloadReadParallel: s.downloadReadParallel,
		globalCache:          s.globalCache,
	}
	go func() {
		err := runWithConnections(sessionCtx, s.runner, userID, OperationDownload, s.downloadReadParallel, func(runCtx context.Context, api *tg.Client) error {
			session.mu.Lock()
			session.api = api
			session.clientID, _ = ClientID(runCtx)
			session.mu.Unlock()
			close(session.ready)
			<-runCtx.Done()
			return runCtx.Err()
		})
		session.mu.Lock()
		session.err = err
		session.mu.Unlock()
		close(session.done)
	}()
	select {
	case <-session.ready:
		return session, nil
	case <-session.done:
		session.mu.Lock()
		err := session.err
		session.mu.Unlock()
		cancel()
		if err == nil {
			err = ErrClientUnavailable
		}
		return nil, err
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}

// Metadata resolves a document size through the session client and its location
// cache. It validates the request with the same predicate as
// GotdStorage.Metadata and returns ErrInvalidRequest for a malformed one before
// it touches the client. It must be called while the session is open: a closed
// session reports ErrClientUnavailable.
func (s *gotdDownloadSession) Metadata(ctx context.Context, request MetadataRequest) (StoredPart, error) {
	if !validMetadataRequest(request) {
		return StoredPart{}, ErrInvalidRequest
	}
	api, err := s.client()
	if err != nil {
		return StoredPart{}, err
	}
	_, size, err := s.documentLocation(ctx, api, request.ChannelID, request.MessageID)
	if err != nil {
		return StoredPart{}, err
	}
	return StoredPart{ChannelID: request.ChannelID, MessageID: request.MessageID, Size: size}, nil
}

// OpenRange opens a range reader on the session client, resolving the document
// location on a background goroutine. It validates the request with the same
// predicate as GotdStorage.OpenRange and returns ErrInvalidRequest for a
// malformed one before it touches the client, so a length below -1 or a negative
// offset never reaches Telegram. The caller owns the reader and must close it.
func (s *gotdDownloadSession) OpenRange(ctx context.Context, request RangeRequest) (io.ReadCloser, error) {
	if !validRangeRequest(request) {
		return nil, ErrInvalidRequest
	}
	api, err := s.client()
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	reader := newTelegramRangeReader(streamCtx, cancel, s.downloadReadBuffers, s.downloadReadParallel)
	go func() {
		location, size, locationErr := s.documentLocation(streamCtx, api, request.ChannelID, request.MessageID)
		if locationErr != nil {
			reader.finish(locationErr)
			return
		}
		refresh := func(refreshCtx context.Context) (*tg.InputDocumentFileLocation, error) {
			refreshed, _, refreshErr := s.refreshDocumentLocation(refreshCtx, api, request.ChannelID, request.MessageID)
			return refreshed, refreshErr
		}
		reader.finish(fillRangeWithLocation(streamCtx, api, request, reader, location, size, refresh))
	}()
	return reader, nil
}

// documentLocation resolves a document through the session cache, tagging ctx
// with the session's Telegram account ID so the cached location stays scoped to
// the account that fetched it.
func (s *gotdDownloadSession) documentLocation(ctx context.Context, api *tg.Client, channelID, messageID int64) (*tg.InputDocumentFileLocation, int64, error) {
	return fetchDocumentLocation(WithClientID(ctx, s.clientID), api, channelID, messageID, s.globalCache)
}

// refreshDocumentLocation forces a new resolution of the document location,
// used when Telegram reports that the cached file reference expired.
func (s *gotdDownloadSession) refreshDocumentLocation(ctx context.Context, api *tg.Client, channelID, messageID int64) (*tg.InputDocumentFileLocation, int64, error) {
	return refreshDocumentLocation(WithClientID(ctx, s.clientID), api, channelID, messageID, s.globalCache)
}

// client returns the API the caller must use for this session. It reports
// ErrClientUnavailable once Close ran, whether the session is pooled or private,
// so a closed session never hands out a client. An open pooled session asks the
// pool on every call, so a closed pool or a released slot reports
// ErrClientUnavailable as well; an open private session returns the client it
// started and reports the run error when it never became usable.
func (s *gotdDownloadSession) client() (*tg.Client, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, ErrClientUnavailable
	}
	if s.clientFn != nil {
		return s.clientFn()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.api == nil {
		if s.err != nil {
			return nil, s.err
		}
		return nil, ErrClientUnavailable
	}
	return s.api, nil
}

// Close releases the session: a pooled session returns its lease, a private
// session cancels its context and waits for the client run to finish. It marks
// the session closed before releasing the lease, so a later or concurrent
// client() reports ErrClientUnavailable instead of handing out the stopped
// client. It is idempotent, returns the release error of a pooled session, and
// must be called after every reader opened through OpenRange has been closed,
// because closing the session stops the client those readers use.
func (s *gotdDownloadSession) Close() error {
	var err error
	s.close.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		if s.closeFn != nil {
			err = s.closeFn()
			return
		}
		s.cancel()
		<-s.done
	})
	return err
}

// telegramRangeReader is a sequential io.ReadCloser over one byte range of a
// Telegram document. A fill goroutine fetches aligned chunks ahead of the
// consumer into a bounded channel, keeping at most parallel fetches in flight,
// and closes the channel when the range ends or fails. Read must be called from
// a single goroutine; Close is idempotent and cancels the fetch.
type telegramRangeReader struct {
	// ctx is the stream lifetime; it is cancelled by Close and by the caller of
	// newTelegramRangeReader.
	ctx context.Context
	// cancel cancels ctx, which aborts in-flight fetches.
	cancel context.CancelFunc
	// buffers carries prefetched chunks in range order; fill closes it when the
	// range ends, which makes Read report the terminal error or io.EOF.
	buffers chan *telegramRangeBuffer
	// done is closed by finish after buffers, so Close can wait for the fill
	// goroutine to stop.
	done chan struct{}
	// cur is the chunk currently being drained. Only Read touches it.
	cur *telegramRangeBuffer
	// readErr is the terminal fill error reported once buffers is drained. It is
	// guarded by mu because fill writes it from another goroutine.
	readErr error
	// finishOnce guards finish, so a stream ends exactly once.
	finishOnce sync.Once
	// closeOnce guards the cancel in Close.
	closeOnce sync.Once
	// mu guards readErr.
	mu sync.Mutex
	// parallel is the maximum number of chunk fetches in flight.
	parallel int
	// timeout bounds one upload.getFile attempt; a timed-out attempt is retried.
	timeout time.Duration
	// attempts is how many times one chunk fetch is attempted before it fails.
	attempts int
}

// telegramRangeBuffer is one fetched chunk together with the read cursor into
// it.
type telegramRangeBuffer struct {
	// buf holds the chunk payload.
	buf []byte
	// off is the number of bytes already handed to the consumer.
	off int
}

// telegramReadPlan is one upload.getFile request derived from a byte range.
type telegramReadPlan struct {
	// offset is the request offset, aligned down to telegramReadAlign and kept
	// inside a single telegramReadChunk window.
	offset int64
	// limit is the aligned request length sent to Telegram, which covers skip
	// plus length.
	limit int
	// skip is the number of leading response bytes that belong to earlier data
	// and must be dropped.
	skip int
	// length is the number of payload bytes the caller wants from this request.
	length int
}

// newTelegramRangeReader returns a reader over the range served on ctx, using a
// buffer channel of the given size and at most parallel fetches in flight.
// Non-positive buffer or parallel counts select the package defaults, and the
// retry policy starts at defaultTelegramReadTimeout and
// defaultTelegramReadAttempts. The caller keeps ownership of cancel and must
// invoke it, directly or through Close, to release the fetches.
func newTelegramRangeReader(ctx context.Context, cancel context.CancelFunc, buffers, parallel int) *telegramRangeReader {
	if buffers <= 0 {
		buffers = defaultTelegramReadBuffers
	}
	if parallel <= 0 {
		parallel = defaultTelegramReadParallel
	}
	return &telegramRangeReader{
		ctx: ctx, cancel: cancel, parallel: parallel,
		timeout: defaultTelegramReadTimeout, attempts: defaultTelegramReadAttempts,
		buffers: make(chan *telegramRangeBuffer, buffers), done: make(chan struct{}),
	}
}

// Read returns bytes from the current chunk, pulling the next chunk when the
// current one is drained; it copies at most one chunk per call. Once the
// buffers are drained it returns the terminal fill error, except that a fill
// stopped by the stream context is reported as io.EOF at the end of the range.
// A zero length read returns (0, nil) without waiting, and ctx.Err() is
// returned when the stream context ends while waiting for a chunk.
func (r *telegramRangeReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.cur == nil || r.cur.empty() {
		select {
		case buf, ok := <-r.buffers:
			if !ok {
				r.mu.Lock()
				err := r.readErr
				r.mu.Unlock()
				if err != nil && !errors.Is(err, context.Canceled) {
					return 0, err
				}
				return 0, io.EOF
			}
			r.cur = buf
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
	n := copy(p, r.cur.remaining())
	r.cur.off += n
	return n, nil
}

// Close cancels the stream and waits until the fill goroutine finished or the
// stream context is done, whichever comes first. It is idempotent and always
// returns nil, so a reader can be closed on every error path.
func (r *telegramRangeReader) Close() error {
	r.closeOnce.Do(func() { r.cancel() })
	select {
	case <-r.done:
	case <-r.ctx.Done():
	}
	return nil
}

// finish records the terminal error of the stream, closes the buffer channel
// so Read can drain what was already prefetched, and closes done. It runs at
// most once; later calls are ignored.
func (r *telegramRangeReader) finish(err error) {
	r.finishOnce.Do(func() {
		r.mu.Lock()
		r.readErr = err
		r.mu.Unlock()
		close(r.buffers)
		close(r.done)
	})
}

// fill fetches the requested range in aligned chunks on up to parallel
// goroutines and pushes them into the reader's buffer channel in range order,
// so a slow consumer waits instead of accumulating unbounded read-ahead. Each
// chunk is attempted up to attempts times with a per-attempt timeout, and a
// download rejected with FILE_REFERENCE_EXPIRED triggers one shared refresh
// before that chunk is retried. It returns the first terminal error, from the
// stream context or from a chunk that ran out of attempts, and reports the
// outcome through finish rather than closing the reader itself.
func (r *telegramRangeReader) fill(ctx context.Context, api *tg.Client, location *tg.InputDocumentFileLocation, offset, remaining int64, refresh func(context.Context) (*tg.InputDocumentFileLocation, error)) error {
	type readResult struct {
		seq     int64
		payload []byte
		err     error
	}

	fetchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	completed := make(chan readResult, r.parallel)
	currentLocation := location
	var locationMu sync.RWMutex
	var refreshMu sync.Mutex

	nextOffset, nextRemaining := offset, remaining
	var nextSeq int64
	var emitSeq int64
	active := 0
	launchNext := func() bool {
		if nextRemaining <= 0 || nextSeq-emitSeq >= int64(r.parallel) {
			return false
		}
		plan := planTelegramReads(nextOffset, nextRemaining, 1)[0]
		seq := nextSeq
		nextSeq++
		nextOffset += int64(plan.length)
		nextRemaining -= int64(plan.length)
		active++
		go func() {
			result := readResult{seq: seq}
			locationMu.RLock()
			usedLocation := currentLocation
			locationMu.RUnlock()

			download := func(callCtx context.Context, loc *tg.InputDocumentFileLocation) (tg.UploadFileClass, error) {
				return api.UploadGetFile(callCtx, &tg.UploadGetFileRequest{
					Location: loc,
					Offset:   plan.offset,
					Limit:    plan.limit,
					Precise:  true,
				})
			}

			var response tg.UploadFileClass
			var err error
			for range r.attempts {
				attemptCtx, attemptCancel := context.WithTimeout(fetchCtx, r.timeout)
				response, err = download(attemptCtx, usedLocation)
				if _, expired := tgerr.AsType(err, "FILE_REFERENCE_EXPIRED"); expired && refresh != nil {
					refreshMu.Lock()
					locationMu.RLock()
					latestLocation := currentLocation
					locationMu.RUnlock()
					if latestLocation == usedLocation {
						refreshed, refreshErr := refresh(attemptCtx)
						if refreshErr != nil {
							err = refreshErr
						} else {
							locationMu.Lock()
							currentLocation = refreshed
							locationMu.Unlock()
							latestLocation = refreshed
						}
					}
					refreshMu.Unlock()
					if latestLocation != nil && latestLocation != usedLocation {
						usedLocation = latestLocation
						response, err = download(attemptCtx, latestLocation)
					}
				}
				attemptCancel()
				if err == nil {
					break
				}
				if fetchCtx.Err() != nil {
					err = fetchCtx.Err()
					break
				}
				if !errors.Is(err, context.DeadlineExceeded) && !isTransientTelegramError(err) {
					break
				}
			}

			if err != nil {
				result.err = fmt.Errorf("download Telegram document chunk at %d: %w", plan.offset, err)
			} else if file, ok := response.(*tg.UploadFile); !ok {
				result.err = fmt.Errorf("unexpected Telegram download response %T", response)
			} else if end := plan.skip + plan.length; len(file.Bytes) < end {
				result.err = io.ErrUnexpectedEOF
			} else {
				result.payload = file.Bytes[plan.skip:end]
			}
			select {
			case completed <- result:
			case <-fetchCtx.Done():
			}
		}()
		return true
	}

	for active < r.parallel && launchNext() {
	}

	ready := make(map[int64][]byte, r.parallel)
	for active > 0 {
		var result readResult
		select {
		case result = <-completed:
			active--
		case <-ctx.Done():
			return ctx.Err()
		}
		if result.err != nil {
			return result.err
		}
		ready[result.seq] = result.payload

		for {
			payload, ok := ready[emitSeq]
			if !ok {
				break
			}
			select {
			case r.buffers <- &telegramRangeBuffer{buf: payload}:
				delete(ready, emitSeq)
				emitSeq++
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		for active < r.parallel && launchNext() {
		}
	}
	return nil
}

// planTelegramReads splits a byte range into at most count upload.getFile
// requests that each stay inside one telegramReadChunk window. Every plan aligns
// the request offset down to telegramReadAlign, keeps the request limit aligned
// as well, and records how many leading response bytes skip must be dropped, so
// the caller receives exactly the requested bytes while Telegram sees aligned
// block reads.
func planTelegramReads(offset, remaining int64, count int) []telegramReadPlan {
	plans := make([]telegramReadPlan, 0, count)
	for remaining > 0 && len(plans) < count {
		requestOffset := offset / telegramReadAlign * telegramReadAlign
		skip := int(offset - requestOffset)
		boundaryRemaining := telegramReadChunk - int(requestOffset%telegramReadChunk)
		maxLimit := telegramReadAlign
		for maxLimit*2 <= boundaryRemaining {
			maxLimit *= 2
		}
		length := int(min(remaining, int64(maxLimit-skip)))
		limit := telegramReadAlign
		for limit < skip+length {
			limit *= 2
		}
		plans = append(plans, telegramReadPlan{
			offset: requestOffset, limit: limit, skip: skip, length: length,
		})
		offset += int64(length)
		remaining -= int64(length)
	}
	return plans
}

// empty reports whether the buffer has no unread byte. It is safe on a nil
// buffer, so a consumer can test its current chunk before using it.
func (b *telegramRangeBuffer) empty() bool {
	return b == nil || len(b.buf)-b.off <= 0
}

// remaining returns the unread part of the buffer as a slice into the stored
// payload. It must not be called on a nil or fully drained buffer.
func (b *telegramRangeBuffer) remaining() []byte {
	return b.buf[b.off:]
}

// CopyPart republishes an existing document into another channel by reference,
// so no bytes are transferred, and returns the new part. It runs as a
// management operation and checks the copied size against the source, reporting
// ErrSizeMismatch when they differ; the copy is already published at that point,
// so it is returned together with the error, which is what lets the caller
// delete it instead of leaving it to the orphan cleanup sweep. Every call
// creates a new message, so it is not idempotent. A nil runner, non-positive
// user ID, zero channel ID, or non-positive source message ID returns
// ErrInvalidRequest along with a zero part.
func (s *GotdStorage) CopyPart(ctx context.Context, userID, sourceChannelID, sourceMessageID, destinationChannelID int64) (StoredPart, error) {
	if s.runner == nil || userID <= 0 || sourceChannelID == 0 || sourceMessageID <= 0 || destinationChannelID == 0 {
		return StoredPart{}, ErrInvalidRequest
	}
	var copied StoredPart
	err := s.runner.Run(ctx, userID, OperationManage, func(runCtx context.Context, api *tg.Client) error {
		location, size, err := documentLocation(runCtx, api, sourceChannelID, sourceMessageID)
		if err != nil {
			return err
		}
		destination, err := inputChannel(runCtx, api, destinationChannelID)
		if err != nil {
			return err
		}
		var randomBytes [8]byte
		if _, err := cryptorand.Read(randomBytes[:]); err != nil {
			return fmt.Errorf("generate Telegram copy random id: %w", err)
		}
		response, err := api.MessagesSendMedia(runCtx, &tg.MessagesSendMediaRequest{
			Silent:   true,
			Peer:     &tg.InputPeerChannel{ChannelID: destination.ChannelID, AccessHash: destination.AccessHash},
			Media:    &tg.InputMediaDocument{ID: &tg.InputDocument{ID: location.ID, AccessHash: location.AccessHash, FileReference: location.FileReference}},
			RandomID: int64(binary.BigEndian.Uint64(randomBytes[:])),
		})
		if err != nil {
			return fmt.Errorf("copy Telegram document: %w", err)
		}
		messageID, copiedSize, err := uploadedMessage(response)
		if err != nil {
			return err
		}
		if copiedSize != size {
			// Record the published message before failing: the caller can only
			// compensate for a document it can name.
			copied = StoredPart{ChannelID: destinationChannelID, MessageID: messageID, Size: copiedSize}
			return fmt.Errorf("%w: copied %d, source %d", ErrSizeMismatch, copiedSize, size)
		}
		copied = StoredPart{ChannelID: destinationChannelID, MessageID: messageID, Size: copiedSize}
		return nil
	})
	if err != nil {
		return copied, err
	}
	return copied, nil
}

// DeleteMessages deletes the given messages from a channel in batches of at most
// deleteBatchSize, on a management session. An empty slice is a no-op and a
// channel that no longer resolves counts as already deleted. Every message ID is
// validated before the first Telegram call, so an ID at or below zero returns
// ErrInvalidRequest without deleting anything. Telegram batches are still not
// transactional: a Telegram failure midway leaves earlier batches deleted. A nil
// runner, non-positive user ID, or zero channel ID returns ErrInvalidRequest
// before any Telegram call.
func (s *GotdStorage) DeleteMessages(ctx context.Context, userID, channelID int64, messageIDs []int64) error {
	if s.runner == nil || userID <= 0 || channelID == 0 {
		return ErrInvalidRequest
	}
	if len(messageIDs) == 0 {
		return nil
	}
	for _, id := range messageIDs {
		if id <= 0 {
			return ErrInvalidRequest
		}
	}
	return s.runner.Run(ctx, userID, OperationManage, func(runCtx context.Context, api *tg.Client) error {
		channel, err := inputChannel(runCtx, api, channelID)
		if errors.Is(err, ErrInvalidChannel) {
			return nil
		}
		if err != nil {
			return err
		}
		for start := 0; start < len(messageIDs); start += deleteBatchSize {
			end := min(start+deleteBatchSize, len(messageIDs))
			ids := make([]int, 0, end-start)
			for _, id := range messageIDs[start:end] {
				ids = append(ids, int(id))
			}
			if _, err := api.ChannelsDeleteMessages(runCtx, &tg.ChannelsDeleteMessagesRequest{Channel: channel, ID: ids}); err != nil {
				return fmt.Errorf("delete Telegram messages: %w", err)
			}
		}
		return nil
	})
}

// ListDocumentMessages returns one page of a channel's history, newest first,
// skipping entries without document media while still tracking the lowest
// message ID seen, so the caller can continue from page.BeforeID. Exhausted is
// inferred from a page shorter than the requested limit. It runs on a management
// session and returns ErrInvalidRequest for a nil runner, a non-positive user ID
// or limit, a limit above 100, a zero channel ID, or a negative cursor.
func (s *GotdStorage) ListDocumentMessages(ctx context.Context, request ListDocumentMessagesRequest) (DocumentMessagePage, error) {
	if s.runner == nil || request.UserID <= 0 || request.ChannelID == 0 || request.BeforeID < 0 || request.Limit <= 0 || request.Limit > 100 {
		return DocumentMessagePage{}, ErrInvalidRequest
	}
	var page DocumentMessagePage
	err := s.runner.Run(ctx, request.UserID, OperationManage, func(runCtx context.Context, api *tg.Client) error {
		channel, err := inputChannel(runCtx, api, request.ChannelID)
		if err != nil {
			return err
		}
		result, err := api.MessagesGetHistory(runCtx, &tg.MessagesGetHistoryRequest{
			Peer:     &tg.InputPeerChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash},
			OffsetID: int(request.BeforeID), Limit: request.Limit,
		})
		if err != nil {
			return fmt.Errorf("list Telegram channel history: %w", err)
		}
		modified, ok := result.AsModified()
		if !ok {
			return errors.New("list Telegram channel history: unexpected unmodified response")
		}
		messages := modified.GetMessages()
		page.Exhausted = len(messages) < request.Limit
		for _, item := range messages {
			// The cursor advances over every entry, not only the ones carrying a
			// document: a page made up entirely of service messages would
			// otherwise report no position at all, and a cleanup caller that
			// checks the cursor moved would treat the channel as stuck.
			switch entry := item.(type) {
			case *tg.Message:
				if page.BeforeID == 0 || int64(entry.ID) < page.BeforeID {
					page.BeforeID = int64(entry.ID)
				}
				if _, ok := entry.Media.(*tg.MessageMediaDocument); ok {
					page.Messages = append(page.Messages, DocumentMessage{ID: int64(entry.ID), CreatedAt: time.Unix(int64(entry.Date), 0).UTC()})
				}
			case *tg.MessageService:
				if page.BeforeID == 0 || int64(entry.ID) < page.BeforeID {
					page.BeforeID = int64(entry.ID)
				}
			}
		}
		if len(messages) > 0 && page.BeforeID == 0 {
			return errors.New("list Telegram channel history: page has no usable message ID")
		}
		return nil
	})
	if err != nil {
		return DocumentMessagePage{}, err
	}
	return page, nil
}

// CreateChannel creates a broadcast channel titled with the trimmed name and
// returns it. When an upload bot provider is configured, its bots are granted
// channel admin rights before the channel is returned, and a bot failure deletes
// the freshly created channel before the error is passed on. It runs on a
// management session; a nil runner, non-positive user ID, or blank name returns
// ErrInvalidRequest, and a response that carries no channel is reported as
// ErrInvalidChannel.
func (s *GotdStorage) CreateChannel(ctx context.Context, userID int64, name string) (Channel, error) {
	if s.runner == nil || userID <= 0 || strings.TrimSpace(name) == "" {
		return Channel{}, ErrInvalidRequest
	}
	var created Channel
	err := s.runner.Run(ctx, userID, OperationManage, func(runCtx context.Context, api *tg.Client) error {
		response, err := api.ChannelsCreateChannel(runCtx, &tg.ChannelsCreateChannelRequest{
			Title:     strings.TrimSpace(name),
			Broadcast: true,
		})
		if err != nil {
			return fmt.Errorf("create Telegram channel: %w", err)
		}
		updates, ok := response.(*tg.Updates)
		if !ok {
			return fmt.Errorf("unexpected Telegram channel response %T", response)
		}
		for _, chat := range updates.Chats {
			channel, ok := chat.(*tg.Channel)
			if !ok {
				continue
			}
			if s.botProvider != nil {
				bots, botErr := s.botProvider.ChannelBots(runCtx, userID, api)
				if botErr != nil {
					_, _ = api.ChannelsDeleteChannel(runCtx, channel.AsInput())
					return fmt.Errorf("resolve Telegram upload bots: %w", botErr)
				}
				for _, bot := range bots {
					if adminErr := setBotAdmin(runCtx, api, channel.AsInput(), bot); adminErr != nil {
						_, _ = api.ChannelsDeleteChannel(runCtx, channel.AsInput())
						return fmt.Errorf("add Telegram upload bot as channel admin: %w", adminErr)
					}
				}
			}
			created = Channel{ID: channel.ID, Name: channel.Title}
			return nil
		}
		return ErrInvalidChannel
	})
	if err != nil {
		return Channel{}, err
	}
	return created, nil
}

// DeleteChannel deletes a channel together with every message it holds on a
// management session. A channel that no longer resolves is treated as already
// deleted and returns nil, which keeps the operation idempotent for callers
// that retry it; other failures are wrapped Telegram errors.
func (s *GotdStorage) DeleteChannel(ctx context.Context, userID, channelID int64) error {
	if s.runner == nil || userID <= 0 || channelID == 0 {
		return ErrInvalidRequest
	}
	return s.runner.Run(ctx, userID, OperationManage, func(runCtx context.Context, api *tg.Client) error {
		channel, err := fullChannel(runCtx, api, channelID)
		if errors.Is(err, ErrInvalidChannel) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := api.ChannelsDeleteChannel(runCtx, channel.AsInput()); err != nil {
			return fmt.Errorf("delete Telegram channel: %w", err)
		}
		return nil
	})
}

// InviteBot resolves username to a bot and promotes it to channel admin on the
// user's own management session. The username is trimmed and may carry a leading
// "@"; only a resolved user that is a bot and whose username matches case
// insensitively is accepted, and anything else returns ErrInvalidRequest
// (wrapped). Progress is logged at info level. A nil runner, non-positive user
// ID, zero channel ID, or blank username returns ErrInvalidRequest, and a
// channel that no longer resolves fails instead of succeeding silently.
func (s *GotdStorage) InviteBot(ctx context.Context, userID, channelID int64, username string) error {
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	if s.runner == nil || userID <= 0 || channelID == 0 || username == "" {
		return ErrInvalidRequest
	}
	return s.runner.Run(ctx, userID, OperationManage, func(runCtx context.Context, api *tg.Client) error {
		slog.InfoContext(runCtx, "Resolving Telegram channel for bot provisioning",
			"user_id", userID,
			"channel_id", channelID,
			"bot_username", username,
		)
		channel, err := fullChannel(runCtx, api, channelID)
		if err != nil {
			return err
		}
		resolved, err := api.ContactsResolveUsername(runCtx, &tg.ContactsResolveUsernameRequest{Username: username})
		if err != nil {
			return fmt.Errorf("resolve Telegram bot %s: %w", username, err)
		}
		var bot tg.InputUserClass
		var botID int64
		for _, item := range resolved.Users {
			user, ok := item.(*tg.User)
			if ok && user.Bot && strings.EqualFold(user.Username, username) {
				bot = user.AsInput()
				botID = user.ID
				break
			}
		}
		if bot == nil {
			return fmt.Errorf("resolve Telegram bot %s: %w", username, ErrInvalidRequest)
		}
		slog.InfoContext(runCtx, "Promoting Telegram bot to channel admin",
			"user_id", userID,
			"channel_id", channelID,
			"bot_id", botID,
			"bot_username", username,
		)
		if err := setBotAdmin(runCtx, api, channel.AsInput(), bot); err != nil {
			return fmt.Errorf("add Telegram bot %s as admin in channel %d: %w", username, channelID, err)
		}
		slog.InfoContext(runCtx, "Telegram bot promoted to channel admin",
			"user_id", userID,
			"channel_id", channelID,
			"bot_id", botID,
			"bot_username", username,
		)
		return nil
	})
}

// setBotAdmin grants bot a fixed message-administration rights set on channel,
// under the rank "bot". It returns ErrInvalidRequest for a nil API, channel, or
// bot and otherwise the raw Telegram error, letting callers decide whether to
// roll back the surrounding operation.
func setBotAdmin(ctx context.Context, api *tg.Client, channel tg.InputChannelClass, bot tg.InputUserClass) error {
	if api == nil || channel == nil || bot == nil {
		return ErrInvalidRequest
	}

	required := tg.ChatAdminRights{
		ChangeInfo:     true,
		PostMessages:   true,
		EditMessages:   true,
		DeleteMessages: true,
		BanUsers:       true,
		InviteUsers:    true,
		PinMessages:    true,
		ManageCall:     true,
		Other:          true,
		ManageTopics:   true,
	}

	_, err := api.ChannelsEditAdmin(ctx, &tg.ChannelsEditAdminRequest{
		Channel:     channel,
		UserID:      bot,
		AdminRights: required,
		Rank:        "bot",
	})
	return err
}

// inputChannel resolves a numeric channel ID through fullChannel and returns the
// input form Telegram expects, which carries the access hash later calls need.
// A channel that does not resolve is reported as ErrInvalidChannel.
func inputChannel(ctx context.Context, api *tg.Client, channelID int64) (*tg.InputChannel, error) {
	channel, err := fullChannel(ctx, api, channelID)
	if err != nil {
		return nil, err
	}
	return channel.AsInput(), nil
}

// fullChannel loads the channel with the given ID and returns the full Telegram
// channel, which carries the access hash and title. An empty chat list is
// reported as ErrInvalidChannel, another chat type as an unexpected-response
// error, and a transport failure as a wrapped resolve error.
func fullChannel(ctx context.Context, api *tg.Client, channelID int64) (*tg.Channel, error) {
	response, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: channelID}})
	if err != nil {
		return nil, fmt.Errorf("resolve Telegram channel: %w", err)
	}
	chats := response.GetChats()
	if len(chats) == 0 {
		return nil, ErrInvalidChannel
	}
	channel, ok := chats[0].(*tg.Channel)
	if !ok {
		return nil, fmt.Errorf("unexpected Telegram chat %T", chats[0])
	}
	return channel, nil
}

// uploadedMessage extracts the message ID and the stored document size from the
// update Telegram returns after a document is published. It returns
// ErrDocumentNotFound when the new channel message carries no document, for
// example when non-file media was published, and ErrMessageNotFound when the
// response contains no channel message at all.
func uploadedMessage(response tg.UpdatesClass) (int64, int64, error) {
	updates, ok := response.(*tg.Updates)
	if !ok {
		return 0, 0, fmt.Errorf("unexpected Telegram upload response %T", response)
	}
	for _, update := range updates.Updates {
		channelMessage, ok := update.(*tg.UpdateNewChannelMessage)
		if !ok {
			continue
		}
		msg, ok := channelMessage.Message.(*tg.Message)
		if !ok {
			continue
		}
		document, ok := messageDocument(msg)
		if !ok {
			return 0, 0, ErrDocumentNotFound
		}
		return int64(msg.ID), document.Size, nil
	}
	return 0, 0, ErrMessageNotFound
}

// documentLocation resolves one document message to its file location and size
// without caching. It returns ErrInvalidChannel when the channel cannot be
// resolved, ErrMessageNotFound when the message does not exist or is not a plain
// message, and ErrDocumentNotFound when it carries no document media.
func documentLocation(ctx context.Context, api *tg.Client, channelID, messageID int64) (*tg.InputDocumentFileLocation, int64, error) {
	channel, err := inputChannel(ctx, api, channelID)
	if err != nil {
		return nil, 0, err
	}
	response, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: channel,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: int(messageID)}},
	})
	if err != nil {
		return nil, 0, fmt.Errorf("get Telegram document message: %w", err)
	}
	modified, ok := response.AsModified()
	if !ok || len(modified.GetMessages()) == 0 {
		return nil, 0, ErrMessageNotFound
	}
	msg, ok := modified.GetMessages()[0].(*tg.Message)
	if !ok {
		return nil, 0, ErrMessageNotFound
	}
	document, ok := messageDocument(msg)
	if !ok {
		return nil, 0, ErrDocumentNotFound
	}
	return document.AsInputDocumentFileLocation(""), document.Size, nil
}

// messageDocument returns the document carried by a message, reporting false
// when the message has no media or its media is not a document, such as a photo,
// poll, or service message.
func messageDocument(msg *tg.Message) (*tg.Document, bool) {
	media, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok {
		return nil, false
	}
	document, ok := media.Document.(*tg.Document)
	return document, ok
}
