package api

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tgdrive/teldrive/v2/internal/api/gen"
	"github.com/tgdrive/teldrive/v2/internal/authn"
	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
	"github.com/tgdrive/teldrive/v2/internal/shares"
	"github.com/tgdrive/teldrive/v2/internal/transfer"
	"github.com/tgdrive/teldrive/v2/internal/uploads"
)

// requireAdmin rejects the request with a 403 problem unless the caller holds the
// admin or owner role. It guards every handler under /v1/admin/users.
func requireAdmin(ctx context.Context) error {
	if !HasAdminRole(ctx) {
		return problem(403, "forbidden", "administrator access is required", shares.ErrForbidden)
	}
	return nil
}

// ListAdminUsers returns the accounts matching an optional search string to an
// administrator. The admin or owner role is required, and a missing Auth service
// reports 503 through mapServiceError.
func (h *Handler) ListAdminUsers(ctx context.Context, params gen.ListAdminUsersParams) (gen.ListAdminUsersRes, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if h.Auth == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	rows, err := h.Auth.ListUsers(ctx, params.Search.Or(""))
	if err != nil {
		return nil, mapServiceError(err)
	}
	out := make(gen.ListAdminUsersOKApplicationJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, adminUserSummary(row))
	}
	return &out, nil
}

// UpdateAdminUser applies a role change and/or a disabled flag to one account on
// behalf of an administrator; disabling also revokes that account's sessions and
// API keys. An empty body is rejected with 422 and an unknown user maps to 404.
func (h *Handler) UpdateAdminUser(ctx context.Context, req *gen.UserAdminUpdateRequest, params gen.UpdateAdminUserParams) (gen.UpdateAdminUserRes, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if h.Auth == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	row, err := h.Auth.GetUser(ctx, params.UserId)
	if err != nil {
		return nil, mapServiceError(err)
	}
	changed := false
	if role, ok := req.Role.Get(); ok {
		row, err = h.Auth.UpdateUserRole(ctx, params.UserId, sqlcgen.UserRole(role))
		if err != nil {
			return nil, mapServiceError(err)
		}
		changed = true
	}
	if disabled, ok := req.Disabled.Get(); ok {
		row, err = h.Auth.SetUserDisabled(ctx, params.UserId, disabled)
		if err != nil {
			return nil, mapServiceError(err)
		}
		changed = true
	}
	if !changed {
		return nil, mapServiceError(authn.ErrInvalidInput)
	}
	out := adminUserSummary(row)
	return &out, nil
}

// RevokeAdminUserAccess revokes every session and API key of one account so it can
// no longer authenticate. Owner accounts are refused with 422 and unknown accounts
// map to 404.
func (h *Handler) RevokeAdminUserAccess(ctx context.Context, params gen.RevokeAdminUserAccessParams) (gen.RevokeAdminUserAccessRes, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if h.Auth == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if err := h.Auth.RevokeUserAccess(ctx, params.UserId); err != nil {
		return nil, mapServiceError(err)
	}
	return &gen.RevokeAdminUserAccessNoContent{}, nil
}

// SearchUsers returns lightweight identities matching the query, which is how a
// share owner finds a grantee. It requires authentication and excludes the caller
// from the results.
func (h *Handler) SearchUsers(ctx context.Context, params gen.SearchUsersParams) (gen.SearchUsersRes, error) {
	actorID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Auth == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	rows, err := h.Auth.SearchUsers(ctx, actorID, params.Search)
	if err != nil {
		return nil, mapServiceError(err)
	}
	out := make(gen.SearchUsersOKApplicationJSON, 0, len(rows))
	for _, row := range rows {
		item := gen.UserSearchResult{UserId: row.UserID}
		if row.DisplayName.Valid {
			item.DisplayName = gen.NewOptString(row.DisplayName.String)
		}
		if row.Username.Valid {
			item.Username = gen.NewOptString(row.Username.String)
		}
		out = append(out, item)
	}
	return &out, nil
}

