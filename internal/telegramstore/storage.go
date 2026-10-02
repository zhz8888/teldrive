package telegramstore

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	// ErrInvalidRequest reports arguments that must not be sent to Telegram,
	// such as a non-positive user ID, a blank channel name, or a negative size.
	ErrInvalidRequest = errors.New("invalid Telegram storage request")
	// ErrInvalidChannel reports a channel Telegram could not resolve or a
	// create call that returned no channel. DeleteMessages and DeleteChannel
	// treat it as "already gone" and report success instead of propagating it.
	ErrInvalidChannel = errors.New("invalid Telegram channel")
	// ErrMessageNotFound reports a message that does not exist in the channel
	// or a Telegram response that carries no channel message at all.
	ErrMessageNotFound = errors.New("Telegram message not found")
	// ErrDocumentNotFound reports a message that exists but holds no document
	// media, for example a text or photo message or a replaced document.
	ErrDocumentNotFound = errors.New("Telegram document not found")
	// ErrSizeMismatch reports a stored document whose size differs from the
	// requested (Upload) or source (CopyPart) size. The document is already
	// published at that point, and the failed call returns no part, so the
	// caller cannot delete it and must leave it to the orphan cleanup sweep.
	ErrSizeMismatch = errors.New("Telegram stored size mismatch")
)

// Operation declares which kind of Telegram session a storage call runs on.
// The runner prefers a bot session for uploads and downloads when the bot
// gateway is configured and always uses the user's own session for management.
type Operation string

const (
	// OperationUpload stores new document bytes, through an upload-eligible bot
	// session when one exists and through the user session otherwise.
	OperationUpload Operation = "upload"
	// OperationDownload reads stored document bytes or metadata, through a
	// download bot session when one is configured and the user session
	// otherwise.
	OperationDownload Operation = "download"
	// OperationManage covers channel and message administration. It requires a
	// real user account: ClientRunner rejects bot sessions for it.
	OperationManage Operation = "manage"
)

// UploadRequest describes one document upload. UserID, ChannelID, Name, and
// Reader are mandatory, so the zero value is rejected with ErrInvalidRequest.
type UploadRequest struct {
	// UserID is the TelDrive user whose Telegram session performs the upload
	// and must be positive.
	UserID int64
	// ChannelID is the destination storage channel and must be non-zero.
	ChannelID int64
	// Name is the file name attached to the Telegram document. It must not be
	// blank after trimming and is stored verbatim otherwise.
	Name string
	// Reader supplies the document bytes. It must yield exactly Size bytes and
	// is never closed by the storage layer, so the caller keeps ownership.
	Reader io.Reader
	// Size is the exact document size in bytes. Zero uploads an empty file and
	// negative values are rejected.
	Size int64
	// Threads is the number of parallel upload connections. Values below one
	// select the default of four.
	Threads int
}

// StoredPart identifies one document that lives in a storage channel.
type StoredPart struct {
	// ChannelID is the channel holding the document.
	ChannelID int64
	// MessageID is the Telegram message ID of the document. A value above zero
	// means the part is stored; zero only appears in the result of a call that
	// reported an error.
	MessageID int64
	// Size is the document size in bytes as reported by Telegram, which is the
	// stored (possibly encrypted) size rather than the plaintext size.
	Size int64
}

// DocumentMessage is one channel message that carries a document.
type DocumentMessage struct {
	// ID is the Telegram message ID, used both as the history cursor and as
	// the identifier passed back to DeleteMessages.
	ID int64
	// CreatedAt is the message timestamp in UTC, used to age out candidates
	// before deletion.
	CreatedAt time.Time
}

// ListDocumentMessagesRequest asks for one page of a channel's history.
type ListDocumentMessagesRequest struct {
	// UserID is the TelDrive user whose session reads the history and must be
	// positive.
	UserID int64
	// ChannelID is the channel to scan and must be non-zero.
	ChannelID int64
	// BeforeID selects messages with an ID strictly lower than this value. Zero
	// starts from the newest message.
	BeforeID int64
	// Limit is the maximum number of history entries to request and must be
	// between 1 and 100, the range Telegram accepts.
	Limit int
}

