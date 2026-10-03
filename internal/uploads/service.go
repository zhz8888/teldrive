package uploads

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
	"github.com/tgdrive/teldrive/v2/internal/treehash"
)

const (
	// defaultPartSize is the part size used when a session does not request one:
	// 512 MiB.
	defaultPartSize int64 = 512 * 1024 * 1024
	// defaultSessionTTL is how long a session may stay claimable after creation;
	// ExpiresAt is written as now plus this duration.
	defaultSessionTTL = 7 * 24 * time.Hour
	// defaultLeaseTTL is the lifetime of a part lease, both when it is first
	// granted and on every renewal. It is deliberately short, because an
	// abandoned uploader only blocks its part until the lease lapses.
	defaultLeaseTTL = time.Minute
	// maxPartSize caps the part size a session may request. The default is
	// 512 MiB, and anything above this is refused rather than stored, which keeps
	// the byte arithmetic below inside int64.
	maxPartSize int64 = 4 << 30
	// maxUploadParts caps the part number one session may address. A part number
	// beyond this cannot belong to a real upload and would otherwise be able to
	// pin a session that can never be completed.
	maxUploadParts = 1 << 20
)

// leaseSeconds converts a lease lifetime into the whole seconds the lease
// queries add to the database clock, which is the clock that decides whether a
// lease is still live. A non-positive duration yields zero seconds, so a
// misconfigured lease expires immediately instead of never.
func leaseSeconds(ttl time.Duration) int32 {
	if ttl <= 0 {
		return 0
	}
	return int32(ttl / time.Second)
}

var (
	// ErrInvalidInput reports a request that violates the documented contract of
	// the called method, such as missing ids, negative sizes, a half-specified
	// hash or an unknown conflict policy. It is never used for database failures.
	ErrInvalidInput = errors.New("invalid upload input")
	// ErrNotFound reports that no session exists for the requested id and user.
	// Sessions owned by another user are reported the same way, so callers cannot
	// tell "unknown" apart from "not yours".
	ErrNotFound = errors.New("upload not found")
	// ErrExpired reports that a session is past its ExpiresAt deadline. The upload
	// cleanup worker later moves such sessions to the expired state.
	ErrExpired = errors.New("upload expired")
	// ErrInvalidState reports a transition the session cannot make, most often
	// completing, claiming, or aborting a session that already left the open state.
	ErrInvalidState = errors.New("invalid upload state")
	// ErrPartConflict reports a claim for a part that is already stored with a
	// different size or checksum, so the retry cannot be served as an idempotent
	// hit and the caller must decide which version wins.
	ErrPartConflict = errors.New("upload part conflicts with stored metadata")
	// ErrPartBusy reports that another uploader still holds a live lease on the
	// part; the caller may retry after the lease lapses.
	ErrPartBusy = errors.New("upload part is currently leased")
	// ErrLeaseLost reports that a lease token no longer matches the stored part:
	// the lease expired and the part was re-claimed, or the attempt already
	// finished. The uploader must stop writing that part.
	ErrLeaseLost = errors.New("upload part lease was lost")
	// ErrIncomplete reports that completion found missing, failed, mis-sized or
	// non-consecutive parts. The completion transaction is rolled back, so the
	// session stays open and the uploader can repair the parts and retry.
	ErrIncomplete = errors.New("upload parts are incomplete")
	// ErrNameConflict reports that the destination name is taken by an active
	// entry and the session's conflict policy cannot resolve the clash.
	ErrNameConflict = errors.New("destination name already exists")
	// ErrInvalidParent reports that ParentID is not an active folder owned by the
	// session's user.
	ErrInvalidParent = errors.New("invalid parent folder")
	// ErrInvalidChannel reports that the requested storage channel does not exist
	// or is not usable by the session's user.
	ErrInvalidChannel = errors.New("invalid storage channel")
	// ErrHashMismatch reports that the tree hash recomputed from the stored parts
	// differs from the hash the session was created with.
	ErrHashMismatch = errors.New("upload hash does not match expected hash")
	// ErrUnsupportedConflictPolicy reports a conflict policy that the called
	// endpoint does not implement: Create and prepareConflictPolicy return it for
	// any value outside the fail, replace and rename policies the package applies
	// at completion, and the API layer returns it when folder creation asks for
	// anything but the fail policy. Retrying with the same policy cannot succeed.
	ErrUnsupportedConflictPolicy = errors.New("upload conflict policy is not implemented")
)

// CatalogCacheInvalidator expires cached catalog rows for files that the uploads
// package changes without going through the catalog service, currently only the
// file retired by a replace-policy completion. Implementations must be safe for
// concurrent use; Complete calls them after the database transaction commits.
type CatalogCacheInvalidator interface {
	// InvalidateFiles expires the cached entries of fileIDs for the owner userID.
	// A non-positive userID or uuid.Nil ids are ignored, and an empty fileIDs list
	// is a no-op.
	InvalidateFiles(context.Context, int64, ...uuid.UUID)
}

// Service runs the upload session protocol against a pgx pool. It keeps no
// per-session state in memory, so one instance is shared by all requests and is
// safe for concurrent use; session, part and lease state live in PostgreSQL.
type Service struct {
	// pool is the connection pool that owns the completion transaction; queries
	// runs the non-transactional statements on the same pool.
	pool *pgxpool.Pool
	// queries wraps pool and is rebound to a transaction inside Complete.
	queries *sqlcgen.Queries
	// now reports the current time; tests replace it to drive lease and session
	// expiry deterministically.
	now func() time.Time
	// sessionTTL is added to now when a session's ExpiresAt is written.
	sessionTTL time.Duration
	// leaseTTL is the lease lifetime granted by ClaimPart and RenewPart.
	leaseTTL time.Duration
	// catalogInvalidator is optional and set once during composition via
	// SetCacheInvalidator; while nil, replace-policy completions skip cache
	// invalidation.
	catalogInvalidator CatalogCacheInvalidator
}