// CreateFileAccessGrant grants another user read or edit access to one of the
// caller's files, replacing any live grant for the same pair. Unknown or disabled
// grantees surface as 404 and invalid input as 422.
func (h *Handler) CreateFileAccessGrant(ctx context.Context, req *gen.FileAccessGrantCreateRequest, params gen.CreateFileAccessGrantParams) (gen.CreateFileAccessGrantRes, error) {
	ownerID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Shares == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	permission := sqlcgen.SharePermissionRead
	if value, ok := req.Permission.Get(); ok {
		permission = sqlcgen.SharePermission(value)
	}
	var expiresAt *time.Time
	if value, ok := req.ExpiresAt.Get(); ok {
		expiresAt = &value
	}
	row, err := h.Shares.CreateGrant(ctx, shares.GrantCreateInput{
		OwnerID: ownerID, FileID: googleUUID(params.FileId), GranteeID: req.GranteeUserId,
		Permission: permission, ExpiresAt: expiresAt,
	})
	if err != nil {
		return nil, mapServiceError(err)
	}
	out := fileAccessGrantSummary(row, nil, nil)
	return &out, nil
}

// ListFileAccessGrants returns the live grants on one of the caller's files, newest
// first and including the grantee display name and username. Expired and revoked
// grants are omitted by the query.
func (h *Handler) ListFileAccessGrants(ctx context.Context, params gen.ListFileAccessGrantsParams) (gen.ListFileAccessGrantsRes, error) {
	ownerID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Shares == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	rows, err := h.Shares.ListGrants(ctx, ownerID, googleUUID(params.FileId))
	if err != nil {
		return nil, mapServiceError(err)
	}
	out := make(gen.ListFileAccessGrantsOKApplicationJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, fileAccessGrantSummaryRow(row))
	}
	return &out, nil
}

// UpdateFileAccessGrant patches a live grant with a new permission, a new expiry,
// or ClearExpiresAt to drop the expiry. An empty patch is rejected with 422 and
// unknown grants are reported as 404.
func (h *Handler) UpdateFileAccessGrant(ctx context.Context, req *gen.FileAccessGrantUpdateRequest, params gen.UpdateFileAccessGrantParams) (gen.UpdateFileAccessGrantRes, error) {
	ownerID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Shares == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	var permission *sqlcgen.SharePermission
	if value, ok := req.Permission.Get(); ok {
		converted := sqlcgen.SharePermission(value)
		permission = &converted
	}
	var expiresAt *time.Time
	if value, ok := req.ExpiresAt.Get(); ok {
		expiresAt = &value
	}
	row, err := h.Shares.UpdateGrant(ctx, shares.GrantUpdateInput{
		OwnerID: ownerID, GrantID: googleUUID(params.GrantId), Permission: permission,
		ExpiresAt: expiresAt, ClearExpiresAt: req.ClearExpiresAt.Or(false),
	})
	if err != nil {
		return nil, mapServiceError(err)
	}
	out := fileAccessGrantSummary(row, nil, nil)
	return &out, nil
}

// RevokeFileAccessGrant revokes a grant the caller owns, which removes the
// grantee's access immediately. Unknown, foreign or already revoked grants are
// reported as 404.
func (h *Handler) RevokeFileAccessGrant(ctx context.Context, params gen.RevokeFileAccessGrantParams) (gen.RevokeFileAccessGrantRes, error) {
	ownerID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Shares == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if err := h.Shares.RevokeGrant(ctx, ownerID, googleUUID(params.GrantId)); err != nil {
		return nil, mapServiceError(err)
	}
	return &gen.RevokeFileAccessGrantNoContent{}, nil
}

// ListShared lists the caller's own files that are reachable through at least one
// live share or grant, most recently updated first and capped by the service. It
// skips rows whose ID cannot be converted instead of failing the whole page.
func (h *Handler) ListShared(ctx context.Context) (gen.ListSharedRes, error) {
	ownerID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Shares == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	rows, err := h.Shares.ListShared(ctx, ownerID)
	if err != nil {
		return nil, mapServiceError(err)
	}
	out := make(gen.ListSharedOKApplicationJSON, 0, len(rows))
	for _, row := range rows {
		entry, err := fileEntry(row)
		if err != nil {
			continue
		}
		out = append(out, entry)
	}
	return &out, nil
}

