package api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zhz8888/teldrive/v2/internal/api/gen"
	"github.com/zhz8888/teldrive/v2/internal/authn"
	"github.com/zhz8888/teldrive/v2/internal/bots"
	"github.com/zhz8888/teldrive/v2/internal/channels"
	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/dbtypes"
	"github.com/zhz8888/teldrive/v2/internal/fileops"
	"github.com/zhz8888/teldrive/v2/internal/shares"
)

// TelegramLoginStart begins an unauthenticated Telegram login flow and sends a
// login code to the requested phone number. It delegates to h.Auth and reports
// flow errors through mapServiceError; a missing auth service yields 503.
func (h *Handler) TelegramLoginStart(ctx context.Context, req *gen.TelegramLoginStartRequest) (gen.TelegramLoginStartRes, error) {
	if h.Auth == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	flow, err := h.Auth.StartLogin(ctx, req.PhoneNumber)
	if err != nil {
		return nil, mapServiceError(err)
	}
	response := loginFlowResponse(flow)
	return &response, nil
}

// TelegramQRLoginStart begins an unauthenticated QR login flow and returns the
// QR URL the user must approve in Telegram. Delegates to h.Auth.StartQR, so a
// missing service or an upstream failure surfaces through mapServiceError.
func (h *Handler) TelegramQRLoginStart(ctx context.Context) (gen.TelegramQRLoginStartRes, error) {
	if h.Auth == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	flow, err := h.Auth.StartQR(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	response := qrLoginFlowResponse(flow)
	return &response, nil
}

// TelegramQRLoginPoll advances a QR login flow and returns either the refreshed
// QR challenge or, once the account is authorized, a token pair. It is
// unauthenticated and maps expired flows to 410 through mapServiceError.
func (h *Handler) TelegramQRLoginPoll(ctx context.Context, req *gen.TelegramQRLoginPollRequest) (gen.TelegramQRLoginPollRes, error) {
	if h.Auth == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	result, err := h.Auth.PollQR(ctx, googleUUID(req.FlowId))
	if err != nil {
		return nil, mapServiceError(err)
	}
	if result.QRFlow != nil {
		response := qrLoginFlowResponse(result.QRFlow)
		return &response, nil
	}
	response := tokenPairResponse(result.Tokens)
	return &response, nil
}

// TelegramLoginVerifyCode submits the login code for a flow and returns a token
// pair, or the flow again with passwordRequired set when two-step verification
// is enabled. Unauthenticated; invalid codes map to 422 and expired flows to 410.
func (h *Handler) TelegramLoginVerifyCode(ctx context.Context, req *gen.TelegramCodeVerifyRequest) (gen.TelegramLoginVerifyCodeRes, error) {
	if h.Auth == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	result, err := h.Auth.VerifyCode(ctx, googleUUID(req.FlowId), req.Code)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if result.Flow != nil {
		response := loginFlowResponse(result.Flow)
		return &response, nil
	}
	response := tokenPairResponse(result.Tokens)
	return &response, nil
}

// TelegramLoginVerifyPassword completes two-step verification for a pending flow
// and returns the token pair. Unauthenticated; a wrong password maps to 401 via
// authn.ErrPasswordInvalid.
func (h *Handler) TelegramLoginVerifyPassword(ctx context.Context, req *gen.TelegramPasswordVerifyRequest) (gen.TelegramLoginVerifyPasswordRes, error) {
	if h.Auth == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	result, err := h.Auth.VerifyPassword(ctx, googleUUID(req.FlowId), req.Password)
	if err != nil {
		return nil, mapServiceError(err)
	}
	response := tokenPairResponse(result.Tokens)
	return &response, nil
}

// RefreshSession rotates the access and refresh token pair using the refresh
// token in the body; the endpoint is unauthenticated because that token is the
// credential. Invalid or revoked tokens map to 401.
func (h *Handler) RefreshSession(ctx context.Context, req *gen.RefreshTokenRequest) (gen.RefreshSessionRes, error) {
	if h.Auth == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	tokens, err := h.Auth.Refresh(ctx, req.RefreshToken)
	if err != nil {
		return nil, mapServiceError(err)
	}
	response := tokenPairResponse(tokens)
	return &response, nil
}

// LogoutSession revokes the session behind the caller's bearer token or browser
// cookie and returns 204. It requires an authenticated identity; session lookup
// failures map to 404 and h.Auth failures are mapped by mapServiceError.
func (h *Handler) LogoutSession(ctx context.Context) (gen.LogoutSessionRes, error) {
	if h.Auth == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		return nil, mapServiceError(ErrUnauthenticated)
	}
	if err := h.Auth.Logout(ctx, identity); err != nil {
		return nil, mapServiceError(err)
	}
	return &gen.LogoutSessionNoContent{}, nil
}