// NewService returns a Service using pool for all statements. An optional
// sessionTTLs overrides the default 7-day session lifetime when its first value
// is positive; further values are ignored.
func NewService(pool *pgxpool.Pool, sessionTTLs ...time.Duration) *Service {
	sessionTTL := defaultSessionTTL
	if len(sessionTTLs) > 0 && sessionTTLs[0] > 0 {
		sessionTTL = sessionTTLs[0]
	}
	return &Service{
		pool:       pool,
		queries:    sqlcgen.New(pool),
		now:        time.Now,
		sessionTTL: sessionTTL,
		leaseTTL:   defaultLeaseTTL,
	}
}

// SetCacheInvalidator wires the catalog cache that Complete invalidates after a
// replace-policy completion retires the previous file. It is meant to be called
// once during composition, before requests are served; leaving the invalidator
// nil disables invalidation.
func (s *Service) SetCacheInvalidator(catalogInvalidator CatalogCacheInvalidator) {
	s.catalogInvalidator = catalogInvalidator
}

// CreateInput describes a new upload session. The session is created in the open
// state with ExpiresAt set to the service's session TTL from now.
type CreateInput struct {
	// ID is the session id; uuid.Nil asks the service to generate one.
	ID uuid.UUID
	// UserID owns the session and must be positive.
	UserID int64
	// ParentID is the destination folder, or nil for the user's root. A non-nil id
	// must be an active folder of UserID, otherwise ErrInvalidParent is returned.
	ParentID *uuid.UUID
	// Name is the destination file name, stored verbatim without normalization. It
	// must not be blank after trimming, otherwise Create returns ErrInvalidInput.
	Name string
	// ExpectedSize is the total plaintext size in bytes, or -1 when it is not known
	// yet; the real size is then derived from the stored parts at completion. It
	// must not be less than -1.
	ExpectedSize int64
	// ExpectedHashAlgorithm and ExpectedHashValue are the optional expected tree
	// hash and must be set together or both nil. The algorithm is lowercased and
	// must be "blake3", the value must be a hex digest of treehash.DigestSize bytes,
	// and the comparison at completion is case-insensitive.
	ExpectedHashAlgorithm *string
	// ExpectedHashValue is the expected hex tree hash; see ExpectedHashAlgorithm.
	ExpectedHashValue *string
	// MIMEType is the optional content type recorded on the published file.
	MIMEType *string
	// ModTime is the file modification time; the zero value means now.
	ModTime time.Time
	// Encryption requests encryption of the stored parts.
	Encryption bool
	// EncryptionKeyVersion selects the user's encryption key and must be non-nil
	// exactly when Encryption is true.
	EncryptionKeyVersion *int32
	// ConflictPolicy decides how completion handles an occupied destination name.
	// The empty value means NameConflictPolicyFail; fail, replace and rename are
	// accepted, and any other value is rejected with ErrUnsupportedConflictPolicy.
	ConflictPolicy sqlcgen.NameConflictPolicy
	// PartSize is the size in bytes every part but the last must have; a
	// non-positive value selects defaultPartSize.
	PartSize int64
}

// Create validates in and inserts a new upload session in the open state, expiring
// sessionTTL after creation. It returns ErrInvalidInput for a malformed request
// (non-positive user, size below -1, a name that is blank after trimming, only one
// half of the expected hash, an encryption flag that disagrees with the key version),
// ErrUnsupportedConflictPolicy for a policy outside fail, replace and rename,
// ErrInvalidParent when ParentID is not an active folder of the user, and otherwise
// the raw error of CreateUploadSession, with no wrapping added.
// Destination name uniqueness is deliberately not checked here: it is enforced at
// completion.
func (s *Service) Create(ctx context.Context, in CreateInput) (*sqlcgen.UploadSession, error) {
	if in.UserID <= 0 || in.ExpectedSize < -1 {
		return nil, ErrInvalidInput
	}
	// A name that is blank after trimming would fail the
	// upload_sessions_name_not_blank check constraint, so it is rejected here
	// instead of surfacing as a database error.
	if strings.TrimSpace(in.Name) == "" {
		return nil, ErrInvalidInput
	}
	if (in.ExpectedHashAlgorithm == nil) != (in.ExpectedHashValue == nil) {
		return nil, ErrInvalidInput
	}
	if in.ExpectedHashAlgorithm != nil {
		algorithm, value, err := normalizeExpectedHash(*in.ExpectedHashAlgorithm, *in.ExpectedHashValue)
		if err != nil {
			return nil, err
		}
		in.ExpectedHashAlgorithm = &algorithm
		in.ExpectedHashValue = &value
	}
	if in.Encryption != (in.EncryptionKeyVersion != nil) {
		return nil, ErrInvalidInput
	}
	if in.ConflictPolicy == "" {
		in.ConflictPolicy = sqlcgen.NameConflictPolicyFail
	}
	switch in.ConflictPolicy {
	case sqlcgen.NameConflictPolicyFail, sqlcgen.NameConflictPolicyReplace, sqlcgen.NameConflictPolicyRename:
		// Applied atomically during publication.
	default:
		return nil, ErrUnsupportedConflictPolicy
	}
	if in.ParentID != nil {
		if _, err := s.queries.GetActiveFolderForUser(ctx, sqlcgen.GetActiveFolderForUserParams{
			FolderID: dbtypes.UUID(*in.ParentID),
			UserID:   in.UserID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrInvalidParent
			}
			return nil, fmt.Errorf("get upload parent: %w", err)
		}
	}
	if in.PartSize <= 0 {
		in.PartSize = defaultPartSize
	}
	if in.PartSize > maxPartSize {
		return nil, ErrInvalidInput
	}
	if remainder := in.PartSize % treehash.BlockSize; remainder != 0 {
		// The file hash is computed from the block hashes of each part, so part
		// boundaries have to fall on tree-hash block boundaries for the result to
		// equal the digest of a whole-file pass over the same bytes. The client
		// reads the effective size back from the session it just created.
		in.PartSize += treehash.BlockSize - remainder
		if in.PartSize > maxPartSize {
			return nil, ErrInvalidInput
		}
	}
	modTime := in.ModTime
	if modTime.IsZero() {
		modTime = s.now().UTC()
	}
	id := in.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	return s.queries.CreateUploadSession(ctx, sqlcgen.CreateUploadSessionParams{
		ID:                    dbtypes.UUID(id),
		UserID:                in.UserID,
		ParentID:              dbtypes.OptionalUUID(in.ParentID),
		Name:                  in.Name,
		ExpectedSize:          in.ExpectedSize,
		ExpectedHashAlgorithm: dbtypes.OptionalText(in.ExpectedHashAlgorithm),
		ExpectedHashValue:     dbtypes.OptionalText(in.ExpectedHashValue),
		MimeType:              dbtypes.OptionalText(in.MIMEType),
		ModTime:               dbtypes.Time(modTime.UTC()),
		Encryption:            in.Encryption,
		EncryptionKeyVersion:  dbtypes.OptionalInt4(in.EncryptionKeyVersion),
		ConflictPolicy:        in.ConflictPolicy,
		PartSize:              in.PartSize,
		ExpiresAt:             dbtypes.Time(s.now().UTC().Add(s.sessionTTL)),
	})
}