// DocumentMessagePage is one page of channel history. Callers advance by
// passing BeforeID into the next request and stop once Exhausted is set.
type DocumentMessagePage struct {
	// Messages holds the document messages of the page, newest first. History
	// entries without document media are skipped, so the count can be lower
	// than the requested limit even on a full page.
	Messages []DocumentMessage
	// BeforeID is the lowest message ID seen in the page, including entries
	// without documents, or zero when the page was empty. Pass it as the
	// BeforeID of the next request.
	BeforeID int64
	// Exhausted reports that the channel history ended, which the
	// implementation infers from a page shorter than the requested limit.
	Exhausted bool
}

// DocumentMessageLister is an optional maintenance capability implemented by
// production storage without expanding the core upload/download boundary.
type DocumentMessageLister interface {
	// ListDocumentMessages returns one page of the channel's document
	// messages. Limit must be between 1 and 100 and BeforeID must not be
	// negative; anything else returns ErrInvalidRequest. Each call is
	// independent and opens its own short-lived Telegram client, so pages may
	// be fetched concurrently, but a page cannot be resumed halfway through:
	// callers continue from DocumentMessagePage.BeforeID and stop when
	// Exhausted is true.
	ListDocumentMessages(context.Context, ListDocumentMessagesRequest) (DocumentMessagePage, error)
}

// MetadataRequest addresses one stored document without opening its body.
type MetadataRequest struct {
	// UserID is the TelDrive user whose session resolves the document and must
	// be positive.
	UserID int64
	// ChannelID is the channel holding the document and must be non-zero.
	ChannelID int64
	// MessageID is the Telegram message ID of the document. A negative value
	// cannot name a message and is rejected with ErrInvalidRequest, while zero
	// is looked up and surfaces as a document lookup error.
	MessageID int64
}

// MetadataReader resolves Telegram document metadata without opening its body.
// Production storage implements it; legacy-size resolution uses it lazily.
type MetadataReader interface {
	// Metadata returns the identity and stored size of a document without
	// transferring its bytes. Production serves repeated lookups from a shared
	// four hour location cache. It returns ErrInvalidRequest for a malformed
	// request, ErrInvalidChannel when the channel cannot be resolved,
	// ErrMessageNotFound for an unknown message, and ErrDocumentNotFound when
	// the message carries no document. Implementations must be safe for
	// concurrent use.
	Metadata(ctx context.Context, request MetadataRequest) (StoredPart, error)
}

// RangeRequest selects a byte range of one stored document. Offset and Length
// address the stored bytes, so encrypted parts must be decrypted by the caller.
type RangeRequest struct {
	// UserID is the TelDrive user whose session reads the document and must be
	// positive.
	UserID int64
	// ChannelID is the channel holding the document and must be non-zero.
	ChannelID int64
	// MessageID is the Telegram message ID of the document and must be
	// positive.
	MessageID int64
	// Offset is the absolute, zero based byte offset to start reading from.
	Offset int64
	Length int64 // -1 reads through the end of the document.
}

// DownloadSession reuses one authenticated Telegram client for all metadata and
// range operations belonging to a single caller request.
type DownloadSession interface {
	// Metadata resolves a document's stored size without opening its body,
	// reusing the session's client. It validates the request like
	// MetadataReader.Metadata, so a malformed one returns ErrInvalidRequest, and
	// reports the remaining document lookup errors the same way. It must be
	// called while the session is still open: a closed session, pooled or
	// private, answers ErrClientUnavailable.
	Metadata(context.Context, MetadataRequest) (StoredPart, error)
	// OpenRange opens a byte range of a document. It validates the request like
	// Storage.OpenRange, so a malformed one returns ErrInvalidRequest before the
	// client is used. The returned reader belongs to the caller and must be
	// closed, which cancels the background fetch; reads end with io.EOF at the
	// end of the range and surface the underlying Telegram error otherwise.
	OpenRange(context.Context, RangeRequest) (io.ReadCloser, error)
	// Close releases the client lease held by the session. It is idempotent and
	// must be called when the request ends, after every reader returned by
	// OpenRange has been closed.
	Close() error
}

