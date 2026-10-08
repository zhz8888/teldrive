// Package shares implements public share links and per-file access grants.
//
// A share is addressed by a high-entropy token; only its SHA-256 hash is
// persisted, so a leaked database cannot be used to redeem live shares. A
// share may additionally carry a bcrypt password, an expiry timestamp and a
// download quota. Callers resolve shares through Resolve, ResolveFile or the
// Reserve* methods, which charge the quota. Failures surface as the package
// sentinel errors below, or as the underlying catalog error, and must be tested
// with errors.Is.
package shares

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/zhz8888/teldrive/v2/internal/catalog"
	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/dbtypes"
	"github.com/zhz8888/teldrive/v2/internal/throttle"
)

var (
	// ErrInvalidInput reports a request that failed local validation, such as a
	// non-positive owner, a nil file ID, an unsupported permission, or an
	// expiry or quota that is not usable. Callers must test it with errors.Is.
	ErrInvalidInput = errors.New("invalid share input")
	// ErrNotFound reports that the addressed share, grant, file or user does
	// not exist, is no longer active, or is not visible to the caller; revoked
	// shares are deliberately reported this way. Callers must test it with
	// errors.Is.
	ErrNotFound = errors.New("share not found")
	// ErrExpired reports a token that is unknown, revoked, past its expiry, or
	// out of downloads. Unknown and dead tokens share this error so that live
	// tokens cannot be discovered by probing. Callers must test it with
	// errors.Is.
	ErrExpired = errors.New("share expired or exhausted")
	// ErrPasswordNeeded reports that the share is password protected and the
	// request carried no password. Callers must test it with errors.Is.
	ErrPasswordNeeded = errors.New("share password is required")
	// ErrInvalidPassword reports that the supplied password does not match the
	// stored bcrypt hash. Repeated failures for one share are counted and, past
	// the threshold, answered with ErrTooManyAttempts instead. Callers must test
	// it with errors.Is.
	ErrInvalidPassword = errors.New("share password is invalid")
	// ErrTooManyAttempts reports that a share has seen too many wrong passwords
	// in a row and is refusing further guesses for a while. It maps to HTTP 429,
	// so a client can retry the same password later instead of treating the
	// share as broken. Callers must test it with errors.Is.
	ErrTooManyAttempts = errors.New("share password attempts are throttled")
	// ErrForbidden reports that the caller is authenticated but holds neither
	// ownership nor an active grant with sufficient permission for the file.
	// Callers must test it with errors.Is.
	ErrForbidden = errors.New("share permission denied")
)

// Created is the result of minting a share. Because only the token hash is
// persisted, the plaintext token and URL in it can never be recovered later.
type Created struct {
	// Row is the share record as written to file_shares.
	Row *sqlcgen.FileShare
	// Token is the plaintext bearer secret, returned to the creator once and
	// never stored; the database keeps only a short display prefix and the hash.
	Token string
	// PublicURL is the share path relative to the API host, not an absolute URL.
	PublicURL url.URL
}

// Public is a share resolved from a token together with the file it exposes.
// Both rows belong to the share owner, so callers must only surface them to the
// extent the share permission allows.
type Public struct {
	// Share is the active share row matched by the token, including the owner
	// ID that downstream storage access needs.
	Share *sqlcgen.GetActiveShareByTokenHashRow
	// File is the shared file, or the folder acting as the share root.
	File *sqlcgen.File
}

// SharedWithMe pairs a file another owner granted to the caller with the
// permission that grant confers.
type SharedWithMe struct {
	// File is the granted file; it was active when the list was read.
	File *sqlcgen.File
	// Permission is the granted level, read or edit.
	Permission sqlcgen.SharePermission
	// GrantUpdatedAt and GrantID are the sort key of the entry, which is the
	// grant's own timestamp and id rather than the file's. They are returned so a
	// caller can build the cursor of the next page; they are not part of the API
	// payload.
	GrantUpdatedAt time.Time
	// GrantID is the grant id half of that cursor and breaks ties between grants
	// written at the same instant; like GrantUpdatedAt it is not part of the API
	// payload.
	GrantID uuid.UUID
}

// ListSharedInput selects one page of the files an owner has shared. Pages are
// cut on the (updated_at, id) pair of the file and returned newest first.
type ListSharedInput struct {
	// OwnerID is the user whose shared files are listed; it must be positive.
	OwnerID int64
	// AfterUpdatedAt is the file timestamp cursor from the previous page; it only
	// takes effect together with AfterID.
	AfterUpdatedAt *time.Time
	// AfterID is the file ID cursor from the previous page.
	AfterID *uuid.UUID
	// Limit caps the page size; it must be positive and is clamped by the caller.
	Limit int32
}

// ListSharedWithMeInput selects one page of the grants that point at the files
// the caller can reach. Pages are cut on the (updated_at, id) pair of the grant
// and returned newest first.
type ListSharedWithMeInput struct {
	// GranteeID is the user the files were granted to; it must be positive.
	GranteeID int64
	// AfterGrantUpdatedAt is the grant timestamp cursor from the previous page; it
	// only takes effect together with AfterGrantID.
	AfterGrantUpdatedAt *time.Time
	// AfterGrantID is the grant ID cursor from the previous page.
	AfterGrantID *uuid.UUID
	// Limit caps the page size; it must be positive and is clamped by the caller.
	Limit int32
}

