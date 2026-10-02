package transfer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tgdrive/teldrive/v2/internal/contentcrypto"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
	"github.com/tgdrive/teldrive/v2/internal/treehash"
	"github.com/tgdrive/teldrive/v2/internal/uploads"
)

var (
	// ErrInvalidUpload reports a malformed part request, a checksum that is not
	// treehash.DigestSize bytes of hex, or a block-hash list whose length is not a
	// multiple of treehash.DigestSize. A request rejected up front leaves no lease
	// or Telegram message behind.
	ErrInvalidUpload = errors.New("invalid upload request")
	// ErrBodyTooShort reports a part body that ended before PlainSize bytes were
	// read. It is raised by exactReader and fails the part, which releases the
	// lease so the upload can be retried.
	ErrBodyTooShort = errors.New("upload body is shorter than Content-Length")
	// ErrBodyTooLong reports that the body still had bytes after PlainSize was
	// consumed. The published message is deleted and the part failed, so the
	// upload can be retried with a corrected length.
	ErrBodyTooLong = errors.New("upload body is longer than Content-Length")
	// ErrChecksumMismatch reports a computed tree hash that differs from the
	// caller-supplied checksum. The published message is deleted before the part
	// is failed.
	ErrChecksumMismatch = errors.New("plaintext part checksum mismatch")
	// ErrEncryptionKey reports that an encrypted session carries no key version,
	// or that KeyProvider could not resolve the requested one. Downloading an
	// encrypted file without a key provider reports it too. The part is failed
	// with the encryption_key_missing code.
	ErrEncryptionKey = errors.New("encryption key is unavailable")
	// ErrStoredSizeMismatch reports that Telegram did not return the channel,
	// message ID, and size that were uploaded. The message is deleted and the part
	// failed.
	ErrStoredSizeMismatch = errors.New("stored Telegram part size mismatch")
	// ErrUploadNotConfigured reports a Pipeline created without a catalog,
	// channel resolver, or storage boundary. The API layer maps it to HTTP 503.
	ErrUploadNotConfigured = errors.New("upload pipeline is not configured")
)

// partCleanupTimeout bounds the detached context used for catalog bookkeeping
// that must still happen after the request context is cancelled, such as failing
// a part or committing a finished one.
const partCleanupTimeout = 5 * time.Second

// defaultLeaseRenewInterval is how often an in-flight part renews its catalog
// lease when Config.LeaseRenewInterval is not set. It must stay well below the
// uploads service lease TTL so a slow part does not lose its lease.
const defaultLeaseRenewInterval = 20 * time.Second

// UploadCatalog is the durable upload-session boundary required by Pipeline.
type UploadCatalog interface {
	// Get returns the upload session owned by userID. It must report
	// uploads.ErrNotFound for an unknown session, and implementations must be
	// safe for concurrent use.
	Get(context.Context, int64, uuid.UUID) (*sqlcgen.UploadSession, error)
	// GetPart returns one part of a session. It reports uploads.ErrNotFound for a
	// part that was never claimed, and callers must treat any other error as fatal
	// rather than as "not stored yet".
	GetPart(context.Context, int64, uuid.UUID, int32) (*sqlcgen.UploadPart, error)
	// ClaimPart takes the lease for one upload attempt, creating the part row when
	// needed. A completed part with matching size and checksum is returned with
	// Existing set instead of being leased again; uploads.ErrPartBusy reports a
	// lease held by another attempt and uploads.ErrPartConflict a stored part that
	// disagrees with the request.
	ClaimPart(context.Context, uploads.ClaimPartInput) (*uploads.ClaimPartResult, error)
	// RenewPart extends the lease so a slow upload keeps ownership of its part. It
	// reports uploads.ErrLeaseLost once the token no longer matches the live lease.
	RenewPart(context.Context, uploads.RenewPartInput) error
	// StorePart marks the part stored with the Telegram message that holds it and
	// the sizes, checksum, salt, and block hashes computed for it. It reports
	// uploads.ErrLeaseLost when the lease expired or was taken over, in which case
	// the caller deletes the message it published.
	StorePart(context.Context, uploads.StorePartInput) (*sqlcgen.UploadPart, error)
	// FailPart records the failure code and releases the lease so the part can be
	// retried. It reports uploads.ErrLeaseLost when the token is already stale.
	FailPart(context.Context, uploads.FailPartInput) (*sqlcgen.UploadPart, error)
}