// ListSharedWithMe lists the files other owners granted the caller, each carrying
// the granted permission, and drops entries whose IDs cannot be converted.
func (h *Handler) ListSharedWithMe(ctx context.Context) (gen.ListSharedWithMeRes, error) {
	granteeID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Shares == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	rows, err := h.Shares.ListSharedWithMe(ctx, granteeID)
	if err != nil {
		return nil, mapServiceError(err)
	}
	out := make(gen.ListSharedWithMeOKApplicationJSON, 0, len(rows))
	for _, row := range rows {
		entry, err := fileEntry(row.File)
		if err != nil {
			continue
		}
		out = append(out, gen.SharedWithMeEntry{
			File:       entry,
			Permission: gen.SharePermission(row.Permission),
		})
	}
	return &out, nil
}

// CreatePublicShareFolder creates a folder inside an edit-enabled public share,
// acting as the share owner. The share token and optional X-Share-Password header
// are the only credentials, and only the fail conflict policy is accepted.
func (h *Handler) CreatePublicShareFolder(ctx context.Context, req *gen.FolderCreateRequest, params gen.CreatePublicShareFolderParams) (gen.CreatePublicShareFolderRes, error) {
	if h.Shares == nil || h.Catalog == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	var requestedParent *uuid.UUID
	if value, ok := req.ParentId.Get(); ok {
		id := googleUUID(value)
		requestedParent = &id
	}
	resolved, parentID, err := h.Shares.ResolvePublicEditableParent(ctx, params.Token, params.XSharePassword.Or(""), requestedParent)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if policy, ok := req.ConflictPolicy.Get(); ok && string(policy) != "fail" {
		return nil, mapServiceError(uploads.ErrUnsupportedConflictPolicy)
	}
	var modTime time.Time
	if value, ok := req.ModTime.Get(); ok {
		modTime = value
	}
	file, err := h.Catalog.CreateFolder(ctx, catalog.CreateFolderInput{UserID: resolved.Share.OwnerID, ParentID: &parentID, Name: req.Name, ModTime: modTime})
	if err != nil {
		return nil, mapServiceError(err)
	}
	entry, err := fileEntry(file)
	if err != nil {
		return nil, mapServiceError(err)
	}
	return &entry, nil
}

// UpdatePublicShareFile renames an entry inside an edit-enabled public share on
// behalf of the share owner. The If-Match generation is enforced, so a stale value
// maps to 412.
func (h *Handler) UpdatePublicShareFile(ctx context.Context, req *gen.FileUpdateRequest, params gen.UpdatePublicShareFileParams) (gen.UpdatePublicShareFileRes, error) {
	if h.Shares == nil || h.Catalog == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	resolved, err := h.Shares.ResolvePublicEditableFile(ctx, params.Token, params.XSharePassword.Or(""), googleUUID(params.FileId))
	if err != nil {
		return nil, mapServiceError(err)
	}
	generation, err := parseGenerationETag(gen.NewOptETag(params.IfMatch))
	if err != nil {
		return nil, mapServiceError(uploads.ErrInvalidInput)
	}
	var name *string
	if value, ok := req.Name.Get(); ok {
		name = &value
	}
	var modTime *time.Time
	if value, ok := req.ModTime.Get(); ok {
		modTime = &value
	}
	file, err := h.Catalog.Update(ctx, catalog.UpdateInput{UserID: resolved.Share.OwnerID, FileID: googleUUID(params.FileId), ExpectedGeneration: generation, Name: name, ModTime: modTime})
	if err != nil {
		return nil, mapServiceError(err)
	}
	entry, err := fileEntry(file)
	if err != nil {
		return nil, mapServiceError(err)
	}
	return &entry, nil
}