// CreateInput describes a new public share. OwnerID and FileID are required and
// FileID must name an active file owned by OwnerID.
type CreateInput struct {
	// OwnerID is the authenticated user creating the share; it must be positive.
	OwnerID int64
	// FileID is the active file or folder to expose.
	FileID uuid.UUID
	// Password protects the share when non-nil; the value is trimmed, must not
	// be blank, and is stored as a bcrypt hash.
	Password *string
	// ExpiresAt, when non-nil, must be strictly after the service clock.
	ExpiresAt *time.Time
	// MaxDownloads caps how many downloads the share allows; when non-nil it
	// must be positive. Every call to a Reserve method charges it.
	MaxDownloads *int64
	// Permission is the access level for the share; the empty value defaults to
	// read.
	Permission sqlcgen.SharePermission
}

// ListInput selects the shares of one file for its owner. Pages are cut on the
// (created_at, id) pair and returned newest first.
type ListInput struct {
	// OwnerID is the user whose shares are listed; it must be positive.
	OwnerID int64
	// FileID is the file whose shares are listed; it must be non-nil and owned
	// by the caller.
	FileID uuid.UUID
	// AfterCreatedAt is the creation-time cursor from the previous page; it only
	// takes effect together with AfterID.
	AfterCreatedAt *time.Time
	// AfterID is the share ID cursor from the previous page.
	AfterID *uuid.UUID
	// Limit caps the page size; zero or negative falls back to 100 and values
	// above 200 are clamped to 200.
	Limit int32
}

// UpdateInput patches an existing share. At least one change must be present,
// and a value and its Clear flag are mutually exclusive.
type UpdateInput struct {
	// OwnerID is the share owner; only their shares can be updated.
	OwnerID int64
	// ShareID identifies the share to update.
	ShareID uuid.UUID
	// Password replaces the share password; the value is trimmed, must not be
	// blank, and is stored as a bcrypt hash. It cannot be combined with
	// ClearPassword.
	Password *string
	// ClearPassword removes password protection; it cannot be combined with
	// Password.
	ClearPassword bool
	// ExpiresAt sets a new expiry that must be strictly after the service clock;
	// it cannot be combined with ClearExpiresAt.
	ExpiresAt *time.Time
	// ClearExpiresAt removes the expiry so the share never lapses.
	ClearExpiresAt bool
	// MaxDownloads sets a new positive quota that may not be lower than the
	// download count already recorded; it cannot be combined with
	// ClearMaxDownloads.
	MaxDownloads *int64
	// ClearMaxDownloads removes the download quota.
	ClearMaxDownloads bool
	// Permission changes the access level; when non-nil it must be read or edit.
	Permission *sqlcgen.SharePermission
}

// GrantCreateInput describes a per-user access grant on one file. OwnerID and
// GranteeID must differ, and the grantee must be an existing, enabled user.
type GrantCreateInput struct {
	// OwnerID is the user granting access; it must be positive.
	OwnerID int64
	// FileID is the granted file or folder, owned by OwnerID.
	FileID uuid.UUID
	// GranteeID is the user receiving access.
	GranteeID int64
	// Permission is the granted level; the empty value defaults to read.
	Permission sqlcgen.SharePermission
	// ExpiresAt, when non-nil, must be strictly after the service clock.
	ExpiresAt *time.Time
}

// GrantUpdateInput patches an access grant. At least one of Permission,
// ExpiresAt and ClearExpiresAt must be set.
type GrantUpdateInput struct {
	// OwnerID is the grant owner; only their grants can be updated.
	OwnerID int64
	// GrantID identifies the grant to update.
	GrantID uuid.UUID
	// Permission replaces the granted level; when non-nil it must be read or
	// edit.
	Permission *sqlcgen.SharePermission
	// ExpiresAt sets a new expiry that must be strictly after the service clock;
	// it cannot be combined with ClearExpiresAt.
	ExpiresAt *time.Time
	// ClearExpiresAt removes the expiry so the grant never lapses.
	ClearExpiresAt bool
}

// Access is the effective access a caller has to one file, derived either from
// ownership or from a grant on an ancestor folder.
type Access struct {
	// OwnerID is the user who owns the file and its storage.
	OwnerID int64
	// RootFileID is where the access originates: the file itself for ownership,
	// or the granted ancestor for a grant.
	RootFileID uuid.UUID
	// Permission is the effective level; owned files always resolve to edit.
	Permission sqlcgen.SharePermission
	// Owned reports whether the caller owns the file; when true, RootFileID
	// equals the queried file.
	Owned bool
}

// PublicListInput lists the entries of a shared folder addressed through a
// public token, optionally password protected.
type PublicListInput struct {
	// Token is the share secret from the request; it is hashed for lookup.
	Token string
	// Password is the share password, empty when the share has none.
	Password string
	// Path is a slash-separated path relative to the share root; empty lists the
	// root folder itself.
	Path string
	// Search filters entries by name.
	Search string
	// AfterName is the name cursor from the previous page; it only takes effect
	// together with AfterID.
	AfterName string
	// AfterID is the file ID cursor from the previous page.
	AfterID *uuid.UUID
	// Limit caps the page size; the catalog defaults it to 100 and clamps it
	// to 500.
	Limit int32
}