// ChannelResolver chooses an owned channel and performs rollover when needed.
type ChannelResolver interface {
	// Resolve returns the channel that should receive the part: the requested one
	// when it belongs to the user and is healthy and not full, otherwise a
	// selected or freshly created channel. A requestedChannelID of zero means
	// "choose for me". It reports the channels package's errors, such as
	// channels.ErrInvalidChannel or channels.ErrChannelFull, when a requested
	// channel cannot be used.
	Resolve(context.Context, int64, int64) (int64, error)
}

// KeyProvider resolves versioned server-managed content-encryption keys.
type KeyProvider interface {
	// Key returns the key material for one content-encryption key version so new
	// parts can be encrypted under it. A version that is not configured must
	// report ErrEncryptionKey. Implementations must be safe for concurrent use and
	// must not mutate the returned key.
	Key(context.Context, int64, int32) (string, error)
}

// Config tunes the upload pipeline. The zero value is usable: NewPipeline
// substitutes crypto/rand and defaultLeaseRenewInterval for unset values.
type Config struct {
	// UploadThreads is the number of parallel connections the storage layer uses
	// to send one part; values below one select the storage default.
	UploadThreads int
	// RandomizePartNames stores each part under a random name instead of
	// "<file name>.<part number>", hiding the original file name from Telegram.
	RandomizePartNames bool
	// DisableHashing skips tree-hash computation unless the caller supplies a
	// checksum or the session expects a specific algorithm. Without hashing,
	// parts are stored with no checksum.
	DisableHashing bool
	// Random is the entropy source for per-part encryption salts; nil means
	// crypto/rand. It must be safe for concurrent use, because one Pipeline may
	// serve several parts at once.
	Random io.Reader
	// LeaseRenewInterval is how often the part lease is renewed while the upload
	// runs; values at or below zero select defaultLeaseRenewInterval.
	LeaseRenewInterval time.Duration
}

// Pipeline stores upload parts for open upload sessions. It holds no per-request
// state, so one instance serves concurrent requests.
type Pipeline struct {
	// catalog owns the durable part lifecycle: claims, lease renewals, and stored
	// parts.
	catalog UploadCatalog
	// channels picks the storage channel for a part, rolling over when needed.
	channels ChannelResolver
	// storage publishes and deletes the Telegram documents that hold part bytes.
	storage telegramstore.Storage
	// keys resolves content-encryption keys by version. A nil provider only breaks
	// encrypted sessions, which fail with ErrEncryptionKey.
	keys KeyProvider
	// config is the caller's Config with the package defaults already applied; the
	// pipeline never mutates it.
	config Config
}

// NewPipeline returns a Pipeline over the given boundaries. Any of them may be
// nil: UploadPart then reports ErrUploadNotConfigured when catalog, channels, or
// storage is missing, and encrypted sessions report ErrEncryptionKey when keys is
// missing. The config is copied, so the caller may reuse and mutate it freely.
func NewPipeline(catalog UploadCatalog, channels ChannelResolver, storage telegramstore.Storage, keys KeyProvider, cfg Config) *Pipeline {
	if cfg.Random == nil {
		cfg.Random = rand.Reader
	}
	if cfg.LeaseRenewInterval <= 0 {
		cfg.LeaseRenewInterval = defaultLeaseRenewInterval
	}
	return &Pipeline{catalog: catalog, channels: channels, storage: storage, keys: keys, config: cfg}
}

// UploadPartRequest describes one part to store.
type UploadPartRequest struct {
	// UserID is the TelDrive user owning the upload session and must be positive.
	UserID int64
	// UploadID identifies the open session and must not be uuid.Nil.
	UploadID uuid.UUID
	// PartNo is the 1-based part number within the session; part sizes must match
	// the session's fixed part size.
	PartNo int32
	// RequestedChannelID asks for a specific storage channel; zero lets the
	// ChannelResolver choose one.
	RequestedChannelID int64
	// PlainSize is the exact number of plaintext bytes Body must yield and must be
	// positive. An encrypted session stores a larger ciphertext, which the
	// pipeline derives itself.
	PlainSize int64
	// Checksum is the optional expected tree hash of the plaintext part as hex,
	// which must decode to treehash.DigestSize bytes. When set, a mismatch fails
	// the part with ErrChecksumMismatch; nil skips only the comparison, not the
	// hashing.
	Checksum *string
	// Body supplies the plaintext bytes. It must yield exactly PlainSize bytes and
	// is never closed by the pipeline.
	Body io.Reader
}