// GetCurrentUser returns the authenticated user's profile, including the
// capabilities derived from their role. Authentication is required, and a
// missing Auth service reports 503 through mapServiceError.
func (h *Handler) GetCurrentUser(ctx context.Context) (gen.GetCurrentUserRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Auth == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	user, err := h.Auth.GetUser(ctx, userID)
	if err != nil {
		return nil, mapServiceError(err)
	}
	response := userProfile(user)
	return &response, nil
}

// GetProfilePhoto streams the Telegram profile photo of the authenticated
// account through h.TelegramAccount and returns 204 when the account has none.
// The response is cacheable for a day and its ETag is the Telegram photo ID.
func (h *Handler) GetProfilePhoto(ctx context.Context) (gen.GetProfilePhotoRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.TelegramAccount == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	photo, found, err := h.TelegramAccount.ProfilePhoto(ctx, userID)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if !found {
		return &gen.GetProfilePhotoNoContent{}, nil
	}
	return &gen.GetProfilePhotoOKHeaders{
		CacheControl:       "private, max-age=86400, must-revalidate",
		ContentDisposition: `inline; filename="profile.jpeg"`,
		ContentLength:      int64(len(photo.Content)),
		Etag:               gen.ETag(fmt.Sprintf("\"%d\"", photo.PhotoID)),
		Response:           gen.GetProfilePhotoOK{Data: bytes.NewReader(photo.Content)},
	}, nil
}

// CreateApiKey mints an API key for the authenticated user and returns the
// plaintext secret exactly once, since only its hash is stored. The generated
// security layer accepts bearer tokens and browser cookies here, not API keys.
func (h *Handler) CreateApiKey(ctx context.Context, req *gen.ApiKeyCreateRequest) (gen.CreateApiKeyRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Auth == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	var expires *time.Time
	if value, ok := req.ExpiresAt.Get(); ok {
		expires = &value
	}
	created, err := h.Auth.CreateAPIKey(ctx, userID, req.Name, expires)
	if err != nil {
		return nil, mapServiceError(err)
	}
	rowID, _ := dbtypes.GoogleUUID(created.Row.ID)
	response := gen.ApiKeyCreated{
		ID: apiUUID(rowID), Name: created.Row.Name, Secret: created.Secret,
		CreatedAt: created.Row.CreatedAt.Time,
	}
	if created.Row.ExpiresAt.Valid {
		response.ExpiresAt = gen.NewOptDateTime(created.Row.ExpiresAt.Time)
	}
	return &response, nil
}

// ListApiKeys returns a cursor page of the authenticated user's API keys, newest
// first. The opaque cursor carries the (createdAt, id) pair of the previous page;
// bearer tokens and browser cookies are accepted, API keys are not.
func (h *Handler) ListApiKeys(ctx context.Context, params gen.ListApiKeysParams) (gen.ListApiKeysRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Auth == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	var cursor datedUUIDCursor
	if err := decodeCursor(params.Cursor, &cursor); err != nil {
		return nil, mapServiceError(authn.ErrInvalidInput)
	}
	input := authn.ListAPIKeysInput{UserID: userID, Limit: clampListLimit(params.Limit.Or(100))}
	if cursor.ID != uuid.Nil {
		input.AfterID, input.AfterCreatedAt = &cursor.ID, &cursor.CreatedAt
	}
	rows, err := h.Auth.ListAPIKeys(ctx, input)
	if err != nil {
		return nil, mapServiceError(err)
	}
	items := make([]gen.ApiKeySummary, 0, len(rows))
	for _, row := range rows {
		items = append(items, apiKeySummary(row))
	}
	response := gen.ListApiKeysOK{Items: items}
	if len(rows) == int(input.Limit) && len(rows) > 0 {
		lastID, _ := dbtypes.GoogleUUID(rows[len(rows)-1].ID)
		response.NextCursor = encodeCursor(datedUUIDCursor{CreatedAt: rows[len(rows)-1].CreatedAt.Time, ID: lastID})
	}
	return &response, nil
}

// RevokeApiKey revokes one API key of the authenticated user and returns 204.
// Unknown or foreign key IDs surface as 404 through authn.ErrAPIKeyNotFound.
func (h *Handler) RevokeApiKey(ctx context.Context, params gen.RevokeApiKeyParams) (gen.RevokeApiKeyRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Auth == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if err := h.Auth.RevokeAPIKey(ctx, userID, googleUUID(params.ApiKeyId)); err != nil {
		return nil, mapServiceError(err)
	}
	return &gen.RevokeApiKeyNoContent{}, nil
}