// Service implements share and grant operations on top of the catalog. It holds
// no mutable state and is safe for concurrent use; all persistent state lives
// in PostgreSQL.
type Service struct {
	// queries runs the share and grant statements against the pool passed to
	// NewService.
	queries *sqlcgen.Queries
	// catalog reads file metadata; every lookup is performed with the share
	// owner's identity, never with the caller's.
	catalog *catalog.Service
	// random is the entropy source for share tokens; tests replace it to make
	// token generation deterministic.
	random io.Reader
	// now is the service clock, read on every validation and expiry check so
	// tests can freeze time.
	now func() time.Time
	// attempts throttles password guessing per share, so a token holder cannot
	// try passwords at the speed bcrypt allows. It is built on first use, which
	// keeps a service assembled by a test literal working.
	attempts *throttle.Limiter
	// attemptsOnce guards the one-time construction of attempts.
	attemptsOnce sync.Once
}

// attemptLimiter returns the per-share password throttle, building it on first
// use.
func (s *Service) attemptLimiter() *throttle.Limiter {
	s.attemptsOnce.Do(func() {
		if s.attempts == nil {
			s.attempts = throttle.New(sharePasswordFailures, sharePasswordBlock, sharePasswordBlockMax, sharePasswordKeys)
		}
	})
	return s.attempts
}

const (
	// maxSharePasswordLength is the longest password bcrypt accepts. Anything
	// longer is rejected during validation instead of failing hash generation.
	maxSharePasswordLength = 72
	// sharePasswordFailures is how many wrong passwords one share may see before
	// further guesses are refused.
	sharePasswordFailures = 5
	// sharePasswordBlock is the first block applied to a share that ran out of
	// attempts; it doubles with every further wrong password up to
	// sharePasswordBlockMax.
	sharePasswordBlock = 30 * time.Second
	// sharePasswordBlockMax caps one block, so a share stays reachable for its
	// owner without a restart or background job.
	sharePasswordBlockMax = 15 * time.Minute
	// sharePasswordKeys bounds how many shares the throttle remembers.
	sharePasswordKeys = 4096
)

// NewService builds a share service on the given connection pool. It returns
// ErrInvalidInput when either dependency is nil and does not ping the pool, so
// connectivity problems only surface on first use.
func NewService(pool *pgxpool.Pool, catalogService *catalog.Service) (*Service, error) {
	if pool == nil || catalogService == nil {
		return nil, ErrInvalidInput
	}
	return &Service{queries: sqlcgen.New(pool), catalog: catalogService, random: rand.Reader, now: time.Now}, nil
}

// normalizePermission substitutes read for the empty permission and returns any
// other value unchanged, including invalid ones.
func normalizePermission(permission sqlcgen.SharePermission) sqlcgen.SharePermission {
	if permission == "" {
		return sqlcgen.SharePermissionRead
	}
	return permission
}

// validPermission reports whether permission is one of the two levels the
// schema accepts; the empty value is not valid and must be normalized first.
func validPermission(permission sqlcgen.SharePermission) bool {
	return permission == sqlcgen.SharePermissionRead || permission == sqlcgen.SharePermissionEdit
}

// optionalPermission converts an optional permission into its nullable SQL
// form; nil becomes an invalid null, which leaves the stored column untouched.
func optionalPermission(permission *sqlcgen.SharePermission) sqlcgen.NullSharePermission {
	if permission == nil {
		return sqlcgen.NullSharePermission{}
	}
	return sqlcgen.NullSharePermission{SharePermission: *permission, Valid: true}
}

// Create mints a share for one of the caller's active files and returns the
// plaintext token exactly once; the row keeps only its hash and a 16-character
// display prefix. The optional password is trimmed and stored as a bcrypt hash,
// ExpiresAt must be in the future, and MaxDownloads must be positive. It
// reports ErrInvalidInput for failed validation, ErrNotFound when the file is
// not an active file of the caller — a trashed file is answered like a foreign
// one so the two cannot be told apart — the catalog error when the file cannot
// be read for the owner, and a wrapped error when hashing or the insert fails.
func (s *Service) Create(ctx context.Context, in CreateInput) (*Created, error) {
	in.Permission = normalizePermission(in.Permission)
	if in.OwnerID <= 0 || in.FileID == uuid.Nil || !validPermission(in.Permission) || (in.ExpiresAt != nil && !in.ExpiresAt.After(s.now())) || (in.MaxDownloads != nil && *in.MaxDownloads <= 0) {
		return nil, ErrInvalidInput
	}
	file, err := s.catalog.Get(ctx, in.OwnerID, in.FileID)
	if err != nil {
		return nil, err
	}
	if file.Status != sqlcgen.FileStatusActive {
		return nil, ErrNotFound
	}
	secret, hash, err := s.newToken()
	if err != nil {
		return nil, err
	}
	var passwordHash *string
	if in.Password != nil {
		hash, err := hashSharePassword(*in.Password)
		if err != nil {
			return nil, err
		}
		passwordHash = hash
	}
	row, err := s.queries.CreateFileShare(ctx, sqlcgen.CreateFileShareParams{
		ID: dbtypes.UUID(uuid.New()), FileID: dbtypes.UUID(in.FileID), OwnerID: in.OwnerID,
		TokenHash: hash, PasswordHash: dbtypes.OptionalText(passwordHash),
		ExpiresAt: dbtypes.OptionalTime(in.ExpiresAt), MaxDownloads: dbtypes.OptionalInt8(in.MaxDownloads),
		Permission: in.Permission,
	})
	if err != nil {
		return nil, fmt.Errorf("create file share: %w", err)
	}
	publicURL := url.URL{Path: "/share/" + secret}
	return &Created{Row: row, Token: secret, PublicURL: publicURL}, nil
}