// TrashPublicShareFile moves an entry inside an edit-enabled public share to the
// owner's trash. The share root itself is protected and yields 403.
func (h *Handler) TrashPublicShareFile(ctx context.Context, params gen.TrashPublicShareFileParams) (gen.TrashPublicShareFileRes, error) {
	if h.Shares == nil || h.Catalog == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	fileID := googleUUID(params.FileId)
	resolved, err := h.Shares.ResolvePublicEditableFile(ctx, params.Token, params.XSharePassword.Or(""), fileID)
	if err != nil {
		return nil, mapServiceError(err)
	}
	rootID, _ := dbtypes.GoogleUUID(resolved.Share.FileID)
	if rootID == fileID {
		return nil, mapServiceError(shares.ErrForbidden)
	}
	if _, err := h.Catalog.Trash(ctx, resolved.Share.OwnerID, fileID); err != nil {
		return nil, mapServiceError(err)
	}
	return &gen.TrashPublicShareFileNoContent{}, nil
}

// CreatePublicShareUpload opens an upload session inside an edit-enabled public
// share and credits it to the share owner, so anyone holding the token and password
// can contribute files.
func (h *Handler) CreatePublicShareUpload(ctx context.Context, req *gen.UploadCreateRequest, params gen.CreatePublicShareUploadParams) (gen.CreatePublicShareUploadRes, error) {
	if h.Shares == nil || h.Uploads == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	var requestedParent *uuid.UUID
	if value, ok := req.ParentId.Get(); ok {
		id := googleUUID(value)
		requestedParent = &id
	}
	resolved, parentID, err := h.Shares.ResolvePublicEditableParent(ctx, params.Token, params.XSharePassword.Or(""), requestedParent)
	if err != nil {
		return nil, mapServiceError(err)
	}
	session, err := h.createUploadForOwner(ctx, resolved.Share.OwnerID, &parentID, req)
	if err != nil {
		return nil, err
	}
	response, err := uploadSession(session)
	if err != nil {
		return nil, mapServiceError(err)
	}
	return &response, nil
}

// PutPublicShareUploadPart streams one part of a public-share upload after
// verifying that the session belongs to the share owner. It answers 200 for an
// already stored part and 201 for a new one.
func (h *Handler) PutPublicShareUploadPart(ctx context.Context, req gen.PutPublicShareUploadPartReq, params gen.PutPublicShareUploadPartParams) (gen.PutPublicShareUploadPartRes, error) {
	if h.Shares == nil || h.Uploads == nil || h.UploadPipeline == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	ownerID, err := h.resolvePublicUploadOwner(ctx, params.Token, params.XSharePassword.Or(""), googleUUID(params.UploadId))
	if err != nil {
		return nil, mapServiceError(err)
	}
	var checksum *string
	if value, ok := params.XPartChecksum.Get(); ok {
		text := string(value)
		checksum = &text
	}
	result, err := h.UploadPipeline.UploadPart(ctx, transfer.UploadPartRequest{
		UserID: ownerID, UploadID: googleUUID(params.UploadId), PartNo: params.PartNo,
		PlainSize: params.ContentLength, Checksum: checksum, Body: req.Data,
	})
	if err != nil {
		return nil, mapServiceError(err)
	}
	part, err := uploadPart(result.Part)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if result.Existing {
		response := gen.PutPublicShareUploadPartOK(part)
		return &response, nil
	}
	response := gen.PutPublicShareUploadPartCreated(part)
	return &response, nil
}

// CompletePublicShareUpload finalizes a public-share upload session into a file
// owned by the share owner and returns 201 with ETag and Location headers.
func (h *Handler) CompletePublicShareUpload(ctx context.Context, params gen.CompletePublicShareUploadParams) (gen.CompletePublicShareUploadRes, error) {
	if h.Shares == nil || h.Uploads == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	ownerID, err := h.resolvePublicUploadOwner(ctx, params.Token, params.XSharePassword.Or(""), googleUUID(params.UploadId))
	if err != nil {
		return nil, mapServiceError(err)
	}
	file, err := h.Uploads.Complete(ctx, ownerID, googleUUID(params.UploadId))
	if err != nil {
		return nil, mapServiceError(err)
	}
	entry, err := fileEntry(file)
	if err != nil {
		return nil, mapServiceError(err)
	}
	fileID := googleUUID(entry.ID)
	headers := gen.CopyFileCreatedHeaders{Etag: generationETag(file.Generation), Location: gen.URI(url.URL{Path: "/files/" + fileID.String()}), Response: entry}
	response := gen.CompletePublicShareUploadCreated(headers)
	return &response, nil
}