// ListSessions returns a cursor page of the authenticated account's TelDrive
// sessions and flags the one backing the current credential. A missing identity
// is reported as 404 rather than 401, while an unwired auth service reports 503
// like every other handler.
func (h *Handler) ListSessions(ctx context.Context, params gen.ListSessionsParams) (gen.ListSessionsRes, error) {
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		return nil, mapServiceError(authn.ErrSessionNotFound)
	}
	if h.Auth == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	var cursor datedUUIDCursor
	if err := decodeCursor(params.Cursor, &cursor); err != nil {
		return nil, mapServiceError(authn.ErrInvalidInput)
	}
	input := authn.ListSessionsInput{UserID: identity.UserID, Limit: clampListLimit(params.Limit.Or(100))}
	if cursor.ID != uuid.Nil {
		input.AfterID, input.AfterCreatedAt = &cursor.ID, &cursor.CreatedAt
	}
	rows, err := h.Auth.ListSessions(ctx, input)
	if err != nil {
		return nil, mapServiceError(err)
	}
	items := make([]gen.SessionSummary, 0, len(rows))
	for _, row := range rows {
		sessionID, ok := dbtypes.GoogleUUID(row.ID)
		if !ok {
			return nil, mapServiceError(authn.ErrSessionNotFound)
		}
		item := gen.SessionSummary{
			ID: apiUUID(sessionID), Current: sessionID == identity.SessionID,
			CreatedAt: row.CreatedAt.Time, ExpiresAt: row.ExpiresAt.Time,
		}
		if row.LastUsedAt.Valid {
			item.LastUsedAt = gen.NewOptDateTime(row.LastUsedAt.Time)
		}
		if row.RevokedAt.Valid {
			item.RevokedAt = gen.NewOptDateTime(row.RevokedAt.Time)
		}
		items = append(items, item)
	}
	response := gen.ListSessionsOK{Items: items}
	if len(rows) == int(input.Limit) && len(rows) > 0 {
		lastID, _ := dbtypes.GoogleUUID(rows[len(rows)-1].ID)
		response.NextCursor = encodeCursor(datedUUIDCursor{CreatedAt: rows[len(rows)-1].CreatedAt.Time, ID: lastID})
	}
	return &response, nil
}

// RevokeSession revokes one TelDrive session of the authenticated account.
// Revoking the current session invalidates its bearer token immediately; unknown
// session IDs and a missing identity surface as 404, while an unwired auth
// service reports 503.
func (h *Handler) RevokeSession(ctx context.Context, params gen.RevokeSessionParams) (gen.RevokeSessionRes, error) {
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		return nil, mapServiceError(authn.ErrSessionNotFound)
	}
	if h.Auth == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if err := h.Auth.RevokeSession(ctx, identity.UserID, googleUUID(params.SessionId)); err != nil {
		return nil, mapServiceError(err)
	}
	return &gen.RevokeSessionNoContent{}, nil
}

