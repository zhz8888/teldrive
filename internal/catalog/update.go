package catalog

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
)

// UpdateInput describes a partial metadata change to one existing entry. At least
// one of Name and ModTime must be set, otherwise the call is rejected.
type UpdateInput struct {
	// UserID is the owner of the entry and must be positive.
	UserID int64
	// FileID identifies the entry and must not be uuid.Nil.
	FileID uuid.UUID
	// ExpectedGeneration, when non-nil, must equal the stored generation or the
	// update fails with ErrPrecondition.
	ExpectedGeneration *int64
	// Name replaces the entry name when non-nil. The value is passed through
	// unchanged, so the caller validates it.
	Name *string
	// ModTime replaces the modification time when non-nil.
	ModTime *time.Time
}

// Update applies a partial metadata change to one active entry owned by in.UserID.
// It returns ErrInvalidName when the file ID is nil or neither field is set, and
// ErrPrecondition rather than ErrNotFound when a supplied generation no longer
// matches. A name already taken in the same folder surfaces as ErrConflict, and
// the cached row is dropped after the write.
func (s *Service) Update(ctx context.Context, in UpdateInput) (*sqlcgen.File, error) {
	if in.UserID <= 0 {
		return nil, ErrInvalidOwner
	}
	if in.FileID == uuid.Nil || (in.Name == nil && in.ModTime == nil) {
		return nil, ErrInvalidName
	}
	var name pgtype.Text
	if in.Name != nil {
		name = dbtypes.Text(*in.Name)
	}
	file, err := s.queries.UpdateFileMetadata(ctx, sqlcgen.UpdateFileMetadataParams{
		Name:               name,
		ModTime:            dbtypes.OptionalTime(in.ModTime),
		FileID:             dbtypes.UUID(in.FileID),
		UserID:             in.UserID,
		ExpectedGeneration: dbtypes.OptionalInt8(in.ExpectedGeneration),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		if in.ExpectedGeneration != nil {
			return nil, ErrPrecondition
		}
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, classifyWriteError("update file", err)
	}
	s.invalidateFile(ctx, in.UserID, in.FileID)
	return file, nil
}
