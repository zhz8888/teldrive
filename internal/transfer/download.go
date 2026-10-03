// Package transfer implements TelDrive's chunked upload and range download
// pipeline on top of the Telegram storage boundary.
//
// Pipeline stores one part of an open upload session per call: it claims a lease
// on the part, streams the body through the tree hasher and, for encrypted
// sessions, through the content cipher, publishes the result as a Telegram
// document, and either commits the part or compensates by deleting the message.
// Downloader opens an active file and returns a seekable reader that spans its
// ordered parts, decrypting and range-reading each part on demand.
//
// Neither type keeps per-request state, so one instance serves concurrent
// requests, but every injected boundary (catalog, channels, storage, keys, and
// the configured entropy source) must be safe for concurrent use itself.
package transfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/google/uuid"

	"github.com/tgdrive/teldrive/v2/internal/contentcrypto"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
)

var (
	// ErrInvalidDownload reports a malformed download request, or a file that is
	// not an active regular file with a known, non-negative size. Callers should
	// treat it as a client error rather than retrying it.
	ErrInvalidDownload = errors.New("invalid download request")
	// ErrRangeNotSatisfiable reports a requested byte range that falls outside
	// the file. The API layer maps it to HTTP 416.
	ErrRangeNotSatisfiable = errors.New("download range is not satisfiable")
	// ErrCorruptPartLayout reports file parts whose sizes, numbering, or channel
	// bindings disagree with the file's recorded size, so no byte range can be
	// mapped onto them. It indicates catalog corruption or tampering rather than
	// a bad request.
	ErrCorruptPartLayout = errors.New("file part layout is inconsistent")
	// ErrDownloadNotConfigured reports a Downloader created without a catalog or
	// storage boundary, or an adapter whose storage cannot resolve metadata. The
	// API layer maps it to HTTP 503.
	ErrDownloadNotConfigured = errors.New("download pipeline is not configured")
)

// FileCatalog is the finalized catalog boundary required by Downloader.
type FileCatalog interface {
	// Get returns the active file owned by userID. Implementations must report a
	// not-found error for a file that does not exist or belongs to another user,
	// and must be safe for concurrent use.
	Get(context.Context, int64, uuid.UUID) (*sqlcgen.File, error)
	// Parts returns the file's parts ordered by ascending part number, and
	// reports a not-a-file error for anything that is not an active regular file.
	// The caller mutates the returned rows in place while resolving legacy sizes,
	// so implementations must hand back rows the caller owns rather than shared
	// cached copies.
	Parts(context.Context, int64, uuid.UUID) ([]*sqlcgen.FilePart, error)
}

// PartSizeBackfiller is an optional FileCatalog capability that persists part
// sizes resolved from Telegram so later downloads do not look them up again.
type PartSizeBackfiller interface {
	// UpdatePartSizes records the plaintext and stored size of one part, both in
	// bytes. Part numbers are 1-based.
	UpdatePartSizes(context.Context, uuid.UUID, int32, int64, int64) error
}

// PartSizeBatchBackfiller is the batched variant of PartSizeBackfiller.
// Downloader prefers it when both are implemented, so one download writes every
// resolved size in a single statement.
type PartSizeBatchBackfiller interface {
	// UpdatePartSizesMany records the sizes of several parts at once. Keys are
	// 1-based part numbers and values hold the plaintext size followed by the
	// stored size, both in bytes.
	UpdatePartSizesMany(context.Context, uuid.UUID, map[int32][2]int64) error
}

// Downloader opens active files for reading. It holds no per-request state, so
// one instance serves concurrent requests.
type Downloader struct {
	// catalog resolves the file and its parts; it must be safe for concurrent use.
	catalog FileCatalog
	// storage serves the byte ranges of every part.
	storage telegramstore.Storage
	// keys resolves decryption keys by version. A nil provider only breaks
	// encrypted files, which fail with ErrEncryptionKey; unencrypted files still
	// download.
	keys KeyProvider
}

// NewDownloader returns a Downloader over the given boundaries. Any of them may
// be nil: Open then reports ErrDownloadNotConfigured when catalog or storage is
// missing, and encrypted files report ErrEncryptionKey when keys is missing.
func NewDownloader(catalog FileCatalog, storage telegramstore.Storage, keys KeyProvider) *Downloader {
	return &Downloader{catalog: catalog, storage: storage, keys: keys}
}