// UploadPartResult is the stored part, either freshly written or already present.
type UploadPartResult struct {
	// Part is the catalog row of the stored part.
	Part *sqlcgen.UploadPart
	// Existing reports that the part was already stored with a matching size and
	// checksum, so Body was not read and nothing was uploaded.
	Existing bool
}

// UploadPart stores one part of an open upload session and returns its catalog
// row. It validates the request, claims a lease on the part, renews that lease
// while the Telegram upload runs, verifies the plaintext size and the optional
// tree hash, and commits the part; every failure that follows a successful
// storage upload deletes the published message first.
//
// The call is idempotent per part: a part already stored with the same plaintext
// size and a matching checksum is returned with Existing set and Body is never
// read, while a part currently leased by another attempt fails with
// uploads.ErrPartBusy. Body is read exactly once and is not closed.
//
// It returns ErrUploadNotConfigured when a boundary is missing, ErrInvalidUpload
// for a malformed request or checksum, and otherwise the boundary's own error.
func (p *Pipeline) UploadPart(ctx context.Context, request UploadPartRequest) (*UploadPartResult, error) {
	if p.catalog == nil || p.channels == nil || p.storage == nil {
		return nil, ErrUploadNotConfigured
	}
	if request.UserID <= 0 || request.UploadID == uuid.Nil || request.PartNo <= 0 || request.PlainSize <= 0 || request.Body == nil {
		return nil, ErrInvalidUpload
	}
	checksum, err := normalizeOptionalChecksum(request.Checksum)
	if err != nil {
		return nil, err
	}
	request.Checksum = checksum

	session, err := p.catalog.Get(ctx, request.UserID, request.UploadID)
	if err != nil {
		return nil, err
	}
	if existing, err := p.catalog.GetPart(ctx, request.UserID, request.UploadID, request.PartNo); err == nil {
		if existing.State == sqlcgen.UploadPartStateStored {
			if existing.PlainSize != request.PlainSize || !checksumMatches(existing.Checksum.String, existing.Checksum.Valid, request.Checksum) {
				return nil, uploads.ErrPartConflict
			}
			return &UploadPartResult{Part: existing, Existing: true}, nil
		}
	} else if !errors.Is(err, uploads.ErrNotFound) {
		return nil, err
	}
	slog.DebugContext(ctx, "upload part started", "upload_id", request.UploadID, "part_no", request.PartNo, "plain_size", request.PlainSize, "requested_channel_id", request.RequestedChannelID)

	channelID, err := p.channels.Resolve(ctx, request.UserID, request.RequestedChannelID)
	if err != nil {
		return nil, err
	}
	claim, err := p.catalog.ClaimPart(ctx, uploads.ClaimPartInput{
		UserID:    request.UserID,
		UploadID:  request.UploadID,
		PartNo:    request.PartNo,
		ChannelID: channelID,
		PlainSize: request.PlainSize,
		Checksum:  request.Checksum,
	})
	if err != nil {
		return nil, err
	}
	if claim.Existing {
		return &UploadPartResult{Part: claim.Part, Existing: true}, nil
	}
	uploadCtx, cancelUpload := context.WithCancelCause(ctx)
	defer cancelUpload(nil)
	renewErrors := p.renewPartLease(uploadCtx, cancelUpload, request, claim.LeaseToken)

	exact := newExactReader(uploadCtx, request.Body, request.PlainSize)
	shouldHash := !p.config.DisableHashing || request.Checksum != nil || session.ExpectedHashAlgorithm.Valid
	var hasher *treehash.BlockHasher
	plainReader := io.Reader(exact)
	if shouldHash {
		hasher = treehash.NewBlockHasher()
		plainReader = io.TeeReader(exact, hasher)
	}
	storedReader := plainReader
	storedSize := request.PlainSize
	var salt *string

	if session.Encryption {
		if p.keys == nil || !session.EncryptionKeyVersion.Valid {
			return nil, p.failPart(ctx, request, claim.LeaseToken, "encryption_key_missing", ErrEncryptionKey)
		}
		key, keyErr := p.keys.Key(ctx, request.UserID, session.EncryptionKeyVersion.Int32)
		if keyErr != nil || key == "" {
			return nil, p.failPart(ctx, request, claim.LeaseToken, "encryption_key_missing", errors.Join(ErrEncryptionKey, keyErr))
		}
		generatedSalt, saltErr := generateSalt(p.config.Random)
		if saltErr != nil {
			return nil, p.failPart(ctx, request, claim.LeaseToken, "salt_generation_failed", saltErr)
		}
		cipher, cipherErr := contentcrypto.NewCipher(key, generatedSalt)
		if cipherErr != nil {
			return nil, p.failPart(ctx, request, claim.LeaseToken, "cipher_initialization_failed", cipherErr)
		}
		encrypted, cipherErr := cipher.EncryptData(plainReader)
		if cipherErr != nil {
			return nil, p.failPart(ctx, request, claim.LeaseToken, "cipher_initialization_failed", cipherErr)
		}
		storedReader = encrypted
		storedSize = contentcrypto.EncryptedSize(request.PlainSize)
		salt = &generatedSalt
	}

	stored, err := p.storage.Upload(uploadCtx, telegramstore.UploadRequest{
		UserID:    request.UserID,
		ChannelID: channelID,
		Name:      p.partName(session.Name, request.PartNo),
		Reader:    storedReader,
		Size:      storedSize,
		Threads:   p.config.UploadThreads,
	})
	slog.DebugContext(ctx, "uploading part to telegram", "upload_id", request.UploadID, "part_no", request.PartNo, "channel_id", channelID, "stored_size", storedSize, "threads", p.config.UploadThreads)
	if err != nil {
		if renewErr := pendingRenewError(renewErrors); renewErr != nil {
			err = errors.Join(err, renewErr)
		}
		return nil, p.failPart(ctx, request, claim.LeaseToken, "telegram_upload_failed", err)
	}
	if renewErr := pendingRenewError(renewErrors); renewErr != nil {
		cleanupErr := p.deleteUploaded(ctx, request.UserID, stored)
		return nil, errors.Join(renewErr, cleanupErr)
	}
	if stored.ChannelID != channelID || stored.MessageID <= 0 || stored.Size != storedSize {
		cleanupErr := p.deleteUploaded(ctx, request.UserID, stored)
		return nil, p.failPart(ctx, request, claim.LeaseToken, "telegram_size_mismatch", errors.Join(ErrStoredSizeMismatch, cleanupErr))
	}
	if err := exact.Verify(); err != nil {
		cleanupErr := p.deleteUploaded(ctx, request.UserID, stored)
		return nil, p.failPart(ctx, request, claim.LeaseToken, "body_size_mismatch", errors.Join(err, cleanupErr))
	}

	var (
		actualChecksum *string
		blockHashes    []byte
	)
	if shouldHash {
		blockHashes = hasher.Sum()
		if len(blockHashes) == 0 || len(blockHashes)%treehash.DigestSize != 0 {
			cleanupErr := p.deleteUploaded(ctx, request.UserID, stored)
			return nil, p.failPart(ctx, request, claim.LeaseToken, "hash_generation_failed", errors.Join(ErrInvalidUpload, cleanupErr))
		}
		value := treehash.SumToHex(treehash.ComputeTreeHash(blockHashes))
		actualChecksum = &value
		if request.Checksum != nil && !strings.EqualFold(*request.Checksum, value) {
			cleanupErr := p.deleteUploaded(ctx, request.UserID, stored)
			return nil, p.failPart(ctx, request, claim.LeaseToken, "checksum_mismatch", errors.Join(ErrChecksumMismatch, cleanupErr))
		}
	}

	storeCtx, cancelStore := partCleanupContext(ctx)
	defer cancelStore()
	part, err := p.catalog.StorePart(storeCtx, uploads.StorePartInput{
		UploadID:    request.UploadID,
		PartNo:      request.PartNo,
		LeaseToken:  claim.LeaseToken,
		MessageID:   stored.MessageID,
		StoredSize:  stored.Size,
		Checksum:    valueOrEmpty(actualChecksum),
		Salt:        salt,
		BlockHashes: append([]byte(nil), blockHashes...),
	})
	if err != nil {
		cleanupErr := p.deleteUploaded(ctx, request.UserID, stored)
		return nil, errors.Join(err, cleanupErr)
	}
	slog.DebugContext(ctx, "upload part completed", "upload_id", request.UploadID, "part_no", request.PartNo, "channel_id", stored.ChannelID, "message_id", stored.MessageID, "stored_size", stored.Size)
	return &UploadPartResult{Part: part}, nil
}