// List returns one page of the owner's shares for a single file, newest first.
// The caller must own the file, and the cursor is the (AfterCreatedAt, AfterID)
// pair taken from the previous page. Limit defaults to 100 and is clamped to
// 200; ErrInvalidInput is returned when OwnerID or FileID is unset.
func (s *Service) List(ctx context.Context, in ListInput) ([]*sqlcgen.FileShare, error) {
	if in.OwnerID <= 0 || in.FileID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	if _, err := s.catalog.Get(ctx, in.OwnerID, in.FileID); err != nil {
		return nil, err
	}
	if in.Limit <= 0 {
		in.Limit = 100
	}
	if in.Limit > 200 {
		in.Limit = 200
	}
	rows, err := s.queries.ListFileShares(ctx, sqlcgen.ListFileSharesParams{
		OwnerID: in.OwnerID, FileID: dbtypes.UUID(in.FileID),
		AfterCreatedAt: dbtypes.OptionalTime(in.AfterCreatedAt), AfterID: dbtypes.OptionalUUID(in.AfterID),
		PageSize: in.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list file shares: %w", err)
	}
	return rows, nil
}

// Update patches a live share owned by the caller and returns the updated row.
// At least one change is required and a value cannot be combined with its Clear
// flag; MaxDownloads may not be lowered below the downloads already recorded.
// Unknown or revoked shares are reported as ErrNotFound, as is an update that
// loses a race with a concurrent download reservation raising the count past
// the new quota.
func (s *Service) Update(ctx context.Context, in UpdateInput) (*sqlcgen.FileShare, error) {
	if in.OwnerID <= 0 || in.ShareID == uuid.Nil ||
		(in.Password != nil && in.ClearPassword) ||
		(in.ExpiresAt != nil && in.ClearExpiresAt) ||
		(in.MaxDownloads != nil && in.ClearMaxDownloads) {
		return nil, ErrInvalidInput
	}
	if in.Password == nil && !in.ClearPassword && in.ExpiresAt == nil && !in.ClearExpiresAt && in.MaxDownloads == nil && !in.ClearMaxDownloads && in.Permission == nil {
		return nil, ErrInvalidInput
	}
	if in.Permission != nil && !validPermission(*in.Permission) {
		return nil, ErrInvalidInput
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(s.now()) {
		return nil, ErrInvalidInput
	}
	if in.MaxDownloads != nil && *in.MaxDownloads <= 0 {
		return nil, ErrInvalidInput
	}
	existing, err := s.queries.GetFileShareForOwner(ctx, sqlcgen.GetFileShareForOwnerParams{
		ID: dbtypes.UUID(in.ShareID), OwnerID: in.OwnerID,
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && existing.RevokedAt.Valid) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get share for update: %w", err)
	}
	if in.MaxDownloads != nil && *in.MaxDownloads < existing.DownloadCount {
		return nil, ErrInvalidInput
	}
	var passwordHash *string
	if in.Password != nil {
		hash, err := hashSharePassword(*in.Password)
		if err != nil {
			return nil, err
		}
		passwordHash = hash
	}
	updated, err := s.queries.UpdateFileShare(ctx, sqlcgen.UpdateFileShareParams{
		ClearPassword: in.ClearPassword, PasswordHash: dbtypes.OptionalText(passwordHash),
		ClearExpiresAt: in.ClearExpiresAt, ExpiresAt: dbtypes.OptionalTime(in.ExpiresAt),
		ClearMaxDownloads: in.ClearMaxDownloads, MaxDownloads: dbtypes.OptionalInt8(in.MaxDownloads),
		Permission: optionalPermission(in.Permission), ID: dbtypes.UUID(in.ShareID), OwnerID: in.OwnerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update share: %w", err)
	}
	return updated, nil
}

// ListPublicFiles lists a folder exposed by a share after resolving the token
// and password. It returns ErrInvalidInput when the share does not point at an
// active folder, forwards the resolve errors (ErrExpired, ErrPasswordNeeded,
// ErrInvalidPassword), and otherwise delegates to the catalog with the text
// search and name cursor as given.
func (s *Service) ListPublicFiles(ctx context.Context, in PublicListInput) ([]*sqlcgen.File, error) {
	resolved, err := s.Resolve(ctx, in.Token, in.Password)
	if err != nil {
		return nil, err
	}
	if resolved.File.Kind != sqlcgen.FileKindFolder || resolved.File.Status != sqlcgen.FileStatusActive {
		return nil, ErrInvalidInput
	}
	rootID, ok := dbtypes.GoogleUUID(resolved.File.ID)
	if !ok {
		return nil, ErrNotFound
	}
	parentID, err := s.catalog.ResolveFolderPath(ctx, resolved.Share.OwnerID, &rootID, in.Path)
	if err != nil {
		return nil, err
	}
	return s.catalog.List(ctx, catalog.ListInput{
		UserID: resolved.Share.OwnerID, ParentID: parentID, Status: sqlcgen.FileStatusActive,
		Search: in.Search, AfterName: in.AfterName, AfterID: in.AfterID, Limit: in.Limit,
	})
}

// Revoke stamps revoked_at on one of the caller's shares, which makes every
// later resolve of its token fail with ErrExpired. It returns ErrNotFound when
// the share is unknown, owned by somebody else, or already revoked.
func (s *Service) Revoke(ctx context.Context, ownerID int64, shareID uuid.UUID) error {
	if ownerID <= 0 || shareID == uuid.Nil {
		return ErrInvalidInput
	}
	count, err := s.queries.RevokeFileShare(ctx, sqlcgen.RevokeFileShareParams{ID: dbtypes.UUID(shareID), OwnerID: ownerID})
	if err != nil {
		return fmt.Errorf("revoke share: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

// Resolve validates a share token and password and returns the share with the
// file it exposes. It does not consume the download quota. Unknown, revoked,
// expired, exhausted and deleted-file tokens all yield ErrExpired, a missing
// password yields ErrPasswordNeeded, a wrong one ErrInvalidPassword, and a
// catalog failure is returned unchanged.
func (s *Service) Resolve(ctx context.Context, token, password string) (*Public, error) {
	row, err := s.resolveRow(ctx, token, password)
	if err != nil {
		return nil, err
	}
	fileID, ok := dbtypes.GoogleUUID(row.FileID)
	if !ok {
		return nil, ErrNotFound
	}
	file, err := s.catalog.Get(ctx, row.OwnerID, fileID)
	if err != nil {
		return nil, err
	}
	return &Public{Share: row, File: file}, nil
}

// ResolveFile resolves a token and then requires fileID to be either the share
// root or a descendant of it. Descendants are recognised by walking the
// target's ancestor chain up to the root — that walk ignores the status of the
// folders in between — and the target itself must still be active; a non-folder
// share exposes only its root. Rejections all surface as a not-found error
// (this package's ErrNotFound, or the catalog's for a file the owner does not
// have) rather than a distinct error, so that foreign IDs cannot be probed.
func (s *Service) ResolveFile(ctx context.Context, token, password string, fileID uuid.UUID) (*Public, error) {
	if fileID == uuid.Nil {
		return nil, ErrNotFound
	}
	resolved, err := s.Resolve(ctx, token, password)
	if err != nil {
		return nil, err
	}
	rootID, ok := dbtypes.GoogleUUID(resolved.File.ID)
	if !ok {
		return nil, ErrNotFound
	}
	if rootID == fileID {
		return resolved, nil
	}
	if resolved.File.Kind != sqlcgen.FileKindFolder {
		return nil, ErrNotFound
	}
	ids, err := s.queries.ListFileAncestorIDs(ctx, sqlcgen.ListFileAncestorIDsParams{FileID: dbtypes.UUID(fileID), UserID: resolved.Share.OwnerID})
	if err != nil {
		return nil, fmt.Errorf("list target ancestors: %w", err)
	}
	allowed := false
	for _, id := range ids {
		if value, ok := dbtypes.GoogleUUID(id); ok && value == rootID {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, ErrNotFound
	}
	file, err := s.catalog.Get(ctx, resolved.Share.OwnerID, fileID)
	if err != nil {
		return nil, err
	}
	if file.Status != sqlcgen.FileStatusActive {
		return nil, ErrNotFound
	}
	return &Public{Share: resolved.Share, File: file}, nil
}

// ReserveFileDownload is ReserveDownload for a single file below a share: it
// resolves that file and then atomically charges one download against the share
// quota. The same caveat applies, so a stream that fails after this call still
// consumes the reservation, and ErrExpired is returned when the quota, expiry
// or revocation state changed since the file was resolved.
func (s *Service) ReserveFileDownload(ctx context.Context, token, password string, fileID uuid.UUID) (*Public, error) {
	resolved, err := s.ResolveFile(ctx, token, password, fileID)
	if err != nil {
		return nil, err
	}
	return s.ReserveResolvedDownload(ctx, resolved)
}

// ReserveDownload atomically consumes one allowed download before bytes are
// exposed. A failed stream consumes the reservation, preventing concurrent
// requests from exceeding max_downloads.
func (s *Service) ReserveDownload(ctx context.Context, token, password string) (*Public, error) {
	resolved, err := s.Resolve(ctx, token, password)
	if err != nil {
		return nil, err
	}
	return s.ReserveResolvedDownload(ctx, resolved)
}

// ReserveResolvedDownload charges one download against a share that the caller
// resolved earlier in the same request. It exists so a caller that resolved the
// share for its response headers, and therefore already paid for the password
// check, does not run bcrypt a second time: a password-protected share would
// otherwise cost two derivations per request, which is what makes guessing and
// CPU exhaustion cheap for whoever holds the link.
func (s *Service) ReserveResolvedDownload(ctx context.Context, resolved *Public) (*Public, error) {
	if resolved == nil || resolved.Share == nil {
		return nil, ErrNotFound
	}
	if _, err := s.queries.IncrementShareDownloadCount(ctx, resolved.Share.ID); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrExpired
	} else if err != nil {
		return nil, fmt.Errorf("reserve share download: %w", err)
	}
	return resolved, nil
}

// resolveRow looks a share up by token hash and enforces its password. The SQL
// query already excludes revoked, expired, exhausted and inactive rows, so the
// Go-side expiry and quota checks only repeat those predicates against s.now()
// as a defense in depth. A blank token yields ErrNotFound, and any other lookup
// miss yields ErrExpired rather than ErrNotFound so that dead and unknown
// tokens are indistinguishable. The password check goes through bcrypt, which
// compares the digest in constant time; the raw password is neither stored nor
// logged. Wrong passwords are counted per share, so guessing is throttled past
// sharePasswordFailures attempts instead of being limited only by bcrypt's work
// factor, which also caps how much CPU one token holder can spend.
func (s *Service) resolveRow(ctx context.Context, token, password string) (*sqlcgen.GetActiveShareByTokenHashRow, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrNotFound
	}
	hash := tokenHash(token)
	row, err := s.queries.GetActiveShareByTokenHash(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrExpired
	}
	if err != nil {
		return nil, fmt.Errorf("resolve share: %w", err)
	}
	if row.ExpiresAt.Valid && !row.ExpiresAt.Time.After(s.now()) {
		return nil, ErrExpired
	}
	if row.MaxDownloads.Valid && row.DownloadCount >= row.MaxDownloads.Int64 {
		return nil, ErrExpired
	}
	if row.PasswordHash.Valid {
		if password == "" {
			return nil, ErrPasswordNeeded
		}
		key := string(hash)
		if _, ok := s.attemptLimiter().Allow(key); !ok {
			return nil, ErrTooManyAttempts
		}
		if err := bcrypt.CompareHashAndPassword([]byte(row.PasswordHash.String), []byte(password)); err != nil {
			s.attemptLimiter().Fail(key)
			return nil, ErrInvalidPassword
		}
		s.attemptLimiter().Succeed(key)
	}
	return row, nil
}

// hashSharePassword validates and hashes a share password. The password is
// trimmed, and a blank or over-long one is rejected as ErrInvalidInput rather
// than reaching bcrypt, which refuses anything longer than
// maxSharePasswordLength bytes and would otherwise surface as a server error.
func hashSharePassword(password string) (*string, error) {
	trimmed := strings.TrimSpace(password)
	if trimmed == "" || len(trimmed) > maxSharePasswordLength {
		return nil, ErrInvalidInput
	}
	digest, err := bcrypt.GenerateFromPassword([]byte(trimmed), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash share password: %w", err)
	}
	value := string(digest)
	return &value, nil
}

// CreateGrant grants another user read or edit access to one of the caller's
// files. An existing live grant for the same (file, grantee) pair is replaced
// rather than duplicated, and a previously revoked one is re-created. FileID
// must name an active file of the caller; an unknown, foreign or non-active
// file is reported as ErrNotFound. GranteeID must name an existing, enabled
// user other than the owner; unknown or disabled grantees are reported as
// ErrNotFound.
func (s *Service) CreateGrant(ctx context.Context, in GrantCreateInput) (*sqlcgen.FileAccessGrant, error) {
	in.Permission = normalizePermission(in.Permission)
	if in.OwnerID <= 0 || in.GranteeID <= 0 || in.OwnerID == in.GranteeID || in.FileID == uuid.Nil || !validPermission(in.Permission) || (in.ExpiresAt != nil && !in.ExpiresAt.After(s.now())) {
		return nil, ErrInvalidInput
	}
	file, err := s.catalog.Get(ctx, in.OwnerID, in.FileID)
	if err != nil {
		return nil, err
	}
	if file.Status != sqlcgen.FileStatusActive {
		return nil, ErrNotFound
	}
	user, err := s.queries.GetUser(ctx, in.GranteeID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && user.DisabledAt.Valid) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get grant recipient: %w", err)
	}
	row, err := s.queries.CreateFileAccessGrant(ctx, sqlcgen.CreateFileAccessGrantParams{
		ID: dbtypes.UUID(uuid.New()), FileID: dbtypes.UUID(in.FileID), OwnerID: in.OwnerID, GranteeID: in.GranteeID,
		Permission: in.Permission, ExpiresAt: dbtypes.OptionalTime(in.ExpiresAt),
	})
	if err != nil {
		return nil, fmt.Errorf("create file access grant: %w", err)
	}
	return row, nil
}

// ListGrants returns the grants on one of the caller's files that have not been
// revoked, newest first, including the grantee's display name and username. The
// query filters on revoked_at alone, so a grant whose expires_at has already
// passed is still listed; ListSharedWithMe is the read path that also filters on
// expiry.
func (s *Service) ListGrants(ctx context.Context, ownerID int64, fileID uuid.UUID) ([]*sqlcgen.ListFileAccessGrantsForOwnerRow, error) {
	if ownerID <= 0 || fileID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	if _, err := s.catalog.Get(ctx, ownerID, fileID); err != nil {
		return nil, err
	}
	rows, err := s.queries.ListFileAccessGrantsForOwner(ctx, sqlcgen.ListFileAccessGrantsForOwnerParams{OwnerID: ownerID, FileID: dbtypes.UUID(fileID)})
	if err != nil {
		return nil, fmt.Errorf("list file access grants: %w", err)
	}
	return rows, nil
}

// UpdateGrant patches a live grant owned by the caller. At least one of
// Permission, ExpiresAt and ClearExpiresAt must be set, a new expiry must be in
// the future and cannot be combined with ClearExpiresAt. Unknown or revoked
// grants are reported as ErrNotFound.
func (s *Service) UpdateGrant(ctx context.Context, in GrantUpdateInput) (*sqlcgen.FileAccessGrant, error) {
	if in.OwnerID <= 0 || in.GrantID == uuid.Nil || (in.Permission == nil && in.ExpiresAt == nil && !in.ClearExpiresAt) || (in.Permission != nil && !validPermission(*in.Permission)) || (in.ExpiresAt != nil && (!in.ExpiresAt.After(s.now()) || in.ClearExpiresAt)) {
		return nil, ErrInvalidInput
	}
	row, err := s.queries.UpdateFileAccessGrant(ctx, sqlcgen.UpdateFileAccessGrantParams{
		Permission: optionalPermission(in.Permission), ExpiresAt: dbtypes.OptionalTime(in.ExpiresAt), ClearExpiresAt: in.ClearExpiresAt,
		ID: dbtypes.UUID(in.GrantID), OwnerID: in.OwnerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update file access grant: %w", err)
	}
	return row, nil
}

// RevokeGrant stamps revoked_at on one of the caller's grants, which removes it
// from access resolution immediately. It returns ErrNotFound when the grant is
// unknown, owned by somebody else, or already revoked.
func (s *Service) RevokeGrant(ctx context.Context, ownerID int64, grantID uuid.UUID) error {
	if ownerID <= 0 || grantID == uuid.Nil {
		return ErrInvalidInput
	}
	count, err := s.queries.RevokeFileAccessGrant(ctx, sqlcgen.RevokeFileAccessGrantParams{ID: dbtypes.UUID(grantID), OwnerID: ownerID})
	if err != nil {
		return fmt.Errorf("revoke file access grant: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

// ListShared returns one page of the caller's active files that are reachable
// through at least one live share or grant, most recently updated first. It is a
// discovery aid only: the rows do not say which share or grant matched, and the
// effective permission is not included. A page is cut at Limit rows and the
// caller pages on with the (updated_at, id) pair of the last entry it received,
// so nothing past the first page is unreachable.
func (s *Service) ListShared(ctx context.Context, in ListSharedInput) ([]*sqlcgen.File, error) {
	if in.OwnerID <= 0 || in.Limit <= 0 {
		return nil, ErrInvalidInput
	}
	params := sqlcgen.ListSharedParams{OwnerID: in.OwnerID, PageSize: in.Limit}
	if in.AfterUpdatedAt != nil && in.AfterID != nil {
		params.AfterUpdatedAt = dbtypes.Time(*in.AfterUpdatedAt)
		params.AfterID = dbtypes.UUID(*in.AfterID)
	}
	rows, err := s.queries.ListShared(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("list shared files: %w", err)
	}
	return rows, nil
}

// ListSharedWithMe returns one page of the active files other owners have
// granted the caller, most recently updated grant first. Expired and revoked
// grants are excluded, and each entry carries the granted permission rather than
// a merged effective one. The page is cut at Limit rows and the caller pages on
// with the grant key of the last entry.
func (s *Service) ListSharedWithMe(ctx context.Context, in ListSharedWithMeInput) ([]SharedWithMe, error) {
	if in.GranteeID <= 0 || in.Limit <= 0 {
		return nil, ErrInvalidInput
	}
	params := sqlcgen.ListSharedWithMeParams{GranteeID: in.GranteeID, PageSize: in.Limit}
	if in.AfterGrantUpdatedAt != nil && in.AfterGrantID != nil {
		params.AfterGrantUpdatedAt = dbtypes.Time(*in.AfterGrantUpdatedAt)
		params.AfterGrantID = dbtypes.UUID(*in.AfterGrantID)
	}
	rows, err := s.queries.ListSharedWithMe(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("list files shared with user: %w", err)
	}
	out := make([]SharedWithMe, 0, len(rows))
	for _, row := range rows {
		grantID, _ := dbtypes.GoogleUUID(row.GrantID)
		entry := SharedWithMe{File: &sqlcgen.File{
			ID: row.ID, UserID: row.UserID, ParentID: row.ParentID, Name: row.Name,
			Kind: row.Kind, MimeType: row.MimeType,
			Size: row.Size, HashAlgorithm: row.HashAlgorithm, HashValue: row.HashValue,
			Encryption: row.Encryption, EncryptionKeyVersion: row.EncryptionKeyVersion,
			Status: row.Status, ModTime: row.ModTime, Generation: row.Generation,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, DeletedAt: row.DeletedAt,
		}, Permission: row.Permission, GrantID: grantID}
		if row.GrantUpdatedAt.Valid {
			entry.GrantUpdatedAt = row.GrantUpdatedAt.Time
		}
		out = append(out, entry)
	}
	return out, nil
}

// ResolveAccess resolves the caller's effective access to a single file. It
// returns ErrInvalidInput for an unset actor or file ID, and ErrNotFound both
// when the file is not an existing active file and when it exists but neither
// ownership nor a qualifying grant covers it, so the two cases stay
// indistinguishable to a caller probing for file existence. With requireEdit, a
// read-only grant does not satisfy the request.
func (s *Service) ResolveAccess(ctx context.Context, actorID int64, fileID uuid.UUID, requireEdit bool) (*Access, error) {
	if actorID <= 0 || fileID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	access, err := s.ResolveAccessMany(ctx, actorID, []uuid.UUID{fileID}, requireEdit)
	if err != nil {
		return nil, err
	}
	return access[0], nil
}

// ResolveAccessMany resolves the caller's effective access for several files at
// once, returning one result per input element in the caller's order and
// repeating entries for duplicate IDs. It is all-or-nothing: if any file does
// not resolve, the whole call fails with ErrNotFound, which covers both an ID
// that is not an active file and an ID that exists without access, so a caller
// learns neither which input failed nor which of the two cases applies. With
// requireEdit, read-only grants do not qualify while ownership always does. A
// nil ID anywhere makes it ErrInvalidInput.
func (s *Service) ResolveAccessMany(ctx context.Context, actorID int64, fileIDs []uuid.UUID, requireEdit bool) ([]*Access, error) {
	if actorID <= 0 || len(fileIDs) == 0 {
		return nil, ErrInvalidInput
	}
	unique := make([]uuid.UUID, 0, len(fileIDs))
	seen := make(map[uuid.UUID]struct{}, len(fileIDs))
	for _, fileID := range fileIDs {
		if fileID == uuid.Nil {
			return nil, ErrInvalidInput
		}
		if _, ok := seen[fileID]; ok {
			continue
		}
		seen[fileID] = struct{}{}
		unique = append(unique, fileID)
	}
	rows, err := s.queries.ResolveFileAccessMany(ctx, sqlcgen.ResolveFileAccessManyParams{
		FileIds: shareUUIDs(unique), ActorID: actorID, RequireEdit: requireEdit,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve file access: %w", err)
	}
	byID := make(map[uuid.UUID]*Access, len(rows))
	for _, row := range rows {
		fileID, fileOK := dbtypes.GoogleUUID(row.TargetFileID)
		rootID, rootOK := dbtypes.GoogleUUID(row.RootFileID)
		if !fileOK || !rootOK {
			return nil, ErrNotFound
		}
		byID[fileID] = &Access{OwnerID: row.OwnerID, RootFileID: rootID, Permission: row.Permission, Owned: row.Owned}
	}
	// A file missing from byID is either not an active file or active without
	// ownership or a qualifying grant. Both are reported as ErrNotFound so that
	// an authenticated caller cannot probe for the existence of file IDs.
	if len(byID) != len(unique) {
		return nil, ErrNotFound
	}
	result := make([]*Access, 0, len(fileIDs))
	for _, fileID := range fileIDs {
		result = append(result, byID[fileID])
	}
	return result, nil
}

// shareUUIDs converts UUIDs into the pgtype form the uuid[] parameters expect,
// preserving the input order.
func shareUUIDs(ids []uuid.UUID) []pgtype.UUID {
	result := make([]pgtype.UUID, len(ids))
	for index, id := range ids {
		result[index] = dbtypes.UUID(id)
	}
	return result
}

// ResolvePublicEditableFile is ResolveFile restricted to shares created with
// edit permission; it backs write operations performed through a public link.
// Shares with read permission are rejected with ErrForbidden, and every other
// failure matches ResolveFile.
func (s *Service) ResolvePublicEditableFile(ctx context.Context, token, password string, fileID uuid.UUID) (*Public, error) {
	resolved, err := s.ResolveFile(ctx, token, password, fileID)
	if err != nil {
		return nil, err
	}
	if resolved.Share.Permission != sqlcgen.SharePermissionEdit {
		return nil, ErrForbidden
	}
	return resolved, nil
}

// ResolvePublicEditableParent resolves the folder a public write should target
// inside an edit share rooted at a folder. A nil, zero or root parentID selects
// the share root; any other ID must be a folder inside the share and is
// resolved as an editable file. It returns the resolved share and the effective
// parent folder, or ErrForbidden when the share is not an editable folder,
// ErrInvalidInput when the requested parent is not a folder, and ErrNotFound
// when it cannot be reached.
func (s *Service) ResolvePublicEditableParent(ctx context.Context, token, password string, parentID *uuid.UUID) (*Public, uuid.UUID, error) {
	resolved, err := s.Resolve(ctx, token, password)
	if err != nil {
		return nil, uuid.Nil, err
	}
	if resolved.Share.Permission != sqlcgen.SharePermissionEdit || resolved.File.Kind != sqlcgen.FileKindFolder {
		return nil, uuid.Nil, ErrForbidden
	}
	rootID, ok := dbtypes.GoogleUUID(resolved.File.ID)
	if !ok {
		return nil, uuid.Nil, ErrNotFound
	}
	if parentID == nil || *parentID == uuid.Nil || *parentID == rootID {
		return resolved, rootID, nil
	}
	child, err := s.ResolvePublicEditableFile(ctx, token, password, *parentID)
	if err != nil {
		return nil, uuid.Nil, err
	}
	if child.File.Kind != sqlcgen.FileKindFolder {
		return nil, uuid.Nil, ErrInvalidInput
	}
	return child, *parentID, nil
}

// newToken mints a share secret and its storage hash. The secret is 32 bytes
// drawn from s.random and rendered as "tds_" plus unpadded base64url; only the
// hash is persisted, so the plaintext returned here cannot be recovered later.
func (s *Service) newToken() (string, []byte, error) {
	buffer := make([]byte, 32)
	if _, err := io.ReadFull(s.random, buffer); err != nil {
		return "", nil, err
	}
	token := "tds_" + base64.RawURLEncoding.EncodeToString(buffer)
	return token, tokenHash(token), nil
}

// tokenHash returns the SHA-256 digest used as the token lookup key. Tokens
// carry full 256-bit entropy, so an unsalted digest is enough to keep stored
// hashes from being redeemed if the table leaks.
func tokenHash(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}