// DownloadRequest selects the byte range of one active file to read.
type DownloadRequest struct {
	// UserID is the TelDrive user that must own the file and must be positive.
	UserID int64
	// FileID identifies the file and must not be uuid.Nil.
	FileID uuid.UUID
	// Offset is the zero-based offset of the first byte to read and must not be
	// negative.
	Offset int64
	// Length is the number of bytes to read from Offset. A length that runs past
	// the end of the file is clamped to the bytes that remain, so the effective
	// length is reported by Download.Length.
	Length int64 // -1 means through end of file.
}

// Download is one resolved byte range of a file.
type Download struct {
	// Reader streams the range; it also supports random access and seeking, and
	// owns the storage session, so the caller must close it.
	Reader DownloadReader
	// File is the catalog row the range was planned from.
	File *sqlcgen.File
	// Offset is the zero-based offset the range starts at, as requested.
	Offset int64
	// Length is the effective number of bytes the range covers after clamping to
	// the end of the file; it is zero for an empty range and for an empty file.
	Length int64
	// TotalSize is the file's full plaintext size in bytes, independent of the
	// range.
	TotalSize int64
	// ContentType is the file's MIME type, or application/octet-stream when the
	// catalog has none.
	ContentType string
}

// DownloadReader is a random-access view of one resolved byte range: every
// offset is relative to the start of the range, not to the file.
//
// ReadAt only takes the internal mutex to observe Close, so concurrent ReadAt
// calls are safe and never move the sequential position. Read, Seek, and Close
// share that mutex and are meant to be driven from a single goroutine.
type DownloadReader interface {
	// Reader streams the range sequentially, opening and closing one storage
	// range per part as it advances, and reports io.EOF only once the whole range
	// was consumed.
	io.Reader
	// ReaderAt reads at an absolute offset within the range without moving the
	// sequential position, reporting io.EOF when the range ends before p is
	// filled.
	io.ReaderAt
	// Seeker moves the sequential position, discarding any open part reader.
	// Positions outside the range are clamped to its bounds rather than rejected.
	io.Seeker
	// Closer releases the open part reader and the storage session. It is
	// idempotent, and reads after it fail with io.ErrClosedPipe.
	io.Closer
}

// Open resolves the requested range of an active regular file. The returned
// Download owns a storage session that is released when its Reader is closed,
// and an empty range still returns a usable reader.
//
// It returns ErrInvalidDownload for a malformed request or a file that is not an
// active regular file with a known size, ErrRangeNotSatisfiable when the range
// starts past the end of the file, and ErrEncryptionKey when an encrypted file
// has no resolvable key.
func (d *Downloader) Open(ctx context.Context, request DownloadRequest) (*Download, error) {
	if d.catalog == nil || d.storage == nil {
		return nil, ErrDownloadNotConfigured
	}
	if request.UserID <= 0 || request.FileID == uuid.Nil || request.Offset < 0 || request.Length < -1 {
		return nil, ErrInvalidDownload
	}
	return d.openOrigin(ctx, request)
}

// openOrigin plans the range of an already validated request: it loads the file
// and its parts, resolves part sizes missing from legacy rows, maps the range
// onto per-part segments, and resolves the decryption key for encrypted files.
// The storage session opened here is handed to the returned reader, and it is
// closed before returning when anything fails.
func (d *Downloader) openOrigin(ctx context.Context, request DownloadRequest) (*Download, error) {
	file, err := d.catalog.Get(ctx, request.UserID, request.FileID)
	if err != nil {
		return nil, err
	}
	if file.Kind != sqlcgen.FileKindFile || file.Status != sqlcgen.FileStatusActive || !file.Size.Valid || file.Size.Int64 < 0 {
		return nil, ErrInvalidDownload
	}
	parts, err := d.catalog.Parts(ctx, request.UserID, request.FileID)
	if err != nil {
		return nil, err
	}
	session, err := d.openDownloadSession(ctx, request.UserID)
	if err != nil {
		return nil, err
	}
	keepSession := false
	defer func() {
		if !keepSession {
			_ = session.Close()
		}
	}()
	if err := d.resolveMissingPartSizes(ctx, session, request.UserID, request.FileID, file, parts); err != nil {
		return nil, err
	}
	segments, length, err := planSegments(parts, file.Size.Int64, request.Offset, request.Length)
	if err != nil {
		return nil, err
	}

	contentType := fileContentType(file)
	if length == 0 {
		return &Download{
			Reader: nopDownloadReader{bytes.NewReader(nil)}, File: file, Offset: request.Offset,
			Length: 0, TotalSize: file.Size.Int64, ContentType: contentType,
		}, nil
	}

	var encryptionKey string
	if file.Encryption {
		if d.keys == nil || !file.EncryptionKeyVersion.Valid {
			return nil, ErrEncryptionKey
		}
		encryptionKey, err = d.keys.Key(ctx, request.UserID, file.EncryptionKeyVersion.Int32)
		if err != nil || encryptionKey == "" {
			return nil, errors.Join(ErrEncryptionKey, err)
		}
	}

	keepSession = true
	return &Download{
		Reader: &downloadReader{
			ctx:          ctx,
			session:      session,
			sessionOwner: true,
			userID:       request.UserID,
			file:         file,
			parts:        segments,
			length:       length,
			key:          encryptionKey,
		}, File: file,
		Offset: request.Offset, Length: length, TotalSize: file.Size.Int64,
		ContentType: contentType,
	}, nil
}