// AbortPublicShareUpload discards a public-share upload session and its parts
// after the same ownership check and returns 204.
func (h *Handler) AbortPublicShareUpload(ctx context.Context, params gen.AbortPublicShareUploadParams) (gen.AbortPublicShareUploadRes, error) {
	if h.Shares == nil || h.Uploads == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	ownerID, err := h.resolvePublicUploadOwner(ctx, params.Token, params.XSharePassword.Or(""), googleUUID(params.UploadId))
	if err != nil {
		return nil, mapServiceError(err)
	}
	if _, err := h.Uploads.Abort(ctx, ownerID, googleUUID(params.UploadId)); err != nil {
		return nil, mapServiceError(err)
	}
	return &gen.AbortPublicShareUploadNoContent{}, nil
}

// resolveAuthenticatedFileAccess returns the caller's effective access to a file,
// either as owner or through a grant, and demands edit permission when requireEdit
// is set. Without a share service it degrades to a plain catalog lookup that only
// succeeds for the caller's own files.
func (h *Handler) resolveAuthenticatedFileAccess(ctx context.Context, fileID uuid.UUID, requireEdit bool) (*shares.Access, error) {
	actorID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if h.Shares == nil {
		if h.Catalog == nil {
			return nil, ErrOperationUnavailable
		}
		if _, err := h.Catalog.Get(ctx, actorID, fileID); err != nil {
			return nil, err
		}
		return &shares.Access{OwnerID: actorID, RootFileID: fileID, Permission: sqlcgen.SharePermissionEdit, Owned: true}, nil
	}
	return h.Shares.ResolveAccess(ctx, actorID, fileID, requireEdit)
}

// resolveAuthenticatedUploadOwner finds the owner of an upload session so the
// caller can act on it: the caller is either that owner or holds access to the
// session's parent with matching ownership. Any mismatch yields ErrForbidden,
// which maps to 403.
func (h *Handler) resolveAuthenticatedUploadOwner(ctx context.Context, uploadID uuid.UUID, requireEdit bool) (int64, error) {
	actorID, err := UserIDFromContext(ctx)
	if err != nil {
		return 0, err
	}
	if h.Uploads == nil {
		return 0, ErrOperationUnavailable
	}
	session, err := h.Uploads.GetAnyOwner(ctx, uploadID)
	if err != nil {
		return 0, err
	}
	if session.UserID == actorID {
		return actorID, nil
	}
	if h.Shares == nil {
		return 0, shares.ErrForbidden
	}
	parentID, ok := dbtypes.GoogleUUID(session.ParentID)
	if !ok {
		return 0, shares.ErrForbidden
	}
	access, err := h.Shares.ResolveAccess(ctx, actorID, parentID, requireEdit)
	if err != nil {
		return 0, err
	}
	if access.OwnerID != session.UserID {
		return 0, shares.ErrForbidden
	}
	return session.UserID, nil
}

// resolvePublicUploadOwner resolves an upload session for the public share flow,
// requiring the token to expose the session's parent as an editable target and the
// session to belong to the share owner.
func (h *Handler) resolvePublicUploadOwner(ctx context.Context, token, password string, uploadID uuid.UUID) (int64, error) {
	session, err := h.Uploads.GetAnyOwner(ctx, uploadID)
	if err != nil {
		return 0, err
	}
	parentID, ok := dbtypes.GoogleUUID(session.ParentID)
	if !ok {
		return 0, shares.ErrForbidden
	}
	resolved, _, err := h.Shares.ResolvePublicEditableParent(ctx, token, password, &parentID)
	if err != nil {
		return 0, err
	}
	if resolved.Share.OwnerID != session.UserID {
		return 0, shares.ErrForbidden
	}
	return session.UserID, nil
}