// failPart marks the part failed and releases its lease so a later attempt can
// claim it again. The catalog call runs on a context detached from the request,
// so it still happens after cancellation; a lost lease is not reported, because
// another attempt owns the part by then, while any other catalog failure is
// joined into the returned error.
func (p *Pipeline) failPart(ctx context.Context, request UploadPartRequest, leaseToken uuid.UUID, code string, cause error) error {
	cleanupCtx, cancel := partCleanupContext(ctx)
	defer cancel()
	_, failErr := p.catalog.FailPart(cleanupCtx, uploads.FailPartInput{
		UploadID:   request.UploadID,
		PartNo:     request.PartNo,
		LeaseToken: leaseToken,
		ErrorCode:  code,
	})
	if failErr != nil && !errors.Is(failErr, uploads.ErrLeaseLost) {
		return errors.Join(cause, failErr)
	}
	return cause
}

// renewPartLease starts a goroutine that renews the part lease every
// Config.LeaseRenewInterval until ctx ends, and returns a buffered one-shot
// channel carrying the first renewal error. That error also cancels ctx with the
// error as cause, aborting the in-flight Telegram upload; callers collect it
// through pendingRenewError once the upload returns.
func (p *Pipeline) renewPartLease(ctx context.Context, cancel context.CancelCauseFunc, request UploadPartRequest, leaseToken uuid.UUID) <-chan error {
	errorsCh := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(p.config.LeaseRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, stop := partCleanupContext(ctx)
				err := p.catalog.RenewPart(renewCtx, uploads.RenewPartInput{
					UploadID: request.UploadID, PartNo: request.PartNo, LeaseToken: leaseToken,
				})
				stop()
				if err != nil {
					errorsCh <- err
					cancel(err)
					return
				}
			}
		}
	}()
	return errorsCh
}