// resolveMissingPartSizes fills in the plaintext and stored sizes of parts that
// predate those columns by reading the stored size from Telegram and deriving
// the plaintext size for encrypted files. It writes the values back through
// PartSizeBatchBackfiller when the catalog implements it, otherwise through
// PartSizeBackfiller one part at a time; a catalog with neither still gets
// correct sizes for the current download. Rows are updated in place, and the
// first lookup or persistence error aborts the download.
func (d *Downloader) resolveMissingPartSizes(ctx context.Context, session telegramstore.DownloadSession, userID int64, fileID uuid.UUID, file *sqlcgen.File, parts []*sqlcgen.FilePart) error {
	backfiller, _ := d.catalog.(PartSizeBackfiller)
	batchBackfiller, _ := d.catalog.(PartSizeBatchBackfiller)
	resolvedSizes := make(map[int32][2]int64)
	for _, part := range parts {
		if part == nil || (part.PlainSize.Valid && part.StoredSize.Valid) {
			continue
		}
		stored, err := session.Metadata(ctx, telegramstore.MetadataRequest{
			UserID: userID, ChannelID: part.ChannelID, MessageID: part.MessageID,
		})
		if err != nil {
			return fmt.Errorf("resolve Telegram part %d metadata: %w", part.PartNo, err)
		}
		plainSize := stored.Size
		if file.Encryption {
			plainSize, err = contentcrypto.DecryptedSize(stored.Size)
			if err != nil {
				return fmt.Errorf("derive plaintext size for part %d: %w", part.PartNo, err)
			}
		}
		part.StoredSize.Int64, part.StoredSize.Valid = stored.Size, true
		part.PlainSize.Int64, part.PlainSize.Valid = plainSize, true
		if batchBackfiller != nil {
			resolvedSizes[part.PartNo] = [2]int64{plainSize, stored.Size}
		} else if backfiller != nil {
			if err := backfiller.UpdatePartSizes(ctx, fileID, part.PartNo, plainSize, stored.Size); err != nil {
				return err
			}
		}
	}
	if batchBackfiller != nil && len(resolvedSizes) > 0 {
		if err := batchBackfiller.UpdatePartSizesMany(ctx, fileID, resolvedSizes); err != nil {
			return err
		}
	}
	return nil
}

// openDownloadSession returns the session used for every range read of one
// download. Production storage implements DownloadSessionOpener and returns a
// pooled, authenticated client; any other storage is wrapped in an adapter that
// forwards to the base methods, whose Close is a no-op.
func (d *Downloader) openDownloadSession(ctx context.Context, userID int64) (telegramstore.DownloadSession, error) {
	if opener, ok := d.storage.(telegramstore.DownloadSessionOpener); ok {
		return opener.OpenDownloadSession(ctx, userID)
	}
	return storageDownloadSession{storage: d.storage}, nil
}

// storageDownloadSession adapts a plain Storage to DownloadSession for storages
// that cannot pool clients. It holds no state between calls.
type storageDownloadSession struct{ storage telegramstore.Storage }

// Metadata resolves a document's stored size through the storage's optional
// MetadataReader; a storage without that capability reports
// ErrDownloadNotConfigured.
func (s storageDownloadSession) Metadata(ctx context.Context, request telegramstore.MetadataRequest) (telegramstore.StoredPart, error) {
	metadata, ok := s.storage.(telegramstore.MetadataReader)
	if !ok {
		return telegramstore.StoredPart{}, ErrDownloadNotConfigured
	}
	return metadata.Metadata(ctx, request)
}