// createUploadForOwner builds and submits the upload session for ownerID,
// defaulting the conflict policy to fail and honoring the requested encryption.
// Encrypted uploads need a positive active key version, otherwise the request
// fails with transfer.ErrEncryptionKey.
func (h *Handler) createUploadForOwner(ctx context.Context, ownerID int64, parentID *uuid.UUID, req *gen.UploadCreateRequest) (*sqlcgen.UploadSession, error) {
	input := uploads.CreateInput{
		UserID: ownerID, ParentID: parentID, Name: req.Name, ExpectedSize: req.Size,
		ModTime: req.ModTime, PartSize: req.PreferredPartSize.Or(0), Encryption: false,
		ConflictPolicy: sqlcgen.NameConflictPolicyFail,
	}
	if value, ok := req.MimeType.Get(); ok {
		input.MIMEType = &value
	}
	if value, ok := req.Hash.Get(); ok {
		algorithm, checksum := string(value.Algorithm), string(value.Value)
		input.ExpectedHashAlgorithm, input.ExpectedHashValue = &algorithm, &checksum
	}
	if value, ok := req.Encryption.Get(); ok {
		input.Encryption = value
	}
	if input.Encryption {
		if h.ActiveEncryptionKeyVersion <= 0 {
			return nil, mapServiceError(transfer.ErrEncryptionKey)
		}
		version := h.ActiveEncryptionKeyVersion
		input.EncryptionKeyVersion = &version
	}
	if value, ok := req.ConflictPolicy.Get(); ok {
		input.ConflictPolicy = sqlcgen.NameConflictPolicy(value)
	}
	session, err := h.Uploads.Create(ctx, input)
	if err != nil {
		return nil, mapServiceError(err)
	}
	return session, nil
}

// adminUserSummary converts a user row into the administrative view, deriving the
// disabled flag from the disabled-at timestamp.
func adminUserSummary(row *sqlcgen.User) gen.AdminUserSummary {
	out := gen.AdminUserSummary{
		UserId: row.UserID, Premium: row.Premium, Role: gen.UserRole(row.Role), Disabled: row.DisabledAt.Valid,
		CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}
	if row.DisplayName.Valid {
		out.DisplayName = gen.NewOptString(row.DisplayName.String)
	}
	if row.Username.Valid {
		out.Username = gen.NewOptString(row.Username.String)
	}
	return out
}

// fileAccessGrantSummary converts a grant row into its API model; the optional
// grantee display name and username are set only when the caller supplies them
// from a joined query.
func fileAccessGrantSummary(row *sqlcgen.FileAccessGrant, displayName, username *string) gen.FileAccessGrantSummary {
	id, _ := dbtypes.GoogleUUID(row.ID)
	fileID, _ := dbtypes.GoogleUUID(row.FileID)
	out := gen.FileAccessGrantSummary{
		ID: apiUUID(id), FileId: apiUUID(fileID), OwnerId: row.OwnerID, GranteeUserId: row.GranteeID,
		Permission: gen.SharePermission(row.Permission), CreatedAt: row.CreatedAt.Time,
	}
	if row.ExpiresAt.Valid {
		out.ExpiresAt = gen.NewOptDateTime(row.ExpiresAt.Time)
	}
	if displayName != nil && strings.TrimSpace(*displayName) != "" {
		out.GranteeDisplayName = gen.NewOptString(*displayName)
	}
	if username != nil && strings.TrimSpace(*username) != "" {
		out.GranteeUsername = gen.NewOptString(*username)
	}
	return out
}

// fileAccessGrantSummaryRow adapts a list query row, which already joins the
// grantee profile, to fileAccessGrantSummary.
func fileAccessGrantSummaryRow(row *sqlcgen.ListFileAccessGrantsForOwnerRow) gen.FileAccessGrantSummary {
	base := &sqlcgen.FileAccessGrant{
		ID: row.ID, FileID: row.FileID, OwnerID: row.OwnerID, GranteeID: row.GranteeID, Permission: row.Permission,
		ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, RevokedAt: row.RevokedAt,
	}
	var displayName, username *string
	if row.GranteeDisplayName.Valid {
		displayName = &row.GranteeDisplayName.String
	}
	if row.GranteeUsername.Valid {
		username = &row.GranteeUsername.String
	}
	return fileAccessGrantSummary(base, displayName, username)
}

var _ = errors.Is