// DownloadSessionOpener is implemented by production storage. Storage fakes and
// local implementations can continue using the base methods through an adapter.
type DownloadSessionOpener interface {
	// OpenDownloadSession returns a session bound to the user's Telegram
	// account. It blocks until the client is authenticated: it returns the
	// context error if ctx ends first, ErrClientUnavailable when the client
	// stops before becoming usable, and ErrDownloadClientPoolClosed when the
	// underlying pool has already been shut down. The caller owns the returned
	// session and must close it.
	OpenDownloadSession(context.Context, int64) (DownloadSession, error)
}

// Channel is a storage channel as known to Telegram.
type Channel struct {
	// ID is the Telegram channel ID recorded with every file part.
	ID int64
	// Name is the channel title. CreateChannel returns the trimmed name that
	// was requested.
	Name string
}

// Storage is the Telegram object boundary used by upload, download, cleanup,
// and rollover services. Tests use deterministic in-memory implementations;
// production uses GotdStorage.
type Storage interface {
	// Upload publishes Reader as one document message in ChannelID and returns
	// the resulting part. It returns ErrInvalidRequest for a malformed request,
	// ErrSizeMismatch when Telegram stored a different size (the document is
	// already published, so this call cannot clean it up), and
	// ErrMessageNotFound when the publish response carries no channel message.
	// The reader is not closed. Every call creates a new message, so retrying a
	// failed upload can produce a second document.
	Upload(ctx context.Context, request UploadRequest) (StoredPart, error)
	// OpenRange streams Length bytes of a stored document starting at Offset.
	// The returned reader belongs to the caller and must be closed to cancel
	// the background download; reads end with io.EOF, and an Offset beyond the
	// document size yields a reader that reports io.EOF immediately.
	OpenRange(ctx context.Context, request RangeRequest) (io.ReadCloser, error)
	// DeleteMessages removes the given messages from a channel, splitting the
	// work into batches of at most 100. An empty slice is a no-op, a channel
	// that no longer resolves is treated as already deleted, and an ID at or
	// below zero returns ErrInvalidRequest before anything is deleted. Telegram
	// batch boundaries are not transactional: a failure midway leaves earlier
	// batches deleted.
	DeleteMessages(ctx context.Context, userID, channelID int64, messageIDs []int64) error
	// CopyPart republishes an existing document into another channel without
	// re-uploading its bytes and returns the new part. Every call creates a new
	// message, so it is not idempotent, and a copied size that differs from the
	// source reports ErrSizeMismatch after the message was published.
	CopyPart(ctx context.Context, userID, sourceChannelID, sourceMessageID, destinationChannelID int64) (StoredPart, error)
	// CreateChannel creates a broadcast channel titled with the trimmed name.
	// When an upload bot provider is configured, its bots are granted channel
	// admin rights before the channel is returned; if that fails, the freshly
	// created channel is deleted and the error is reported.
	CreateChannel(ctx context.Context, userID int64, name string) (Channel, error)
	// DeleteChannel deletes a channel together with every message it holds. A
	// channel that no longer resolves is treated as already deleted and returns
	// nil.
	DeleteChannel(ctx context.Context, userID, channelID int64) error
}

// BotInviter is an optional Telegram administration capability. Production
// GotdStorage implements it; deterministic content-storage fakes do not need to.
type BotInviter interface {
	// InviteBot promotes the bot named by username to channel admin on the
	// user's own session. The username is trimmed and may carry a leading "@";
	// matching against the resolved account is case insensitive. A username
	// that does not resolve to a bot returns ErrInvalidRequest (wrapped), and
	// the granted rights are a fixed message-administration set.
	InviteBot(ctx context.Context, userID, channelID int64, username string) error
}