// OpenRange opens a byte range of a stored document. The returned reader belongs
// to the caller and must be closed to cancel the background fetch.
func (s storageDownloadSession) OpenRange(ctx context.Context, request telegramstore.RangeRequest) (io.ReadCloser, error) {
	return s.storage.OpenRange(ctx, request)
}

// Close is a no-op because the adapter holds no client lease; each call goes
// straight to the storage.
func (storageDownloadSession) Close() error { return nil }

// downloadSegment is one contiguous span of a single file part, in plaintext
// coordinates.
type downloadSegment struct {
	// part is the catalog row the span is read from.
	part *sqlcgen.FilePart
	// offset is the span's zero-based start within the part's plaintext.
	offset int64
	// length is the span's size in plaintext bytes.
	length int64
}

// downloadReader serves one resolved range across its parts. The plan (file,
// parts, length, key, session) is immutable after construction; only the
// sequential position and the currently open part reader change.
type downloadReader struct {
	// ctx is the request context passed to every storage call; once it is
	// cancelled, opening further part ranges fails.
	ctx context.Context
	// session serves metadata and range reads for every part of this download.
	session telegramstore.DownloadSession
	// sessionOwner reports whether Close must close session (the Downloader
	// opened it) or leave it to the caller.
	sessionOwner bool
	// userID is the owner sent with every storage request.
	userID int64
	// file is the catalog row the range was planned from; it decides whether
	// parts are decrypted.
	file *sqlcgen.File
	// parts lists the segments the range covers, in file order.
	parts []downloadSegment
	// length is the range length in bytes; it bounds both the sequential
	// position and the offsets ReadAt accepts.
	length int64
	// key is the decryption key for an encrypted file, empty otherwise.
	key string

	// mu guards pos, closed, and reader.
	mu sync.Mutex
	// pos is the sequential read position within the range, in bytes.
	pos int64
	// closed reports that Close already ran; later reads fail with
	// io.ErrClosedPipe.
	closed bool
	// reader is the part range currently being streamed, or nil when the next
	// read must open one at pos.
	reader io.ReadCloser
	// readerEnd is the range position just past the last byte the open reader is
	// expected to yield; it is only meaningful while reader is non-nil.
	readerEnd int64

	// cipherMu guards ciphers, which is separate from mu because a reader is
	// opened while mu may already be held.
	cipherMu sync.Mutex
	// ciphers holds the content cipher of each part opened so far, keyed by the
	// part salt. Deriving a part key costs an scrypt pass, and a sequential
	// download opens one reader per part while a client using ReadAt opens one per
	// call, so without this the same key would be derived again and again.
	ciphers map[string]*contentcrypto.Cipher
}

// partCipher returns the content cipher of the part identified by salt, deriving
// its key at most once per download. It is safe for concurrent use, and the
// cipher it returns has no per-stream state.
func (r *downloadReader) partCipher(salt string) (*contentcrypto.Cipher, error) {
	r.cipherMu.Lock()
	defer r.cipherMu.Unlock()
	if cached, ok := r.ciphers[salt]; ok {
		return cached, nil
	}
	cipher, err := contentcrypto.NewCipher(r.key, salt)
	if err != nil {
		return nil, err
	}
	if r.ciphers == nil {
		r.ciphers = make(map[string]*contentcrypto.Cipher)
	}
	r.ciphers[salt] = cipher
	return cipher, nil
}

// Read streams the range sequentially from the current position, opening and
// draining one part range at a time. It reports io.EOF only when the whole range
// was consumed, so a failure in the middle surfaces the storage error after the
// bytes already read. A part range that ends before the range position the
// segment promised is such a failure: the reader is closed and the read reports
// io.ErrUnexpectedEOF instead of reopening the same offset, which would never
// make progress.
func (r *downloadReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	if len(p) == 0 {
		return 0, nil
	}
	read := 0
	for read < len(p) && r.pos < r.length {
		if r.reader == nil {
			segment, segmentStart, err := r.segmentAt(r.pos)
			if err != nil {
				return read, err
			}
			segmentOffset := r.pos - segmentStart
			r.reader, err = r.openPartReader(segment, segment.offset+segmentOffset, segment.length-segmentOffset)
			if err != nil {
				return read, err
			}
			r.readerEnd = segmentStart + segment.length
		}
		n, err := r.reader.Read(p[read:])
		read += n
		r.pos += int64(n)
		if errors.Is(err, io.EOF) {
			_ = r.reader.Close()
			r.reader = nil
			if r.readerEnd > r.pos {
				return read, fmt.Errorf("download reader: part range ended %d bytes early: %w", r.readerEnd-r.pos, io.ErrUnexpectedEOF)
			}
			continue
		}
		if err != nil {
			return read, err
		}
		if n == 0 {
			return read, io.ErrNoProgress
		}
	}
	if read == 0 {
		return 0, io.EOF
	}
	return read, nil
}