// pendingRenewError returns the renewal error that has already arrived, or nil
// while the lease is still healthy. It never blocks, so callers can poll it after
// an upload finishes to learn that the lease was lost underneath them.
func pendingRenewError(errorsCh <-chan error) error {
	select {
	case err := <-errorsCh:
		return err
	default:
		return nil
	}
}

// partCleanupContext returns a context that keeps the values of ctx but not its
// cancellation, bounded by partCleanupTimeout, so catalog bookkeeping still
// completes after the client goes away.
func partCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), partCleanupTimeout)
}

// deleteUploaded removes the Telegram message published for a part whose catalog
// write will not happen. It does nothing when the part carries no message, and
// otherwise wraps the storage error so it can be reported next to the original
// failure.
func (p *Pipeline) deleteUploaded(ctx context.Context, userID int64, part telegramstore.StoredPart) error {
	if part.ChannelID == 0 || part.MessageID <= 0 {
		return nil
	}
	if err := p.storage.DeleteMessages(ctx, userID, part.ChannelID, []int64{part.MessageID}); err != nil {
		return fmt.Errorf("compensate Telegram upload: %w", err)
	}
	return nil
}

// partName returns the Telegram document name of a part: "<fileName>.<NNN>" with
// the part number zero-padded to at least three digits by default, or a random
// hex digest when Config.RandomizePartNames is set, which keeps the original file
// name out of Telegram.
func (p *Pipeline) partName(fileName string, partNo int32) string {
	if !p.config.RandomizePartNames {
		return fmt.Sprintf("%s.%03d", fileName, partNo)
	}
	digest := sha256.Sum256([]byte(uuid.NewString()))
	return hex.EncodeToString(digest[:])
}