// Get returns the session with the given id when it belongs to userID. A session
// owned by somebody else is reported as ErrNotFound, so the call cannot be used to
// probe for foreign ids.
func (s *Service) Get(ctx context.Context, userID int64, uploadID uuid.UUID) (*sqlcgen.UploadSession, error) {
	if userID <= 0 {
		return nil, ErrInvalidInput
	}
	session, err := s.queries.GetUploadSessionForUser(ctx, sqlcgen.GetUploadSessionForUserParams{
		UploadID: dbtypes.UUID(uploadID),
		UserID:   userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get upload: %w", err)
	}
	return session, nil
}

// GetAnyOwner returns a session regardless of its owner and therefore performs no
// authorization at all: callers must compare the returned UserID with the actor or
// resolve access to the session's parent through the shares package before acting
// on the row. It exists for the authenticated-grant and public-share upload flows,
// which resolve the owner separately. An unknown id yields ErrNotFound, while
// uuid.Nil is rejected with ErrInvalidInput.
func (s *Service) GetAnyOwner(ctx context.Context, uploadID uuid.UUID) (*sqlcgen.UploadSession, error) {
	if uploadID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	session, err := s.queries.GetUploadSessionAnyOwner(ctx, dbtypes.UUID(uploadID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get upload by id: %w", err)
	}
	return session, nil
}

// GetPart returns one upload part after verifying ownership of its session.
func (s *Service) GetPart(ctx context.Context, userID int64, uploadID uuid.UUID, partNo int32) (*sqlcgen.UploadPart, error) {
	if userID <= 0 || uploadID == uuid.Nil || partNo <= 0 {
		return nil, ErrInvalidInput
	}
	if _, err := s.Get(ctx, userID, uploadID); err != nil {
		return nil, err
	}
	part, err := s.queries.GetUploadPart(ctx, sqlcgen.GetUploadPartParams{
		UploadID: dbtypes.UUID(uploadID),
		PartNo:   partNo,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get upload part: %w", err)
	}
	return part, nil
}

// ClaimPartInput reserves one part of an upload session for exclusive writing.
type ClaimPartInput struct {
	// UserID is the session owner and must be positive.
	UserID int64
	// UploadID identifies the session whose part is claimed.
	UploadID uuid.UUID
	// PartNo is the 1-based part number; when the session has a known expected size
	// it must address a byte range inside that size.
	PartNo int32
	// ChannelID is the storage channel the part will be uploaded to and must be a
	// channel the user may use.
	ChannelID int64
	// PlainSize is the plaintext size of this part in bytes.
	PlainSize int64
	// Checksum is the optional expected tree hash of the part as a hex digest; it
	// is lowercased and validated before the part is claimed.
	Checksum *string
}

// ClaimPartResult reports the outcome of a claim. Callers must branch on Existing
// before using the lease.
type ClaimPartResult struct {
	// Part is the stored or newly reserved part row; its State is stored when
	// Existing is true and uploading otherwise.
	Part *sqlcgen.UploadPart
	// LeaseToken authorizes StorePart, RenewPart and FailPart and is uuid.Nil when
	// Existing is true, because a stored part holds no lease.
	LeaseToken uuid.UUID
	// Existing is true when an identical part was already stored, so there is
	// nothing to upload and the claim is idempotent.
	Existing bool
}

// ClaimPart reserves a part for writing and returns the lease token that
// StorePart, RenewPart and FailPart require. The lease lasts leaseTTL and must be
// renewed while the upload runs: once it lapses another claimer may take the part
// over and the old token starts failing with ErrLeaseLost.
//
// The call is idempotent. A part already stored with the same size and checksum
// comes back with Existing set and no lease, stored with different metadata it
// yields ErrPartConflict, and one still under a live lease yields ErrPartBusy;
// failed and lease-expired parts are re-claimed in place. The session must be open
// and unexpired, the part must fit its geometry, and the channel must belong to
// the user, otherwise ErrInvalidState, ErrExpired or ErrInvalidChannel is
// returned. Concurrent claims for the same part are settled by the database's
// unique constraint, so exactly one caller wins and the others see ErrPartBusy.
// The deadline is computed by the database itself, so a lease can never be
// granted already expired by a clock difference between the two hosts.
func (s *Service) ClaimPart(ctx context.Context, in ClaimPartInput) (*ClaimPartResult, error) {
	if in.UserID <= 0 || in.PartNo <= 0 || in.ChannelID == 0 || in.PlainSize < 0 {
		return nil, ErrInvalidInput
	}
	if in.Checksum != nil {
		checksum, err := normalizeDigest(*in.Checksum)
		if err != nil {
			return nil, err
		}
		in.Checksum = &checksum
	}
	session, err := s.Get(ctx, in.UserID, in.UploadID)
	if err != nil {
		return nil, err
	}
	if session.State != sqlcgen.UploadStateOpen {
		return nil, ErrInvalidState
	}
	if !session.ExpiresAt.Time.After(s.now()) {
		return nil, ErrExpired
	}
	if err := validatePartShape(session, in.PartNo, in.PlainSize); err != nil {
		return nil, err
	}
	if _, err := s.queries.GetChannelForUser(ctx, sqlcgen.GetChannelForUserParams{
		UserID:    in.UserID,
		ChannelID: in.ChannelID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidChannel
		}
		return nil, fmt.Errorf("get upload channel: %w", err)
	}

	existing, err := s.queries.GetUploadPart(ctx, sqlcgen.GetUploadPartParams{
		UploadID: dbtypes.UUID(in.UploadID),
		PartNo:   in.PartNo,
	})
	if err == nil && existing.State == sqlcgen.UploadPartStateStored {
		if existing.PlainSize == in.PlainSize && (in.Checksum == nil || optionalTextEqual(existing.Checksum, in.Checksum)) {
			return &ClaimPartResult{Part: existing, Existing: true}, nil
		}
		return nil, ErrPartConflict
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("get upload part: %w", err)
	}

	leaseToken := uuid.New()
	part, err := s.queries.ClaimUploadPart(ctx, sqlcgen.ClaimUploadPartParams{
		UploadID:     dbtypes.UUID(in.UploadID),
		PartNo:       in.PartNo,
		ChannelID:    in.ChannelID,
		PlainSize:    in.PlainSize,
		Checksum:     dbtypes.OptionalText(in.Checksum),
		LeaseToken:   dbtypes.UUID(leaseToken),
		LeaseSeconds: leaseSeconds(s.leaseTTL),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPartBusy
	}
	if err != nil {
		return nil, fmt.Errorf("claim upload part: %w", err)
	}
	return &ClaimPartResult{Part: part, LeaseToken: leaseToken}, nil
}

// StorePartInput records a part whose bytes have been written to storage.
type StorePartInput struct {
	// UploadID identifies the session the part belongs to.
	UploadID uuid.UUID
	// PartNo is the 1-based part number being marked stored.
	PartNo int32
	// LeaseToken is the token returned by ClaimPart; a stale token makes the call
	// fail with ErrLeaseLost and leaves the part untouched.
	LeaseToken uuid.UUID
	// MessageID is the storage message id of the stored object and must be
	// positive.
	MessageID int64
	// StoredSize is the on-disk size in bytes, after encryption when the session is
	// encrypted.
	StoredSize int64
	// Checksum is the part's tree hash as a hex digest, empty when hashing was
	// disabled.
	Checksum string
	// Salt is the per-part encryption salt, nil for unencrypted parts.
	Salt *string
	// BlockHashes is the concatenated fixed-width block digests behind Checksum.
	// It is empty exactly when Checksum is empty, and its length must otherwise be
	// a multiple of treehash.DigestSize.
	BlockHashes []byte
}

// RenewPartInput extends the lease of a part that is still uploading.
type RenewPartInput struct {
	// UploadID identifies the session the part belongs to.
	UploadID uuid.UUID
	// PartNo is the 1-based part number whose lease is renewed.
	PartNo int32
	// LeaseToken is the token returned by ClaimPart.
	LeaseToken uuid.UUID
}

// RenewPart pushes the lease deadline of an uploading part to the database
// clock's now plus leaseTTL, which is the clock the claim predicate reads.
// ErrLeaseLost means the token no longer matches, either because the lease lapsed
// and the part was re-claimed or because the attempt already finished; the
// uploader must stop writing that part.
func (s *Service) RenewPart(ctx context.Context, in RenewPartInput) error {
	if in.UploadID == uuid.Nil || in.PartNo <= 0 || in.LeaseToken == uuid.Nil {
		return ErrInvalidInput
	}
	updated, err := s.queries.RenewUploadPartLease(ctx, sqlcgen.RenewUploadPartLeaseParams{
		LeaseSeconds: leaseSeconds(s.leaseTTL),
		UploadID:     dbtypes.UUID(in.UploadID),
		PartNo:       in.PartNo,
		LeaseToken:   dbtypes.UUID(in.LeaseToken),
	})
	if err != nil {
		return fmt.Errorf("renew upload part lease: %w", err)
	}
	if updated == 0 {
		return ErrLeaseLost
	}
	return nil
}

// StorePart marks a claimed part as stored and releases its lease. The row must
// still be in the uploading state under this lease token, otherwise ErrLeaseLost
// is returned and nothing is written; that is also the outcome when a newer
// claimer has taken the part over. Checksum and BlockHashes must both be present
// or both absent, a non-empty BlockHashes must be a whole number of
// treehash.DigestSize digests, and a non-positive MessageID, negative StoredSize
// or malformed checksum is rejected with ErrInvalidInput. The block-hash slice is
// copied before it is sent to the database, so the caller may reuse its buffer
// afterwards.
func (s *Service) StorePart(ctx context.Context, in StorePartInput) (*sqlcgen.UploadPart, error) {
	if in.PartNo <= 0 || in.LeaseToken == uuid.Nil || in.MessageID <= 0 || in.StoredSize < 0 || (len(in.BlockHashes) > 0 && len(in.BlockHashes)%treehash.DigestSize != 0) {
		return nil, ErrInvalidInput
	}
	if (strings.TrimSpace(in.Checksum) == "") != (len(in.BlockHashes) == 0) {
		return nil, ErrInvalidInput
	}
	var checksum *string
	if strings.TrimSpace(in.Checksum) != "" {
		normalized, err := normalizeDigest(in.Checksum)
		if err != nil {
			return nil, err
		}
		checksum = &normalized
	}
	part, err := s.queries.MarkUploadPartStored(ctx, sqlcgen.MarkUploadPartStoredParams{
		MessageID:   dbtypes.Int8(in.MessageID),
		StoredSize:  dbtypes.Int8(in.StoredSize),
		Checksum:    dbtypes.OptionalText(checksum),
		Salt:        dbtypes.OptionalText(in.Salt),
		BlockHashes: append([]byte(nil), in.BlockHashes...),
		UploadID:    dbtypes.UUID(in.UploadID),
		PartNo:      in.PartNo,
		LeaseToken:  dbtypes.UUID(in.LeaseToken),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLeaseLost
	}
	if err != nil {
		return nil, fmt.Errorf("store upload part: %w", err)
	}
	return part, nil
}

// FailPartInput marks one part attempt as failed and releases its lease.
type FailPartInput struct {
	// UploadID identifies the session the part belongs to.
	UploadID uuid.UUID
	// PartNo is the 1-based part number of the failed attempt.
	PartNo int32
	// LeaseToken is the token returned by ClaimPart; a stale token reports
	// ErrLeaseLost and leaves a newer attempt untouched.
	LeaseToken uuid.UUID
	// ErrorCode is a short machine-readable reason stored as the part's
	// last_error_code; a blank value is rejected.
	ErrorCode string
}

// FailPart releases a part lease after a transport or validation failure. The
// lease token prevents a stale uploader from overwriting a newer attempt.
func (s *Service) FailPart(ctx context.Context, in FailPartInput) (*sqlcgen.UploadPart, error) {
	if in.UploadID == uuid.Nil || in.PartNo <= 0 || in.LeaseToken == uuid.Nil || strings.TrimSpace(in.ErrorCode) == "" {
		return nil, ErrInvalidInput
	}
	part, err := s.queries.MarkUploadPartFailed(ctx, sqlcgen.MarkUploadPartFailedParams{
		ErrorCode:  dbtypes.Text(strings.TrimSpace(in.ErrorCode)),
		UploadID:   dbtypes.UUID(in.UploadID),
		PartNo:     in.PartNo,
		LeaseToken: dbtypes.UUID(in.LeaseToken),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLeaseLost
	}
	if err != nil {
		return nil, fmt.Errorf("fail upload part: %w", err)
	}
	return part, nil
}

// Complete publishes a session as a catalog file inside a single transaction. It
// locks the session row, verifies that every part is stored and contiguous,
// recomputes the file's tree hash from the stored block hashes and compares it
// with the expected hash, resolves the destination name through the session's
// conflict policy, and finally inserts the file and its parts before committing.
//
// Completing an already completed session is idempotent and returns the file it
// published. An open session whose deadline has passed fails with ErrExpired, a
// session in the completing, aborted or expired state with ErrInvalidState, and an
// unknown id with ErrNotFound. Every error rolls the transaction back, so the
// session stays open and the uploader can repair it and retry. ErrIncomplete,
// ErrHashMismatch, ErrNameConflict and ErrNotFound reach the caller as those
// sentinels, directly or, for the parts-count mismatch, wrapped with %w, so
// errors.Is matches all of them. A replace-policy completion marks the previous
// file deletion_pending, revokes its shares inside the transaction, and invalidates
// its catalog cache entry after the commit.
func (s *Service) Complete(ctx context.Context, userID int64, uploadID uuid.UUID) (*sqlcgen.File, error) {
	if userID <= 0 {
		return nil, ErrInvalidInput
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin upload completion: %w", err)
	}
	defer tx.Rollback(ctx)
	q := s.queries.WithTx(tx)

	session, err := q.LockUploadSessionForCompletion(ctx, sqlcgen.LockUploadSessionForCompletionParams{
		UploadID: dbtypes.UUID(uploadID),
		UserID:   userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock upload: %w", err)
	}
	if session.State == sqlcgen.UploadStateCompleted {
		fileID, ok := dbtypes.GoogleUUID(session.FileID)
		if !ok {
			return nil, fmt.Errorf("completed upload has no file id")
		}
		file, err := q.GetFileForUser(ctx, sqlcgen.GetFileForUserParams{FileID: dbtypes.UUID(fileID), UserID: userID})
		if err != nil {
			return nil, fmt.Errorf("get completed file: %w", err)
		}
		return file, nil
	}
	if session.State != sqlcgen.UploadStateOpen {
		return nil, ErrInvalidState
	}
	if !session.ExpiresAt.Time.After(s.now()) {
		return nil, ErrExpired
	}

	completedSize, err := validateStoredParts(ctx, tx, session)
	if err != nil {
		return nil, err
	}
	if session.ExpectedSize < 0 {
		session, err = q.FinalizeUploadExpectedSize(ctx, sqlcgen.FinalizeUploadExpectedSizeParams{
			ExpectedSize: completedSize,
			UploadID:     dbtypes.UUID(uploadID),
			UserID:       userID,
		})
		if err != nil {
			return nil, fmt.Errorf("finalize upload size: %w", err)
		}
	}
	blockHashSets, err := q.ListStoredUploadPartHashes(ctx, dbtypes.UUID(uploadID))
	if err != nil {
		return nil, fmt.Errorf("list stored upload hashes: %w", err)
	}
	var hashAlgorithm, hashValue *string
	concatenated := make([]byte, 0)
	sawUnhashed := false
	for _, blockHashes := range blockHashSets {
		if len(blockHashes) == 0 {
			sawUnhashed = true
			if len(concatenated) > 0 || session.ExpectedHashAlgorithm.Valid {
				return nil, ErrIncomplete
			}
			continue
		}
		if sawUnhashed || len(blockHashes)%treehash.DigestSize != 0 {
			return nil, ErrIncomplete
		}
		concatenated = append(concatenated, blockHashes...)
	}
	if len(concatenated) > 0 {
		algorithm := string(treehash.TypeBlake3)
		value := treehash.SumToHex(treehash.ComputeTreeHash(concatenated))
		hashAlgorithm, hashValue = &algorithm, &value
	}
	if session.ExpectedHashAlgorithm.Valid {
		if hashAlgorithm == nil || !strings.EqualFold(session.ExpectedHashAlgorithm.String, *hashAlgorithm) || !strings.EqualFold(session.ExpectedHashValue.String, *hashValue) {
			return nil, ErrHashMismatch
		}
	}
	replacedID, err := prepareConflictPolicy(ctx, tx, session)
	if err != nil {
		return nil, err
	}
	if _, err := q.MarkUploadCompleting(ctx, sqlcgen.MarkUploadCompletingParams{
		UploadID: dbtypes.UUID(uploadID),
		UserID:   userID,
	}); err != nil {
		return nil, fmt.Errorf("mark upload completing: %w", err)
	}

	fileID := uuid.New()
	file, err := q.InsertFileFromUpload(ctx, sqlcgen.InsertFileFromUploadParams{
		FileID:        dbtypes.UUID(fileID),
		HashAlgorithm: dbtypes.OptionalText(hashAlgorithm),
		HashValue:     dbtypes.OptionalText(hashValue),
		UploadID:      dbtypes.UUID(uploadID),
		UserID:        userID,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrNameConflict
		}
		return nil, fmt.Errorf("create file from upload: %w", err)
	}
	inserted, err := q.InsertFilePartsFromUpload(ctx, sqlcgen.InsertFilePartsFromUploadParams{
		FileID:   dbtypes.UUID(fileID),
		UploadID: dbtypes.UUID(uploadID),
	})
	if err != nil {
		return nil, fmt.Errorf("copy upload parts: %w", err)
	}
	expectedParts := expectedPartCount(session.ExpectedSize, session.PartSize)
	if inserted != expectedParts {
		return nil, fmt.Errorf("copied %d upload parts, expected %d: %w", inserted, expectedParts, ErrIncomplete)
	}
	if _, err := q.CompleteUploadSession(ctx, sqlcgen.CompleteUploadSessionParams{
		FileID:   dbtypes.UUID(fileID),
		UploadID: dbtypes.UUID(uploadID),
		UserID:   userID,
	}); err != nil {
		return nil, fmt.Errorf("complete upload session: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit upload completion: %w", err)
	}
	if replacedID != nil {
		if s.catalogInvalidator != nil {
			s.catalogInvalidator.InvalidateFiles(ctx, userID, *replacedID)
		}
	}
	return file, nil
}

// prepareConflictPolicy resolves the destination name of a completing session
// inside tx and returns the id of a file whose catalog cache entry must be
// dropped, if any. It first takes a transaction-scoped advisory lock on the
// destination (owner plus parent folder), so concurrent completions into the same
// folder cannot race on the same name, and then applies the policy: fail returns
// ErrNameConflict when an active entry occupies the name; replace requires that
// entry to be a regular file, marks it deletion_pending, revokes its shares and
// returns its id; rename stores the first free "name (n)" variant on the session.
// A policy outside those three is reported as ErrUnsupportedConflictPolicy. The
// caller must already hold the session row lock.
func prepareConflictPolicy(ctx context.Context, tx pgx.Tx, session *sqlcgen.UploadSession) (*uuid.UUID, error) {
	if session == nil {
		return nil, ErrInvalidInput
	}
	queries := sqlcgen.New(tx)
	if err := queries.AcquireAdvisoryTransactionLock(ctx, uploadDestinationLockID(session)); err != nil {
		return nil, fmt.Errorf("lock upload destination: %w", err)
	}

	existing, err := queries.LockUploadDestinationConflict(ctx, sqlcgen.LockUploadDestinationConflictParams{
		UserID: session.UserID, ParentID: session.ParentID, Name: session.Name,
	})
	hasConflict := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("check upload destination conflict: %w", err)
	}

	switch session.ConflictPolicy {
	case sqlcgen.NameConflictPolicyFail:
		if hasConflict {
			return nil, ErrNameConflict
		}
		return nil, nil
	case sqlcgen.NameConflictPolicyReplace:
		if !hasConflict {
			return nil, nil
		}
		if existing.Kind != sqlcgen.FileKindFile {
			return nil, ErrNameConflict
		}
		existingID, ok := dbtypes.GoogleUUID(existing.ID)
		if !ok {
			return nil, ErrNameConflict
		}
		count, err := queries.MarkActiveFileDeletionPendingForReplace(ctx, sqlcgen.MarkActiveFileDeletionPendingForReplaceParams{
			FileID: dbtypes.UUID(existingID), UserID: session.UserID,
		})
		if err != nil {
			return nil, fmt.Errorf("mark replaced file for cleanup: %w", err)
		}
		if count != 1 {
			return nil, ErrNameConflict
		}
		if err := queries.RevokeActiveSharesForFile(ctx, sqlcgen.RevokeActiveSharesForFileParams{
			FileID: dbtypes.UUID(existingID), UserID: session.UserID,
		}); err != nil {
			return nil, fmt.Errorf("revoke replaced file shares: %w", err)
		}
		return &existingID, nil
	case sqlcgen.NameConflictPolicyRename:
		if !hasConflict {
			return nil, nil
		}
		name, err := nextAvailableUploadName(ctx, queries, session)
		if err != nil {
			return nil, err
		}
		count, err := queries.RenameUploadSession(ctx, sqlcgen.RenameUploadSessionParams{
			Name: name, UploadID: session.ID, UserID: session.UserID,
		})
		if err != nil {
			return nil, fmt.Errorf("rename upload destination: %w", err)
		}
		if count != 1 {
			return nil, ErrInvalidState
		}
		session.Name = name
		return nil, nil
	default:
		return nil, ErrUnsupportedConflictPolicy
	}
}

// nextAvailableUploadName returns the first "name (n).ext" variant that no active
// entry uses in the session's destination folder. Numbering starts at 1 and gives
// up after 10000 candidates with ErrNameConflict. The caller must hold the
// destination advisory lock, because the list of used names is read without a lock
// of its own.
func nextAvailableUploadName(ctx context.Context, queries *sqlcgen.Queries, session *sqlcgen.UploadSession) (string, error) {
	names, err := queries.ListActiveNames(ctx, sqlcgen.ListActiveNamesParams{
		UserID: session.UserID, ParentID: session.ParentID,
	})
	if err != nil {
		return "", fmt.Errorf("list upload destination names: %w", err)
	}
	used := make(map[string]struct{}, len(names))
	for _, normalized := range names {
		used[normalized] = struct{}{}
	}

	base, extension := splitUploadName(session.Name)
	for sequence := 1; sequence <= 10000; sequence++ {
		candidate := base + fmt.Sprintf(" (%d)", sequence) + extension
		if _, exists := used[candidate]; !exists {
			return candidate, nil
		}
	}
	return "", ErrNameConflict
}

// splitUploadName splits name into the stem and the extension including its dot.
// A dot at the start or at the very end is treated as part of the stem, so
// ".bashrc" and "report." have no extension.
func splitUploadName(name string) (string, string) {
	index := strings.LastIndex(name, ".")
	if index <= 0 || index == len(name)-1 {
		return name, ""
	}
	return name[:index], name[index:]
}

// uploadDestinationLockID derives the advisory transaction lock key for a session
// destination from its owner and parent folder. The key is the first 8 bytes of a
// SHA-256 over a fixed domain prefix, the owner id and the parent id (16 zero
// bytes for the root), reinterpreted as the signed big-endian int64 that
// pg_advisory_xact_lock takes. Different folders of one user therefore lock
// independently, while the same folder always maps to the same key.
func uploadDestinationLockID(session *sqlcgen.UploadSession) int64 {
	input := make([]byte, 0, 8+16+len("teldrive/upload-destination/"))
	input = append(input, []byte("teldrive/upload-destination/")...)
	var user [8]byte
	binary.BigEndian.PutUint64(user[:], uint64(session.UserID))
	input = append(input, user[:]...)
	if session.ParentID.Valid {
		input = append(input, session.ParentID.Bytes[:]...)
	} else {
		input = append(input, make([]byte, 16)...)
	}
	digest := sha256.Sum256(input)
	return int64(binary.BigEndian.Uint64(digest[:8]))
}

// Abort moves an open or completing session to the aborted state and returns the
// resulting row. Aborting a session that is already aborted or expired is a no-op
// that returns the current row, which makes retries safe, while a completed
// session yields ErrInvalidState.
//
// Aborting rewrites only upload_sessions.state and leaves upload_parts untouched,
// so the session's part leases are not revoked; the same holds for the expiry the
// upload cleanup sweep applies. StorePart, RenewPart and FailPart authorize on the
// lease token alone and never re-check the session state, so the holder of a part
// lease can still renew it, or mark its part stored, while the row stays uploading
// under its token; a lapsed lease does not by itself close that window. The upload
// cleanup sweep later finds the parts of aborted and expired sessions by
// message_id and removes both the stored objects and the part rows.
func (s *Service) Abort(ctx context.Context, userID int64, uploadID uuid.UUID) (*sqlcgen.UploadSession, error) {
	if userID <= 0 {
		return nil, ErrInvalidInput
	}
	session, err := s.Get(ctx, userID, uploadID)
	if err != nil {
		return nil, err
	}
	switch session.State {
	case sqlcgen.UploadStateAborted, sqlcgen.UploadStateExpired:
		return session, nil
	case sqlcgen.UploadStateCompleted:
		return nil, ErrInvalidState
	}
	aborted, err := s.queries.AbortUploadSession(ctx, sqlcgen.AbortUploadSessionParams{
		UploadID: dbtypes.UUID(uploadID),
		UserID:   userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidState
	}
	if err != nil {
		return nil, fmt.Errorf("abort upload: %w", err)
	}
	return aborted, nil
}

// validatePartShape checks that partNo and plainSize fit the session's geometry
// before a lease is granted. partNo must be positive and at most maxUploadParts, and
// plainSize must fit the part. When ExpectedSize is -1 the total is unknown, so the
// only further check is that plainSize is greater than zero and at most PartSize: a
// part that is short without being the final one is accepted here, because the rule
// that only the final part may be short is enforced later, at completion, by
// CountInvalidOpenEndedUploadParts. With a known size, partNo must address a byte
// range inside ExpectedSize and plainSize must equal PartSize, or the remainder for
// the last part. A session of zero bytes rejects every part with ErrInvalidInput.
func validatePartShape(session *sqlcgen.UploadSession, partNo int32, plainSize int64) error {
	if session.PartSize <= 0 || partNo < 1 || int64(partNo) > maxUploadParts {
		return ErrInvalidInput
	}
	if session.ExpectedSize < 0 {
		if plainSize <= 0 || plainSize > session.PartSize {
			return ErrInvalidInput
		}
		return nil
	}
	if session.ExpectedSize == 0 {
		return ErrInvalidInput
	}
	// Comparing through the last valid part number instead of multiplying keeps a
	// huge part number from wrapping the product into a valid-looking offset.
	if int64(partNo-1) > (session.ExpectedSize-1)/session.PartSize {
		return ErrInvalidInput
	}
	offset := int64(partNo-1) * session.PartSize
	expected := session.PartSize
	if remaining := session.ExpectedSize - offset; remaining < expected {
		expected = remaining
	}
	if plainSize != expected {
		return ErrInvalidInput
	}
	return nil
}

// validateStoredParts verifies inside tx that the session's stored parts form a
// complete, gap-free run and returns the session's total plaintext size. With a
// known ExpectedSize every part must be stored, the stored plain sizes must add up
// to exactly that size, and the part numbers must run from 1 to the expected
// count; with an unknown size the stored parts must still be consecutive from 1
// and every non-final part must be exactly one part size. Any violation is
// reported as ErrIncomplete.
func validateStoredParts(ctx context.Context, tx pgx.Tx, session *sqlcgen.UploadSession) (int64, error) {
	queries := sqlcgen.New(tx)
	summary, err := queries.GetAllUploadPartSummary(ctx, session.ID)
	if err != nil {
		return 0, fmt.Errorf("summarize upload parts: %w", err)
	}
	if session.ExpectedSize >= 0 {
		expected := expectedPartCount(session.ExpectedSize, session.PartSize)
		if summary.TotalParts != expected || summary.StoredParts != expected || summary.StoredPlainSize != session.ExpectedSize {
			return 0, ErrIncomplete
		}
		if expected > 0 && (summary.MinPartNo != 1 || int64(summary.MaxPartNo) != expected) {
			return 0, ErrIncomplete
		}
		return session.ExpectedSize, nil
	}
	if summary.TotalParts != summary.StoredParts {
		return 0, ErrIncomplete
	}
	if summary.StoredParts > 0 && (summary.MinPartNo != 1 || int64(summary.MaxPartNo) != summary.StoredParts) {
		return 0, ErrIncomplete
	}
	invalidParts, err := queries.CountInvalidOpenEndedUploadParts(ctx, sqlcgen.CountInvalidOpenEndedUploadPartsParams{
		UploadID: session.ID,
		PartSize: session.PartSize,
	})
	if err != nil {
		return 0, fmt.Errorf("validate open-ended upload parts: %w", err)
	}
	if invalidParts != 0 {
		return 0, ErrIncomplete
	}
	return summary.StoredPlainSize, nil
}

// expectedPartCount returns how many partSize parts cover size bytes, rounding up.
// A zero-byte upload needs no parts at all. size must be non-negative and partSize
// positive; callers finalize an open-ended session's size before using it.
func expectedPartCount(size, partSize int64) int64 {
	if size == 0 {
		return 0
	}
	return (size + partSize - 1) / partSize
}

// normalizeExpectedHash lowercases the algorithm, requires it to be the only
// supported value treehash.TypeBlake3, and normalizes the digest with
// normalizeDigest. An unknown algorithm or a malformed digest yields
// ErrInvalidInput, and the returned pair is what Create persists.
func normalizeExpectedHash(algorithm, value string) (string, string, error) {
	algorithm = strings.ToLower(strings.TrimSpace(algorithm))
	if algorithm != string(treehash.TypeBlake3) {
		return "", "", ErrInvalidInput
	}
	normalized, err := normalizeDigest(value)
	if err != nil {
		return "", "", err
	}
	return algorithm, normalized, nil
}

// normalizeDigest lowercases and trims a hex digest, requires it to decode to
// exactly treehash.DigestSize bytes, and returns the normalized text; anything
// else yields ErrInvalidInput. The normalized form is the one compared against
// stored checksums and persisted in the database.
func normalizeDigest(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	digest, err := hex.DecodeString(value)
	if err != nil || len(digest) != treehash.DigestSize {
		return "", ErrInvalidInput
	}
	return value, nil
}

// optionalTextEqual reports whether a nullable stored text equals an optional
// expected value: a nil expectation matches only SQL NULL, while a non-nil one
// requires a valid, exactly equal text. Both sides are assumed to be normalized
// already, so the comparison is case-sensitive.
func optionalTextEqual(stored pgtype.Text, expected *string) bool {
	if expected == nil {
		return !stored.Valid
	}
	return stored.Valid && stored.String == *expected
}