// CreateBots registers Telegram bot tokens for the authenticated user: malformed
// or duplicate tokens are reported by index in failedIndexes, the valid ones are
// stored as pending, and provisioning is queued through h.Jobs.InsertBotProvision.
//
// Provisioning is queued for every bot ID this request validated, not only for the
// rows that were newly inserted. Storing a token and queueing its job are two
// writes, so a request that failed between them leaves a pending row behind; a
// retry with the same token inserts nothing, and queueing per inserted row would
// leave that bot pending forever.
func (h *Handler) CreateBots(ctx context.Context, req *gen.BotCreateRequest) (gen.CreateBotsRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Bots == nil || h.Jobs == nil || req == nil || len(req.Tokens) == 0 {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	response := gen.BotCreateResponse{Bots: []gen.BotSummary{}, FailedIndexes: []int32{}}
	tokens := make([]string, 0, len(req.Tokens))
	requested := make([]int64, 0, len(req.Tokens))
	seen := make(map[int64]struct{}, len(req.Tokens))
	for index, raw := range req.Tokens {
		token := strings.TrimSpace(raw)
		botID, parseErr := bots.TokenBotID(token)
		if parseErr != nil {
			response.FailedIndexes = append(response.FailedIndexes, int32(index))
			continue
		}
		if _, exists := seen[botID]; exists {
			response.FailedIndexes = append(response.FailedIndexes, int32(index))
			continue
		}
		seen[botID] = struct{}{}
		tokens = append(tokens, token)
		requested = append(requested, botID)
	}
	// A request whose tokens were all malformed or duplicated still has a
	// meaningful answer: the per-index feedback the contract models. Reporting it
	// instead of calling InsertPending keeps an empty token list from turning the
	// documented response into a bare 422 that drops failedIndexes.
	if len(tokens) == 0 {
		return &response, nil
	}
	rows, insertErr := h.Bots.InsertPending(ctx, userID, tokens)
	if insertErr != nil {
		return nil, mapServiceError(insertErr)
	}
	for _, row := range rows {
		response.Bots = append(response.Bots, botSummary(row))
	}
	if len(requested) > 0 {
		jobID, jobErr := h.Jobs.InsertBotProvision(ctx, userID, requested)
		if jobErr != nil {
			return nil, mapServiceError(jobErr)
		}
		if jobID != "" {
			response.JobId = gen.NewOptString(jobID)
		}
	}
	return &response, nil
}

// ListBots returns a cursor page of the authenticated user's registered bots,
// newest first. A malformed cursor maps to 422 through bots.ErrInvalidInput.
func (h *Handler) ListBots(ctx context.Context, params gen.ListBotsParams) (gen.ListBotsRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Bots == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	var cursor datedInt64Cursor
	if err := decodeCursor(params.Cursor, &cursor); err != nil {
		return nil, mapServiceError(bots.ErrInvalidInput)
	}
	input := bots.ListInput{UserID: userID, Limit: clampListLimit(params.Limit.Or(100))}
	if cursor.ID != 0 {
		input.AfterCreatedAt, input.AfterBotID = &cursor.CreatedAt, &cursor.ID
	}
	rows, err := h.Bots.List(ctx, input)
	if err != nil {
		return nil, mapServiceError(err)
	}
	items := make([]gen.BotSummary, 0, len(rows))
	for _, row := range rows {
		items = append(items, botSummary(row))
	}
	response := gen.ListBotsOK{Items: items}
	if len(rows) == int(input.Limit) && len(rows) > 0 {
		last := rows[len(rows)-1]
		response.NextCursor = encodeCursor(datedInt64Cursor{CreatedAt: last.CreatedAt.Time, ID: last.BotID})
	}
	return &response, nil
}

// DeleteBot removes one of the authenticated user's bots. The lookup is scoped by
// user ID, so unknown or foreign bot IDs surface as 404 via bots.ErrNotFound.
func (h *Handler) DeleteBot(ctx context.Context, params gen.DeleteBotParams) (gen.DeleteBotRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Bots == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if err := h.Bots.Delete(ctx, userID, params.BotId); err != nil {
		return nil, mapServiceError(err)
	}
	return &gen.DeleteBotNoContent{}, nil
}

// DiscoverChannels lists the Telegram channels where the authenticated account
// may add administrators, querying Telegram through h.TelegramAccount. Both the
// account and its upstream call must succeed for the list to be returned.
func (h *Handler) DiscoverChannels(ctx context.Context) (gen.DiscoverChannelsRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.TelegramAccount == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	remote, err := h.TelegramAccount.DiscoverChannels(ctx, userID)
	if err != nil {
		return nil, mapServiceError(err)
	}
	items := make([]gen.DiscoveredChannel, 0, len(remote))
	for _, channel := range remote {
		items = append(items, gen.DiscoveredChannel{ID: channel.ID, Name: channel.Name})
	}
	response := gen.DiscoverChannelsOKApplicationJSON(items)
	return &response, nil
}

// SyncChannels discovers manageable Telegram channels and upserts them through
// h.Channels.Sync. Rows that are missing remotely are deliberately kept, so the
// operation never deletes a channel that still holds stored parts.
func (h *Handler) SyncChannels(ctx context.Context) (gen.SyncChannelsRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.TelegramAccount == nil || h.Channels == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	discovered, err := h.TelegramAccount.DiscoverChannels(ctx, userID)
	if err != nil {
		return nil, mapServiceError(err)
	}
	remote := make([]channels.RemoteChannel, 0, len(discovered))
	for _, channel := range discovered {
		remote = append(remote, channels.RemoteChannel{ID: channel.ID, Name: channel.Name})
	}
	rows, err := h.Channels.Sync(ctx, userID, remote)
	if err != nil {
		return nil, mapServiceError(err)
	}
	items := make([]gen.ChannelSummary, 0, len(rows))
	for _, row := range rows {
		items = append(items, channelSummary(row))
	}
	response := gen.SyncChannelsOKApplicationJSON(items)
	return &response, nil
}

// CreateChannel creates a real Telegram channel for the authenticated user and
// records it. A blank name is generated from the configured prefix and timestamp,
// and selecting the new channel clears the previous selection.
func (h *Handler) CreateChannel(ctx context.Context, req *gen.ChannelCreateRequest) (gen.CreateChannelRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Channels == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	row, err := h.Channels.Create(ctx, userID, req.Name.Or(""), req.Selected.Or(false))
	if err != nil {
		return nil, mapServiceError(err)
	}
	response := channelSummary(row)
	return &response, nil
}

// ListChannels returns a cursor page of the authenticated user's channels, newest
// first. A malformed cursor is reported as 422, the status every other cursor
// listing uses.
func (h *Handler) ListChannels(ctx context.Context, params gen.ListChannelsParams) (gen.ListChannelsRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Channels == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	var cursor datedInt64Cursor
	if err := decodeCursor(params.Cursor, &cursor); err != nil {
		return nil, problem(http.StatusUnprocessableEntity, "invalid_cursor", "channel cursor is invalid", err)
	}
	input := channels.ListInput{UserID: userID, Limit: clampListLimit(params.Limit.Or(100))}
	if cursor.ID != 0 {
		input.AfterCreatedAt, input.AfterChannelID = &cursor.CreatedAt, &cursor.ID
	}
	rows, err := h.Channels.List(ctx, input)
	if err != nil {
		return nil, mapServiceError(err)
	}
	items := make([]gen.ChannelSummary, 0, len(rows))
	for _, row := range rows {
		items = append(items, channelSummary(row))
	}
	response := gen.ListChannelsOK{Items: items}
	if len(rows) == int(input.Limit) && len(rows) > 0 {
		last := rows[len(rows)-1]
		response.NextCursor = encodeCursor(datedInt64Cursor{CreatedAt: last.CreatedAt.Time, ID: last.ChannelID})
	}
	return &response, nil
}

// SelectChannel makes one channel the active upload target, clearing any previous
// selection. Unknown channels map to 404 through channels.ErrInvalidChannel, while
// an unhealthy or full channel maps to 409 conflict.
func (h *Handler) SelectChannel(ctx context.Context, params gen.SelectChannelParams) (gen.SelectChannelRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Channels == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	row, err := h.Channels.Select(ctx, userID, params.ChannelId)
	if err != nil {
		return nil, mapServiceError(err)
	}
	response := channelSummary(row)
	return &response, nil
}

// DeleteChannel deletes the Telegram channel and its row for the authenticated
// user. The selected channel is refused with 409 via channels.ErrSelectedChannel,
// and a channel still holding referenced parts is refused with 409 as well.
func (h *Handler) DeleteChannel(ctx context.Context, params gen.DeleteChannelParams) (gen.DeleteChannelRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Channels == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if err := h.Channels.Delete(ctx, userID, params.ChannelId); err != nil {
		return nil, mapServiceError(err)
	}
	return &gen.DeleteChannelNoContent{}, nil
}

// CopyFile copies a file or folder through h.FileOps.Copy. The source needs read
// access and the destination edit access; crossing owners is rejected with 403.
// Success is 201 with ETag and Location headers.
func (h *Handler) CopyFile(ctx context.Context, req *gen.FileCopyRequest, params gen.CopyFileParams) (gen.CopyFileRes, error) {
	if h.FileOps == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	var name *string
	if value, ok := req.Name.Get(); ok {
		name = &value
	}
	policy := sqlcgen.NameConflictPolicyFail
	if value, ok := req.ConflictPolicy.Get(); ok {
		policy = sqlcgen.NameConflictPolicy(value)
	}
	sourceAccess, err := h.resolveAuthenticatedFileAccess(ctx, googleUUID(params.FileId), false)
	if err != nil {
		return nil, mapServiceError(err)
	}
	parentID := optionalGoogleUUID(req.ParentId)
	if parentID == nil {
		if !sourceAccess.Owned {
			return nil, mapServiceError(shares.ErrForbidden)
		}
	} else {
		destinationAccess, err := h.resolveAuthenticatedFileAccess(ctx, *parentID, true)
		if err != nil {
			return nil, mapServiceError(err)
		}
		if destinationAccess.OwnerID != sourceAccess.OwnerID {
			return nil, mapServiceError(shares.ErrForbidden)
		}
	}
	file, err := h.FileOps.Copy(ctx, fileops.CopyInput{
		UserID: sourceAccess.OwnerID, FileID: googleUUID(params.FileId), ParentID: parentID, Name: name,
		ConflictPolicy: policy,
	})
	if err != nil {
		return nil, mapServiceError(err)
	}
	entry, err := fileEntry(file)
	if err != nil {
		return nil, mapServiceError(err)
	}
	fileID := googleUUID(entry.ID)
	return &gen.CopyFileCreatedHeaders{
		Etag: generationETag(file.Generation), Location: gen.URI(url.URL{Path: "/v1/files/" + fileID.String()}),
		Response: entry,
	}, nil
}

// PurgeFile queues permanent deletion of a trashed file through
// h.FileOps.QueuePurge and asks h.Jobs for an immediate sweep. If that enqueue
// fails it is only logged, so the request still returns 204.
func (h *Handler) PurgeFile(ctx context.Context, params gen.PurgeFileParams) (gen.PurgeFileRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.FileOps == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if err := h.FileOps.QueuePurge(ctx, userID, googleUUID(params.FileId)); err != nil {
		return nil, mapServiceError(err)
	}
	if h.Jobs != nil {
		if err := h.Jobs.InsertPurge(ctx); err != nil {
			slog.WarnContext(ctx, "queue immediate purge sweep", "error", err)
		}
	}
	return &gen.PurgeFileNoContent{}, nil
}

// CleanTrash marks every trashed entry of the authenticated user as pending
// deletion through h.FileOps.CleanTrash; the background purge worker performs the
// actual removal. It returns 204 and discards the affected row count.
func (h *Handler) CleanTrash(ctx context.Context) (gen.CleanTrashRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.FileOps == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if _, err := h.FileOps.CleanTrash(ctx, userID); err != nil {
		return nil, mapServiceError(err)
	}
	return &gen.CleanTrashNoContent{}, nil
}

// CreateShare creates a public share link for a file the caller owns, defaulting
// to read permission. Password, expiry and download limit are optional, and the
// plaintext token is returned only in this response.
func (h *Handler) CreateShare(ctx context.Context, req *gen.ShareCreateRequest, params gen.CreateShareParams) (gen.CreateShareRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Shares == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	var password *string
	if value, ok := req.Password.Get(); ok {
		password = &value
	}
	var expires *time.Time
	if value, ok := req.ExpiresAt.Get(); ok {
		expires = &value
	}
	var maxDownloads *int64
	if value, ok := req.MaxDownloads.Get(); ok {
		maxDownloads = &value
	}
	permission := sqlcgen.SharePermissionRead
	if value, ok := req.Permission.Get(); ok {
		permission = sqlcgen.SharePermission(value)
	}
	created, err := h.Shares.Create(ctx, shares.CreateInput{
		OwnerID: userID, FileID: googleUUID(params.FileId), Password: password,
		ExpiresAt: expires, MaxDownloads: maxDownloads, Permission: permission,
	})
	if err != nil {
		return nil, mapServiceError(err)
	}
	response := shareCreated(created)
	return &response, nil
}

// ListFileShares returns a cursor page of the shares created for one file, newest
// first, including revoked and expired rows with their state. The caller must own
// the file.
func (h *Handler) ListFileShares(ctx context.Context, params gen.ListFileSharesParams) (gen.ListFileSharesRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Shares == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	var cursor datedUUIDCursor
	if err := decodeCursor(params.Cursor, &cursor); err != nil {
		return nil, mapServiceError(shares.ErrInvalidInput)
	}
	input := shares.ListInput{OwnerID: userID, FileID: googleUUID(params.FileId), Limit: clampListLimit(params.Limit.Or(100))}
	if cursor.ID != uuid.Nil {
		input.AfterCreatedAt, input.AfterID = &cursor.CreatedAt, &cursor.ID
	}
	rows, err := h.Shares.List(ctx, input)
	if err != nil {
		return nil, mapServiceError(err)
	}
	items := make([]gen.ShareSummary, 0, len(rows))
	for _, row := range rows {
		items = append(items, shareSummary(row))
	}
	response := gen.ListFileSharesOK{Items: items}
	if len(rows) == int(input.Limit) && len(rows) > 0 {
		last := rows[len(rows)-1]
		lastID, _ := dbtypes.GoogleUUID(last.ID)
		response.NextCursor = encodeCursor(datedUUIDCursor{CreatedAt: last.CreatedAt.Time, ID: lastID})
	}
	return &response, nil
}

// RevokeShare revokes one share owned by the authenticated user, which makes later
// resolves of its token fail with 410. Unknown, foreign or already revoked share
// IDs surface as 404.
func (h *Handler) RevokeShare(ctx context.Context, params gen.RevokeShareParams) (gen.RevokeShareRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Shares == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if err := h.Shares.Revoke(ctx, userID, googleUUID(params.ShareId)); err != nil {
		return nil, mapServiceError(err)
	}
	return &gen.RevokeShareNoContent{}, nil
}

// GetPublicShare resolves a public share token and returns the shared entry. The
// endpoint is unauthenticated: the password travels in X-Share-Password, a missing
// or wrong password maps to 401, and an expired share maps to 410.
func (h *Handler) GetPublicShare(ctx context.Context, params gen.GetPublicShareParams) (gen.GetPublicShareRes, error) {
	if h.Shares == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	resolved, err := h.Shares.Resolve(ctx, params.Token, params.XSharePassword.Or(""))
	if err != nil {
		return nil, mapServiceError(err)
	}
	entry, err := fileEntry(resolved.File)
	if err != nil {
		return nil, mapServiceError(err)
	}
	shareID, _ := dbtypes.GoogleUUID(resolved.Share.ID)
	response := gen.PublicShare{
		ID: apiUUID(shareID), File: entry, PasswordProtected: resolved.Share.PasswordHash.Valid,
		Permission: gen.SharePermission(resolved.Share.Permission),
	}
	if resolved.Share.ExpiresAt.Valid {
		response.ExpiresAt = gen.NewOptDateTime(resolved.Share.ExpiresAt.Time)
	}
	return &response, nil
}

// HeadPublicShare returns download metadata for a share root without a body,
// resolving the token with the same rules as GetPublicShare. Entries that are not
// sized regular files are reported as 422.
func (h *Handler) HeadPublicShare(ctx context.Context, params gen.HeadPublicShareParams) (gen.HeadPublicShareRes, error) {
	if h.Shares == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	resolved, err := h.Shares.Resolve(ctx, params.Token, params.XSharePassword.Or(""))
	if err != nil {
		return nil, mapServiceError(err)
	}
	file := resolved.File
	if file.Kind != sqlcgen.FileKindFile || !file.Size.Valid {
		return nil, rejectUndownloadable()
	}
	return &gen.HeadPublicShareOK{
		AcceptRanges: gen.HeadPublicShareOKAcceptRanges("bytes"), ContentDisposition: contentDisposition(file.Name, false),
		ContentLength: file.Size.Int64, Etag: contentETag(file), LastModified: file.ModTime.Time,
	}, nil
}

// HeadPublicShareLegacy serves the pre-v1 HEAD path with the same token, password
// and file checks as HeadPublicShare. It exists only for older clients.
func (h *Handler) HeadPublicShareLegacy(ctx context.Context, params gen.HeadPublicShareLegacyParams) (gen.HeadPublicShareLegacyRes, error) {
	if h.Shares == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	resolved, err := h.Shares.Resolve(ctx, params.Token, params.XSharePassword.Or(""))
	if err != nil {
		return nil, mapServiceError(err)
	}
	file := resolved.File
	if file.Kind != sqlcgen.FileKindFile || !file.Size.Valid {
		return nil, rejectUndownloadable()
	}
	return &gen.HeadPublicShareLegacyOK{
		AcceptRanges: gen.HeadPublicShareLegacyOKAcceptRanges("bytes"), ContentDisposition: contentDisposition(file.Name, false),
		ContentLength: file.Size.Int64, Etag: contentETag(file), LastModified: file.ModTime.Time,
	}, nil
}

// HeadPublicShareFile resolves one file inside a shared tree and returns its
// download metadata. Files outside the share are rejected by ResolveFile, and
// entries without a non-negative size are reported as 422.
func (h *Handler) HeadPublicShareFile(ctx context.Context, params gen.HeadPublicShareFileParams) (gen.HeadPublicShareFileRes, error) {
	if h.Shares == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	resolved, err := h.Shares.ResolveFile(ctx, params.Token, params.XSharePassword.Or(""), googleUUID(params.FileId))
	if err != nil {
		return nil, mapServiceError(err)
	}
	file := resolved.File
	if file.Kind != sqlcgen.FileKindFile || !file.Size.Valid || file.Size.Int64 < 0 {
		return nil, rejectUndownloadable()
	}
	return &gen.HeadPublicShareFileOK{
		AcceptRanges: gen.HeadPublicShareFileOKAcceptRanges("bytes"), ContentDisposition: contentDisposition(file.Name, false),
		ContentLength: file.Size.Int64, Etag: contentETag(file), LastModified: file.ModTime.Time,
	}, nil
}

// HeadPublicShareFileLegacy serves the pre-v1 HEAD path for one file inside a
// shared tree, applying the same checks as HeadPublicShareFile.
func (h *Handler) HeadPublicShareFileLegacy(ctx context.Context, params gen.HeadPublicShareFileLegacyParams) (gen.HeadPublicShareFileLegacyRes, error) {
	if h.Shares == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	resolved, err := h.Shares.ResolveFile(ctx, params.Token, params.XSharePassword.Or(""), googleUUID(params.FileId))
	if err != nil {
		return nil, mapServiceError(err)
	}
	file := resolved.File
	if file.Kind != sqlcgen.FileKindFile || !file.Size.Valid || file.Size.Int64 < 0 {
		return nil, rejectUndownloadable()
	}
	return &gen.HeadPublicShareFileLegacyOK{
		AcceptRanges: gen.HeadPublicShareFileLegacyOKAcceptRanges("bytes"), ContentDisposition: contentDisposition(file.Name, false),
		ContentLength: file.Size.Int64, Etag: contentETag(file), LastModified: file.ModTime.Time,
	}, nil
}

// loginFlowResponse converts an authn.FlowResult into the generated login
// response, exposing the flow ID, its expiry and whether a two-step password is
// still required.
func loginFlowResponse(flow *authn.FlowResult) gen.TelegramLoginStartResponse {
	return gen.TelegramLoginStartResponse{FlowId: apiUUID(flow.ID), ExpiresAt: flow.ExpiresAt, PasswordRequired: flow.PasswordRequired}
}

// qrLoginFlowResponse converts an authn.QRFlowResult into the generated QR
// response, translating PasswordRequired into the enum state and omitting the QR
// URL and expiry when the flow does not carry them.
func qrLoginFlowResponse(flow *authn.QRFlowResult) gen.TelegramQRLoginResponse {
	state := gen.TelegramQRLoginStatePending
	if flow.PasswordRequired {
		state = gen.TelegramQRLoginStatePasswordRequired
	}
	response := gen.TelegramQRLoginResponse{
		FlowId: apiUUID(flow.ID), ExpiresAt: flow.ExpiresAt, State: state,
	}
	if flow.QRURL != "" {
		response.QrUrl = gen.NewOptString(flow.QRURL)
	}
	if !flow.QRExpiresAt.IsZero() {
		response.QrExpiresAt = gen.NewOptDateTime(flow.QRExpiresAt)
	}
	return response
}

// tokenPairResponse converts an authn.TokenPair into the generated wire model,
// which always advertises the Bearer token type.
func tokenPairResponse(tokens *authn.TokenPair) gen.TokenPair {
	return gen.TokenPair{AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, TokenType: gen.TokenPairTokenTypeBearer, ExpiresIn: tokens.ExpiresIn}
}

// userProfile converts a user row into the API profile, omitting unset display
// name and username and expanding the role into capability strings.
func userProfile(row *sqlcgen.User) gen.UserProfile {
	out := gen.UserProfile{
		UserId: row.UserID, Premium: row.Premium, Role: gen.UserRole(row.Role),
		Capabilities: authn.Capabilities(row.Role), CreatedAt: row.CreatedAt.Time,
	}
	if row.DisplayName.Valid {
		out.DisplayName = gen.NewOptString(row.DisplayName.String)
	}
	if row.Username.Valid {
		out.Username = gen.NewOptString(row.Username.String)
	}
	return out
}

// apiKeySummary converts an API key row into its summary, setting the last-used,
// expiry and revocation timestamps only when their columns are not NULL, so a
// key that was never used or never expires omits those fields. The key hash is
// never exposed.
func apiKeySummary(row *sqlcgen.ApiKey) gen.ApiKeySummary {
	id, _ := dbtypes.GoogleUUID(row.ID)
	out := gen.ApiKeySummary{ID: apiUUID(id), Name: row.Name, CreatedAt: row.CreatedAt.Time}
	if row.LastUsedAt.Valid {
		out.LastUsedAt = gen.NewOptDateTime(row.LastUsedAt.Time)
	}
	if row.ExpiresAt.Valid {
		out.ExpiresAt = gen.NewOptDateTime(row.ExpiresAt.Time)
	}
	if row.RevokedAt.Valid {
		out.RevokedAt = gen.NewOptDateTime(row.RevokedAt.Time)
	}
	return out
}

// botSummary converts a bot row into its API summary, exposing the username only
// once Telegram has reported it.
func botSummary(row *sqlcgen.Bot) gen.BotSummary {
	out := gen.BotSummary{ID: row.BotID, Enabled: row.Enabled, CreatedAt: row.CreatedAt.Time}
	if row.Username.Valid {
		out.Username = gen.NewOptString(row.Username.String)
	}
	return out
}

// channelSummary converts a channel row into its API summary, including the
// selection flag and the last known health state.
func channelSummary(row *sqlcgen.Channel) gen.ChannelSummary {
	return gen.ChannelSummary{
		ID: row.ChannelID, Name: row.Name, Selected: row.Selected,
		Health: gen.ChannelHealth(row.Health), CreatedAt: row.CreatedAt.Time,
	}
}

// shareCreated converts a freshly created share into the API model, including the
// plaintext token and public URL that are only ever returned here.
func shareCreated(created *shares.Created) gen.ShareCreated {
	id, _ := dbtypes.GoogleUUID(created.Row.ID)
	fileID, _ := dbtypes.GoogleUUID(created.Row.FileID)
	out := gen.ShareCreated{
		ID: apiUUID(id), FileId: apiUUID(fileID), Token: created.Token, PublicUrl: gen.URI(created.PublicURL),
		PasswordProtected: created.Row.PasswordHash.Valid, Permission: gen.SharePermission(created.Row.Permission), CreatedAt: created.Row.CreatedAt.Time,
	}
	if created.Row.ExpiresAt.Valid {
		out.ExpiresAt = gen.NewOptDateTime(created.Row.ExpiresAt.Time)
	}
	if created.Row.MaxDownloads.Valid {
		out.MaxDownloads = gen.NewOptInt64(created.Row.MaxDownloads.Int64)
	}
	return out
}

// shareSummary converts a share row into its summary, reporting password
// protection, download count and revocation state without the token.
func shareSummary(row *sqlcgen.FileShare) gen.ShareSummary {
	id, _ := dbtypes.GoogleUUID(row.ID)
	fileID, _ := dbtypes.GoogleUUID(row.FileID)
	out := gen.ShareSummary{
		ID: apiUUID(id), FileId: apiUUID(fileID), PasswordProtected: row.PasswordHash.Valid,
		DownloadCount: row.DownloadCount, Permission: gen.SharePermission(row.Permission), CreatedAt: row.CreatedAt.Time,
	}
	if row.ExpiresAt.Valid {
		out.ExpiresAt = gen.NewOptDateTime(row.ExpiresAt.Time)
	}
	if row.MaxDownloads.Valid {
		out.MaxDownloads = gen.NewOptInt64(row.MaxDownloads.Int64)
	}
	if row.RevokedAt.Valid {
		out.RevokedAt = gen.NewOptDateTime(row.RevokedAt.Time)
	}
	return out
}

// datedUUIDCursor is the keyset cursor payload for pages ordered by creation time
// and UUID. It is JSON-encoded and base64url-encoded by encodeCursor, so renaming
// a field would invalidate cursors issued by older builds.
type datedUUIDCursor struct {
	// CreatedAt is the creation timestamp of the last row on the previous page.
	CreatedAt time.Time `json:"created_at"`
	// ID is the UUID of that row and breaks ties between equal timestamps.
	ID uuid.UUID `json:"id"`
}

// datedInt64Cursor is the keyset cursor payload for pages ordered by creation time
// and a numeric ID such as a bot or channel ID. It is encoded exactly like
// datedUUIDCursor.
type datedInt64Cursor struct {
	// CreatedAt is the creation timestamp of the last row on the previous page.
	CreatedAt time.Time `json:"created_at"`
	// ID is the numeric ID of that row and breaks ties between equal timestamps.
	ID int64 `json:"id"`
}