// ReadAt reads into p starting at off bytes into the range, without changing the
// sequential position, so it is safe alongside other ReadAt calls. It reports
// io.EOF when the range ends before p is filled and surfaces the storage error
// of a segment it could not read in full.
func (r *downloadReader) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return 0, io.ErrClosedPipe
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, fmt.Errorf("download reader: negative offset %d", off)
	}
	if off >= r.length {
		return 0, io.EOF
	}
	want := len(p)
	if off+int64(want) > r.length {
		want = int(r.length - off)
	}

	read := 0
	for read < want {
		segment, segmentStart, err := r.segmentAt(off + int64(read))
		if err != nil {
			return read, err
		}
		segmentOffset := off + int64(read) - segmentStart
		span := min(int64(want-read), segment.length-segmentOffset)
		n, err := r.readSegmentAt(p[read:read+int(span)], segment, segmentOffset)
		read += n
		if err != nil {
			return read, err
		}
		if n != int(span) {
			return read, io.ErrUnexpectedEOF
		}
	}
	if want < len(p) {
		return read, io.EOF
	}
	return read, nil
}

// Seek moves the sequential position to offset interpreted by whence, closing
// the open part range so the next read starts at the new position. Positions
// outside the range are clamped to its bounds, so it fails only for an unknown
// whence, and it returns the resulting absolute position.
func (r *downloadReader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.length + offset
	default:
		return r.pos, fmt.Errorf("download reader: invalid whence %d", whence)
	}
	if abs < 0 {
		abs = 0
	}
	if abs > r.length {
		abs = r.length
	}
	if r.reader != nil {
		_ = r.reader.Close()
		r.reader = nil
	}
	r.pos = abs
	return r.pos, nil
}

// Close releases the open part range and, when this reader owns it, the storage
// session. It is idempotent; reads after it fail with io.ErrClosedPipe.
func (r *downloadReader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	if r.reader != nil {
		_ = r.reader.Close()
		r.reader = nil
	}
	r.mu.Unlock()
	if r.sessionOwner {
		return r.session.Close()
	}
	return nil
}

// segmentAt returns the segment containing the range-relative offset off along
// with the offset at which that segment starts. It reports io.EOF when off lies
// at or past the end of the range.
func (r *downloadReader) segmentAt(off int64) (downloadSegment, int64, error) {
	var start int64
	for _, segment := range r.parts {
		end := start + segment.length
		if off < end {
			return segment, start, nil
		}
		start = end
	}
	return downloadSegment{}, 0, io.EOF
}

