// Package uploads manages resumable upload sessions: creation and expiry, the
// per-part lease protocol that serializes writers of a single part, and the
// transactional publication of a completed session as a catalog file.
//
// Sessions belong to one user and move through the open, completing, completed,
// aborted and expired states. Parts are written under a time-boxed lease, so a
// stalled uploader cannot overwrite a newer attempt once its lease lapses.
// Contract violations are reported through the sentinel errors declared here;
// callers should match them with errors.Is.
package uploads

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/dbtypes"
)

// ListInput selects one page of a user's upload sessions.
type ListInput struct {
	// UserID is the session owner whose rows are listed and must be positive.
	UserID int64
	// State restricts the page to a single session state; nil lists every state.
	State *sqlcgen.UploadState
	// AfterCreatedAt is the created_at half of the (created_at, id) cursor from the
	// previous page. It only has an effect together with AfterID: on its own it
	// matches no rows.
	AfterCreatedAt *time.Time
	// AfterID is the id half of the cursor and has no effect unless AfterCreatedAt
	// is also set; rows at or above the cursor in descending (created_at, id) order
	// are skipped.
	AfterID *uuid.UUID
	// Limit caps the page size to 1..200; a non-positive value means 100.
	Limit int32
}

// List returns one page of the user's sessions, newest first, optionally
// restricted to a single state. Limit defaults to 100 and is clamped to at most
// 200. A non-positive UserID yields ErrInvalidInput; database failures are
// wrapped rather than mapped to a sentinel.
func (s *Service) List(ctx context.Context, in ListInput) ([]*sqlcgen.UploadSession, error) {
	if in.UserID <= 0 {
		return nil, ErrInvalidInput
	}
	if in.Limit <= 0 {
		in.Limit = 100
	}
	if in.Limit > 200 {
		in.Limit = 200
	}
	var state sqlcgen.NullUploadState
	if in.State != nil {
		state = sqlcgen.NullUploadState{UploadState: *in.State, Valid: true}
	}
	items, err := s.queries.ListUploadSessions(ctx, sqlcgen.ListUploadSessionsParams{
		UserID:         in.UserID,
		State:          state,
		AfterCreatedAt: dbtypes.OptionalTime(in.AfterCreatedAt),
		AfterID:        dbtypes.OptionalUUID(in.AfterID),
		PageSize:       in.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list uploads: %w", err)
	}
	return items, nil
}

// ListPartsInput selects one page of the parts of a single upload session.
type ListPartsInput struct {
	// UserID must be the session owner: ownership is verified before listing, and a
	// session belonging to somebody else is reported as ErrNotFound.
	UserID int64
	// UploadID identifies the session whose parts are listed; uuid.Nil is rejected.
	UploadID uuid.UUID
	// AfterPartNo is the exclusive cursor: only parts with a greater part_no are
	// returned, and nil starts at the first part.
	AfterPartNo *int32
	// Limit caps the page size to 1..200; a non-positive value means 100.
	Limit int32
}

// ListParts returns one page of the session's parts in ascending part number,
// starting after AfterPartNo when it is set. Ownership is checked first, so an
// unknown or foreign session yields ErrNotFound. The page size follows the same
// default of 100 and cap of 200 as List, and parts are listed in every state,
// including ones still under a lease or failed.
func (s *Service) ListParts(ctx context.Context, in ListPartsInput) ([]*sqlcgen.UploadPart, error) {
	if in.UserID <= 0 || in.UploadID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	if _, err := s.Get(ctx, in.UserID, in.UploadID); err != nil {
		return nil, err
	}
	if in.Limit <= 0 {
		in.Limit = 100
	}
	if in.Limit > 200 {
		in.Limit = 200
	}
	var after pgtype.Int4
	if in.AfterPartNo != nil {
		after = dbtypes.Int4(*in.AfterPartNo)
	}
	parts, err := s.queries.ListUploadParts(ctx, sqlcgen.ListUploadPartsParams{
		UploadID:    dbtypes.UUID(in.UploadID),
		AfterPartNo: after,
		PageSize:    in.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list upload parts: %w", err)
	}
	return parts, nil
}