// valueOrEmpty dereferences value, yielding an empty string for a nil pointer. It
// maps an absent optional checksum onto the catalog's empty-string column.
func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// generateSalt reads 32 bytes from random and returns their SHA-256 digest,
// base64url encoded, as the per-part content-encryption salt; the digest gives
// the salt a fixed size regardless of the entropy source. A nil source is an
// error.
func generateSalt(random io.Reader) (string, error) {
	if random == nil {
		return "", errors.New("random source is required")
	}
	seed := make([]byte, 32)
	if _, err := io.ReadFull(random, seed); err != nil {
		return "", fmt.Errorf("read encryption salt entropy: %w", err)
	}
	digest := sha256.Sum256(seed)
	return base64.URLEncoding.EncodeToString(digest[:]), nil
}

// normalizeOptionalChecksum lowercases and trims an optional checksum and
// requires it to be hex that decodes to treehash.DigestSize bytes. It reports
// ErrInvalidUpload for anything else and leaves a nil input nil.
func normalizeOptionalChecksum(value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	normalized := strings.ToLower(strings.TrimSpace(*value))
	digest, err := hex.DecodeString(normalized)
	if err != nil || len(digest) != treehash.DigestSize {
		return nil, ErrInvalidUpload
	}
	return &normalized, nil
}

// checksumMatches reports whether a stored checksum satisfies the caller's
// expectation. A nil expectation matches any stored value; a stored part without
// a valid checksum never matches a non-nil one, and the comparison ignores case.
func checksumMatches(stored string, valid bool, expected *string) bool {
	if expected == nil {
		return true
	}
	return valid && strings.EqualFold(stored, *expected)
}

// exactReader yields at most a fixed number of bytes from source and reports the
// mismatch when the source turns out shorter or longer than promised, so a
// truncated or padded body cannot be stored silently.
type exactReader struct {
	// ctx is the upload context, checked before every read so a cancelled request
	// stops the transfer promptly.
	ctx context.Context
	// source is the caller's body; exactReader never closes it.
	source io.Reader
	// remaining counts the bytes that may still be read before the promised size
	// is exhausted.
	remaining int64
}

// newExactReader wraps source so that exactly size bytes are expected from it:
// Read stops at size, and Verify rejects both a short body and one with trailing
// bytes.
func newExactReader(ctx context.Context, source io.Reader, size int64) *exactReader {
	return &exactReader{ctx: ctx, source: source, remaining: size}
}

// Read returns at most the bytes still expected from the source. It reports
// ErrBodyTooShort as soon as the source ends early, io.ErrNoProgress when the
// source makes no progress, and io.EOF once the promised size was delivered; the
// final read may return the last bytes together with io.EOF.
func (r *exactReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.source.Read(p)
	r.remaining -= int64(n)
	if errors.Is(err, io.EOF) && r.remaining > 0 {
		return n, ErrBodyTooShort
	}
	if n == 0 && err == nil {
		return 0, io.ErrNoProgress
	}
	return n, err
}

// Verify confirms the body matched the promised size: ErrBodyTooShort while bytes
// are still outstanding, ErrBodyTooLong when the source has more bytes after
// them, and the source's own error otherwise. The over-long probe consumes one
// byte of the extra data.
func (r *exactReader) Verify() error {
	if r.remaining != 0 {
		return ErrBodyTooShort
	}
	var probe [1]byte
	n, err := r.source.Read(probe[:])
	if n > 0 {
		return ErrBodyTooLong
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// StaticKeyProvider is suitable for configuration-backed key rotation. New
// uploads reference a version while old files remain decryptable.
type StaticKeyProvider map[int32]string

// Key returns the configured key for version. A version absent from the map, or
// mapped to an empty string, reports ErrEncryptionKey so an upload never silently
// stores plaintext under a missing key.
func (p StaticKeyProvider) Key(_ context.Context, _ int64, version int32) (string, error) {
	key, ok := p[version]
	if !ok || key == "" {
		return "", ErrEncryptionKey
	}
	return key, nil
}