// readSegmentAt fills p with bytes read off bytes into segment through a
// dedicated range reader, which it closes before returning. That makes it usable
// from concurrent ReadAt calls; a segment shorter than the requested span
// reports io.ErrUnexpectedEOF.
func (r *downloadReader) readSegmentAt(p []byte, segment downloadSegment, off int64) (int, error) {
	span := int64(len(p))
	reader, err := r.openPartReader(segment, segment.offset+off, span)
	if err != nil {
		return 0, err
	}
	defer reader.Close()
	n, err := io.ReadFull(reader, p)
	if errors.Is(err, io.EOF) {
		// The segment promised this span but the reader ended before supplying a
		// single byte. Reporting plain EOF would tell the caller the range ended
		// normally, so it is normalized the same way the sequential path does.
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// openPartReader opens a reader for span bytes at partOffset within a segment's
// part, in plaintext coordinates. Encrypted parts are served by a seekable
// content cipher that reopens the underlying Telegram range and require a stored
// salt; a part without one reports ErrCorruptPartLayout. The returned reader
// belongs to the caller and must be closed, which releases the underlying range.
func (r *downloadReader) openPartReader(segment downloadSegment, partOffset, span int64) (io.ReadCloser, error) {
	var reader io.ReadCloser
	var err error
	if r.file.Encryption {
		if !segment.part.Salt.Valid || segment.part.Salt.String == "" {
			return nil, ErrCorruptPartLayout
		}
		cipher, cipherErr := r.partCipher(segment.part.Salt.String)
		if cipherErr != nil {
			return nil, fmt.Errorf("create part cipher: %w", cipherErr)
		}
		reader, err = cipher.DecryptDataSeek(r.ctx, func(openCtx context.Context, offset, limit int64) (io.ReadCloser, error) {
			return r.session.OpenRange(openCtx, telegramstore.RangeRequest{
				UserID: r.userID, ChannelID: segment.part.ChannelID, MessageID: segment.part.MessageID,
				Offset: offset, Length: limit,
			})
		}, partOffset, span)
	} else {
		reader, err = r.session.OpenRange(r.ctx, telegramstore.RangeRequest{
			UserID: r.userID, ChannelID: segment.part.ChannelID, MessageID: segment.part.MessageID,
			Offset: partOffset, Length: span,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("open Telegram part %d: %w", segment.part.PartNo, err)
	}
	return reader, nil
}

// planSegments maps the byte range [offset, offset+length) onto the file's parts
// and returns the segments plus the effective length, which normalizeDownloadRange
// may have clamped to the end of the file.
//
// Parts must be numbered 1..n in slice order, carry positive plaintext and
// stored sizes, and sum exactly to totalSize; anything else is
// ErrCorruptPartLayout, as is an empty file that still has parts. A zero-length
// result carries no segments and describes an empty file or a range that starts
// exactly at the end of the file, while the final ErrRangeNotSatisfiable branch
// is defensive and unreachable once the layout sums to totalSize.
func planSegments(parts []*sqlcgen.FilePart, totalSize, offset, requestedLength int64) ([]downloadSegment, int64, error) {
	length, err := normalizeDownloadRange(totalSize, offset, requestedLength)
	if err != nil {
		return nil, 0, err
	}
	if totalSize == 0 {
		if len(parts) != 0 || offset != 0 || length != 0 {
			return nil, 0, ErrCorruptPartLayout
		}
		return nil, 0, nil
	}

	var layoutSize int64
	for index, part := range parts {
		if part == nil || part.PartNo != int32(index+1) || !part.PlainSize.Valid || part.PlainSize.Int64 <= 0 || part.ChannelID == 0 || part.MessageID <= 0 || !part.StoredSize.Valid || part.StoredSize.Int64 <= 0 {
			return nil, 0, ErrCorruptPartLayout
		}
		layoutSize += part.PlainSize.Int64
	}
	if layoutSize != totalSize {
		return nil, 0, ErrCorruptPartLayout
	}
	if length == 0 {
		return nil, 0, nil
	}

	end := offset + length
	segments := make([]downloadSegment, 0)
	var partStart int64
	for _, part := range parts {
		partEnd := partStart + part.PlainSize.Int64
		if end <= partStart {
			break
		}
		if offset < partEnd && end > partStart {
			segmentStart := max(offset, partStart)
			segmentEnd := min(end, partEnd)
			segments = append(segments, downloadSegment{
				part: part, offset: segmentStart - partStart, length: segmentEnd - segmentStart,
			})
		}
		partStart = partEnd
	}
	if len(segments) == 0 {
		return nil, 0, ErrRangeNotSatisfiable
	}
	return segments, length, nil
}

// normalizeDownloadRange clamps a requested range to the file and returns the
// effective length: a length of -1, or one that runs past the end, is shortened
// to the bytes that remain. It returns ErrRangeNotSatisfiable for a negative
// size or offset, a length below -1, or an offset past the end of the file.
func normalizeDownloadRange(totalSize, offset, requestedLength int64) (int64, error) {
	if totalSize < 0 || offset < 0 || requestedLength < -1 || offset > totalSize {
		return 0, ErrRangeNotSatisfiable
	}
	length := requestedLength
	available := totalSize - offset
	if length == -1 || length > available {
		length = available
	}
	if length < 0 {
		return 0, ErrRangeNotSatisfiable
	}
	return length, nil
}

// fileContentType returns the file's MIME type, falling back to
// application/octet-stream when the catalog row carries none.
func fileContentType(file *sqlcgen.File) string {
	if file.MimeType.Valid && file.MimeType.String != "" {
		return file.MimeType.String
	}
	return "application/octet-stream"
}

// nopDownloadReader is the reader of a zero-length download: every read reports
// io.EOF and Close releases nothing because no session stands behind it.
type nopDownloadReader struct {
	// Reader is the empty source the zero-length download reads from.
	*bytes.Reader
}

// Close is a no-op; a zero-length download holds neither a session nor a part
// range.
func (nopDownloadReader) Close() error { return nil }
