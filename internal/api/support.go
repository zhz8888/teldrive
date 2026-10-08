package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/ogen-go/ogen/ogenerrors"

	"github.com/zhz8888/teldrive/v2/internal/api/gen"
	"github.com/zhz8888/teldrive/v2/internal/authn"
	"github.com/zhz8888/teldrive/v2/internal/bots"
	"github.com/zhz8888/teldrive/v2/internal/catalog"
	"github.com/zhz8888/teldrive/v2/internal/channels"
	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/dbtypes"
	"github.com/zhz8888/teldrive/v2/internal/events"
	"github.com/zhz8888/teldrive/v2/internal/fileops"
	"github.com/zhz8888/teldrive/v2/internal/jobs"
	"github.com/zhz8888/teldrive/v2/internal/shares"
	"github.com/zhz8888/teldrive/v2/internal/telegramstore"
	"github.com/zhz8888/teldrive/v2/internal/transfer"
	"github.com/zhz8888/teldrive/v2/internal/uploads"
)

// listPageCap is the largest page the listing services return. Each of them
// silently clamps its own copy of the requested limit to this value, while the
// contract lets a client ask for up to 500; a handler that compared the returned
// row count against the requested limit would therefore never emit a cursor for a
// request above the cap, and the client would mistake a truncated page for the end
// of the listing. Handlers clamp the input once with clampListLimit and then
// compare against that clamped value.
const listPageCap = 200

// clampListLimit returns the page size the listing services will actually apply
// to requested.
func clampListLimit(requested int32) int32 {
	if requested > listPageCap {
		return listPageCap
	}
	return requested
}

// Problem is an error carrying the HTTP status, stable error code and public
// message that should be returned to the client. Handlers build it with problem
// or mapServiceError and the generated router renders it through ErrorHandler,
// while Cause keeps the internal error available for logging and errors.Is.
type Problem struct {
	// Status is the HTTP status code to send. ErrorHandler accepts it only when it
	// falls in the 400-599 range and otherwise keeps its own default.
	Status int
	// Code is the machine-readable error code exposed in the response body, for
	// example "not_found" or "invalid_request". Each code identifies one failure
	// class: a body that cannot be decoded is reported as "malformed_request"
	// (400) and is therefore kept apart from the "invalid_request" (422) of a
	// decoded request that fails validation.
	Code string
	// Message is the human-readable message exposed in the response body; it must
	// stay free of internal details.
	Message string
	// Cause is the wrapped internal error. It is never serialized but is returned
	// by Unwrap so errors.Is and errors.As keep matching the domain error.
	Cause error
}

// Error renders the message together with the cause, so logs contain the
// underlying failure while the response body only carries Message.
func (p *Problem) Error() string {
	if p.Cause == nil {
		return p.Message
	}
	return p.Message + ": " + p.Cause.Error()
}

// Unwrap returns the wrapped cause so errors.Is and errors.As can inspect the
// original domain error.
func (p *Problem) Unwrap() error { return p.Cause }

// problem builds a Problem from the given status, code, message and cause.
// Callers use it directly when an error has no domain sentinel to map.
func problem(status int, code, message string, cause error) error {
	return &Problem{Status: status, Code: code, Message: message, Cause: cause}
}

// mapServiceError converts a domain error into the HTTP Problem that should be
// returned for it, matching sentinels with errors.Is so wrapped errors still map.
//
// It is the single place where status codes are chosen: 401 for authentication
// and share-password failures, 403 forbidden, 404 missing resources, 409 state
// conflicts (including an upload completed before its parts are all stored), 410
// for an expired upload session, Telegram login flow or share, 412 stale
// generations, 413 an oversized Telegram profile photo, 416 unsatisfiable ranges,
// 422 for invalid input, an invalid event cursor, a requested file operation on a
// non-file, an upload body that does not match its declared length, or a hash
// mismatch, 429 too many event streams, 503 an unavailable service or a missing
// encryption key, 504 for a deadline that expired, and 500 as the fallback with
// the cause hidden from the client. Each expired resource keeps its own code, so
// clients can tell an expired upload session from an expired login flow or share.
// Nil and context.Canceled pass through unchanged, because a cancelled request has
// no client left to answer. The original error stays reachable as Cause.
func mapServiceError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return problem(http.StatusGatewayTimeout, "request_timeout", "request timed out", err)
	case errors.Is(err, authn.ErrPasswordInvalid):
		return problem(http.StatusUnauthorized, "telegram_password_invalid", "Telegram two-step password is invalid", err)
	case errors.Is(err, ErrUnauthenticated), errors.Is(err, authn.ErrInvalidCredential), errors.Is(err, authn.ErrUserNotAllowed):
		return problem(http.StatusUnauthorized, "unauthorized", "authentication is required", err)
	case errors.Is(err, shares.ErrPasswordNeeded), errors.Is(err, shares.ErrInvalidPassword):
		return problem(http.StatusUnauthorized, "share_password_required", "a valid share password is required", err)
	case errors.Is(err, shares.ErrForbidden), errors.Is(err, authn.ErrOwnerProtected):
		return problem(http.StatusForbidden, "forbidden", "operation is not permitted", err)
	case errors.Is(err, events.ErrInvalidCursor):
		return problem(http.StatusUnprocessableEntity, "invalid_event_cursor", "event cursor is invalid", err)
	case errors.Is(err, jobs.ErrInvalidCursor):
		return problem(http.StatusUnprocessableEntity, "invalid_cursor", "job cursor is invalid", err)
	case errors.Is(err, jobs.ErrInvalidJobKind):
		return problem(http.StatusUnprocessableEntity, "invalid_job_kind", "job kind is not available on this deployment", err)
	case errors.Is(err, events.ErrTooManyConnections):
		return problem(http.StatusTooManyRequests, "too_many_event_streams", "too many event streams are open", err)
	case errors.Is(err, authn.ErrLoginBusy):
		return problem(http.StatusTooManyRequests, "login_flow_busy", "another request is already working on this login flow", err)
	case errors.Is(err, authn.ErrTooManyAttempts):
		return problem(http.StatusTooManyRequests, "too_many_login_attempts", "too many login attempts, try again later", err)
	case errors.Is(err, channels.ErrAllocationBusy):
		return problem(http.StatusTooManyRequests, "channel_allocation_busy", "another upload is already allocating channel capacity", err)
	case errors.Is(err, shares.ErrTooManyAttempts):
		return problem(http.StatusTooManyRequests, "share_password_throttled", "too many wrong share passwords, try again later", err)
	case errors.Is(err, events.ErrServiceClosed), errors.Is(err, ErrOperationUnavailable), errors.Is(err, transfer.ErrUploadNotConfigured), errors.Is(err, transfer.ErrDownloadNotConfigured), errors.Is(err, transfer.ErrEncryptionKey):
		return problem(http.StatusServiceUnavailable, "service_unavailable", "operation is not available", err)
	case errors.Is(err, catalog.ErrNotFound), errors.Is(err, uploads.ErrNotFound), errors.Is(err, authn.ErrSessionNotFound), errors.Is(err, authn.ErrAPIKeyNotFound), errors.Is(err, authn.ErrUserNotFound), errors.Is(err, bots.ErrNotFound), errors.Is(err, channels.ErrInvalidChannel), errors.Is(err, channels.ErrInvalidOwner), errors.Is(err, shares.ErrNotFound), errors.Is(err, fileops.ErrNotFound):
		return problem(http.StatusNotFound, "not_found", "resource was not found", err)
	case errors.Is(err, uploads.ErrExpired):
		return problem(http.StatusGone, "upload_expired", "upload session has expired", err)
	case errors.Is(err, authn.ErrFlowNotFound):
		return problem(http.StatusGone, "login_flow_expired", "Telegram login flow has expired", err)
	case errors.Is(err, shares.ErrExpired):
		return problem(http.StatusGone, "share_expired", "share has expired", err)
	case errors.Is(err, catalog.ErrConflict), errors.Is(err, catalog.ErrCycle), errors.Is(err, uploads.ErrNameConflict), errors.Is(err, uploads.ErrInvalidState), errors.Is(err, uploads.ErrPartBusy), errors.Is(err, uploads.ErrPartConflict), errors.Is(err, uploads.ErrLeaseLost), errors.Is(err, uploads.ErrIncomplete), errors.Is(err, channels.ErrSelectedChannel), errors.Is(err, channels.ErrChannelInUse), errors.Is(err, channels.ErrChannelUnhealthy), errors.Is(err, channels.ErrChannelFull), errors.Is(err, channels.ErrAutoCreateOff), errors.Is(err, channels.ErrNoSelected), errors.Is(err, jobs.ErrInvalidJobState), errors.Is(err, fileops.ErrNotTrashed):
		return problem(http.StatusConflict, "conflict", "operation conflicts with current state", err)
	case errors.Is(err, catalog.ErrPrecondition):
		return problem(http.StatusPreconditionFailed, "precondition_failed", "resource generation does not match", err)
	case errors.Is(err, telegramstore.ErrProfilePhotoTooLarge):
		return problem(http.StatusRequestEntityTooLarge, "profile_photo_too_large", "Telegram profile photo is too large to serve", err)
	case errors.Is(err, transfer.ErrRangeNotSatisfiable):
		return problem(http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable", "requested byte range is not satisfiable", err)
	case errors.Is(err, uploads.ErrHashMismatch), errors.Is(err, transfer.ErrChecksumMismatch):
		return problem(http.StatusUnprocessableEntity, "hash_mismatch", "content hash does not match", err)
	case errors.Is(err, catalog.ErrInvalidName), errors.Is(err, catalog.ErrNotAFile), errors.Is(err, catalog.ErrInvalidParent), errors.Is(err, catalog.ErrInvalidOwner), errors.Is(err, catalog.ErrInvalidFilter), errors.Is(err, catalog.ErrUnsupportedConflictPolicy), errors.Is(err, uploads.ErrInvalidInput), errors.Is(err, uploads.ErrInvalidParent), errors.Is(err, uploads.ErrInvalidChannel), errors.Is(err, uploads.ErrUnsupportedConflictPolicy), errors.Is(err, transfer.ErrInvalidUpload), errors.Is(err, transfer.ErrInvalidDownload), errors.Is(err, transfer.ErrBodyTooShort), errors.Is(err, transfer.ErrBodyTooLong), errors.Is(err, authn.ErrInvalidInput), errors.Is(err, authn.ErrCodeInvalid), errors.Is(err, authn.ErrLoginStateInvalid), errors.Is(err, authn.ErrPasswordRequired), errors.Is(err, bots.ErrInvalidInput), errors.Is(err, bots.ErrNotBot), errors.Is(err, shares.ErrInvalidInput), errors.Is(err, fileops.ErrInvalidInput):
		return problem(http.StatusUnprocessableEntity, "invalid_request", "request is invalid", err)
	default:
		return problem(http.StatusInternalServerError, "internal_error", "request failed", err)
	}
}

// rejectUndownloadable maps the condition "this entry exists but is not
// downloadable content" — a folder, a trashed row, or a file whose size is
// unknown — to a missing resource. The content endpoints declare 404 rather than
// 422, and the authenticated HEAD path already answers 404, so one condition gets
// one answer wherever it is detected.
func rejectUndownloadable() error {
	return mapServiceError(catalog.ErrNotFound)
}

// ErrorHandler is the ogen error hook: it serializes any error escaping a handler
// as the JSON error envelope with the matching status code.
//
// It derives the status from an *ogenerrors.SecurityError (401) or a request or
// parameter decoding error (400 with the code "malformed_request"), then lets a
// *Problem override status, code and message. Responses of 500 and above are
// logged with the request ID. A cancelled request returns without writing
// anything because the client is already gone.
func ErrorHandler(ctx context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	requestID := middleware.GetReqID(ctx)
	status := http.StatusInternalServerError
	code := "internal_error"
	message := "request failed"
	var securityErr *ogenerrors.SecurityError
	var decodeRequestErr *ogenerrors.DecodeRequestError
	var decodeParamsErr *ogenerrors.DecodeParamsError
	switch {
	case errors.As(err, &securityErr), errors.Is(err, ErrUnauthenticated):
		status = http.StatusUnauthorized
		code = "unauthorized"
		message = "authentication is required"
	case errors.As(err, &decodeRequestErr), errors.As(err, &decodeParamsErr):
		status = http.StatusBadRequest
		code = "malformed_request"
		message = "request could not be decoded"
	case errors.Is(err, ErrOperationUnavailable):
		status = http.StatusServiceUnavailable
		code = "service_unavailable"
		message = "operation is not available"
	}
	var p *Problem
	if errors.As(err, &p) {
		if p.Status >= 400 && p.Status <= 599 {
			status = p.Status
		}
		if p.Code != "" {
			code = p.Code
		}
		if p.Message != "" {
			message = p.Message
		}
	}
	if status >= http.StatusInternalServerError {
		slog.ErrorContext(ctx, "api.request_failed", "status", status, "code", code, "request_id", requestID, "error", err)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code": code, "message": message,
		},
	})
}

// NewServer builds the generated ogen server from the JSON handler, the raw
// handler and the security handler, installing ErrorHandler as the error hook.
// Extra gen.ServerOption values are applied after that hook, so a caller-supplied
// option can still replace it.
func NewServer(handler *Handler, security *Security, opts ...gen.ServerOption) (*gen.Server, error) {
	opts = append([]gen.ServerOption{gen.WithErrorHandler(ErrorHandler)}, opts...)
	return gen.NewServer(handler, NewRawHandler(handler), security, opts...)
}

// generationETag formats a file generation as a strong HTTP entity tag: the
// generation 7 becomes "7". Clients send the value back in If-Match to make an
// update conditional on the revision they last saw.
func generationETag(generation int64) gen.ETag {
	return gen.ETag(fmt.Sprintf(`"%d"`, generation))
}

// parseGenerationETag parses an If-Match header into a generation number. It
// returns a nil generation when the header is absent, meaning the caller did not
// ask for a conditional update, and an error when the value is not a
// non-negative integer. A weak validator prefix (W/) plus surrounding quotes and
// whitespace are tolerated; anything else is rejected.
func parseGenerationETag(value gen.OptETag) (*int64, error) {
	etag, ok := value.Get()
	if !ok {
		return nil, nil
	}
	raw := strings.TrimSpace(string(etag))
	raw = strings.TrimPrefix(raw, "W/")
	raw = strings.Trim(raw, `"`)
	generation, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || generation < 0 {
		return nil, errors.New("invalid generation ETag")
	}
	return &generation, nil
}

// googleUUID converts a contract UUID into the google/uuid representation used by
// the domain services. Both share the same byte layout, so this is a plain cast
// with no validation; use dbtypes.GoogleUUID for a value that comes from a
// database column, where the ok result reports a NULL column.
func googleUUID(value gen.UUID) uuid.UUID { return uuid.UUID(value) }

// apiUUID is the inverse of googleUUID and converts a domain identifier into the
// type the generated API exposes.
func apiUUID(value uuid.UUID) gen.UUID { return gen.UUID(value) }

// optionalAPIUUID converts a nullable database UUID into an optional contract
// field. The value is unset when the column is NULL, so a row without that
// column degrades to a missing field instead of failing the request.
func optionalAPIUUID(value pgtype.UUID) gen.OptUUID {
	id, ok := dbtypes.GoogleUUID(value)
	if !ok {
		return gen.OptUUID{}
	}
	return gen.NewOptUUID(apiUUID(id))
}

// optionalGoogleUUID converts an optional contract UUID into a pointer, returning
// nil when the client did not supply the field. Handlers use that nil case to mean
// "not given", for example a move with no destination parent.
func optionalGoogleUUID(value gen.OptUUID) *uuid.UUID {
	id, ok := value.Get()
	if !ok {
		return nil
	}
	converted := googleUUID(id)
	return &converted
}

// fileEntry maps a database file row to its API representation, filling the
// optional mime type, size and hash only when the columns are set. It fails when
// the stored ID is NULL, which indicates a corrupt row rather than a client error
// and therefore maps to a 500 response.
func fileEntry(file *sqlcgen.File) (gen.FileEntry, error) {
	id, ok := dbtypes.GoogleUUID(file.ID)
	if !ok {
		return gen.FileEntry{}, errors.New("file has invalid id")
	}
	entry := gen.FileEntry{
		ID: apiUUID(id), ParentId: optionalAPIUUID(file.ParentID), Name: file.Name,
		Kind: gen.FileKind(file.Kind), Encryption: file.Encryption,
		Status: gen.FileStatus(file.Status), ModTime: file.ModTime.Time, Generation: file.Generation,
		CreatedAt: file.CreatedAt.Time, UpdatedAt: file.UpdatedAt.Time,
	}
	if file.MimeType.Valid {
		entry.MimeType = gen.NewOptString(file.MimeType.String)
	}
	if file.Size.Valid {
		entry.Size = gen.NewOptInt64(file.Size.Int64)
	}
	if file.HashAlgorithm.Valid && file.HashValue.Valid {
		entry.Hash = gen.NewOptFileHash(gen.FileHash{
			Algorithm: gen.HashAlgorithm(file.HashAlgorithm.String), Value: gen.Checksum(file.HashValue.String),
		})
	}
	return entry, nil
}

// uploadSession maps a database upload session row to its API representation,
// exposing the expected hash, mime type and completion time only when they are
// set. It fails when the stored ID is NULL.
func uploadSession(session *sqlcgen.UploadSession) (gen.UploadSession, error) {
	id, ok := dbtypes.GoogleUUID(session.ID)
	if !ok {
		return gen.UploadSession{}, errors.New("upload has invalid id")
	}
	result := gen.UploadSession{
		ID: apiUUID(id), ParentId: optionalAPIUUID(session.ParentID), Name: session.Name,
		ExpectedSize: session.ExpectedSize, ModTime: session.ModTime.Time,
		Encryption: session.Encryption, ConflictPolicy: gen.NameConflictPolicy(session.ConflictPolicy),
		PartSize: session.PartSize, State: gen.UploadState(session.State), ExpiresAt: session.ExpiresAt.Time,
		CreatedAt: session.CreatedAt.Time, FileId: optionalAPIUUID(session.FileID),
	}
	if session.ExpectedHashAlgorithm.Valid && session.ExpectedHashValue.Valid {
		result.ExpectedHash = gen.NewOptFileHash(gen.FileHash{
			Algorithm: gen.HashAlgorithm(session.ExpectedHashAlgorithm.String), Value: gen.Checksum(session.ExpectedHashValue.String),
		})
	}
	if session.MimeType.Valid {
		result.MimeType = gen.NewOptString(session.MimeType.String)
	}
	if session.CompletedAt.Valid {
		result.CompletedAt = gen.NewOptDateTime(session.CompletedAt.Time)
	}
	return result, nil
}

// uploadPart maps a database upload part row to its API representation, exposing
// the stored size and checksum only once the part has been written to storage. It
// fails when the stored upload ID is NULL.
func uploadPart(part *sqlcgen.UploadPart) (gen.UploadPart, error) {
	uploadID, ok := dbtypes.GoogleUUID(part.UploadID)
	if !ok {
		return gen.UploadPart{}, errors.New("upload part has invalid upload id")
	}
	result := gen.UploadPart{
		UploadId: apiUUID(uploadID), PartNo: part.PartNo, State: gen.UploadPartState(part.State),
		PlainSize: part.PlainSize, CreatedAt: part.CreatedAt.Time, UpdatedAt: part.UpdatedAt.Time,
	}
	if part.StoredSize.Valid {
		result.StoredSize = gen.NewOptInt64(part.StoredSize.Int64)
	}
	if part.Checksum.Valid {
		result.Checksum = gen.NewOptChecksum(gen.Checksum(part.Checksum.String))
	}
	return result, nil
}

// encodeCursor serializes an opaque pagination cursor as URL-safe base64 JSON. It
// returns an unset optional value when the payload cannot be marshalled, which
// callers treat as "no next page" rather than an error.
func encodeCursor(value any) gen.OptCursor {
	data, err := json.Marshal(value)
	if err != nil {
		return gen.OptCursor{}
	}
	return gen.NewOptCursor(gen.Cursor(base64.RawURLEncoding.EncodeToString(data)))
}

// decodeCursor decodes a cursor produced by encodeCursor into target. An unset
// cursor leaves target untouched and reports no error, so the first page needs no
// special case; malformed base64 or JSON yields a generic "invalid cursor" error,
// which every caller answers with 422: most translate it into an invalid-input
// sentinel that mapServiceError maps there, while the channel and job listings
// build the problem directly so they can keep a cursor-specific code.
func decodeCursor(value gen.OptCursor, target any) error {
	cursor, ok := value.Get()
	if !ok {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(string(cursor))
	if err != nil {
		return errors.New("invalid cursor")
	}
	if err := json.Unmarshal(data, target); err != nil {
		return errors.New("invalid cursor")
	}
	return nil
}

// fileCursor is the decoded form of a file listing cursor. It records the sort key
// of the last item on the previous page together with the sort mode, so a request
// that changes sort or order can be rejected instead of silently returning a
// nonsensical page.
type fileCursor struct {
	// Name is the last item's name, recorded for every sort mode. It supplies the
	// cursor text only when Sort is "name"; for the other sort modes the key comes
	// from Value. The tie-breaker for every sort mode is ID.
	Name string `json:"name,omitempty"`
	// Sort is the sort column the page was produced with, for example "name",
	// "size" or "updatedAt".
	Sort string `json:"sort,omitempty"`
	// Order is the direction, "asc" or "desc", the page was produced with.
	Order string `json:"order,omitempty"`
	// Value is the encoded sort key of the last item — an RFC 3339 timestamp, a
	// byte size or a UUID string depending on Sort — and repeats Name when the
	// listing is ordered by name.
	Value string `json:"value,omitempty"`
	// ID is the UUID of the last item on the page and is required: the zero UUID
	// means no usable cursor was supplied.
	ID uuid.UUID `json:"id"`
	// Scope is the listing scope the page was produced with, one of "folder",
	// "drive" or "recursive". A cursor is refused when the request asks for
	// another scope, so a page cannot be continued under different rules.
	Scope string `json:"scope,omitempty"`
	// Fingerprint is a digest of the listing parameters the page was produced
	// with — scope, folder, search text and type, sort, filters and windows — so
	// a cursor stops working as soon as any of them changes.
	Fingerprint string `json:"fingerprint,omitempty"`
	// FolderID is the folder a recursive listing was rooted at, and stays empty
	// for the folder and drive scopes.
	FolderID string `json:"folder_id,omitempty"`
}

// uploadCursor is the decoded form of an upload listing cursor. Both fields come
// from the last session of the previous page and together form the sort key.
type uploadCursor struct {
	// CreatedAt is the creation timestamp of the last session on the page.
	CreatedAt time.Time `json:"created_at"`
	// ID is the UUID of that session; the zero UUID means no usable cursor was
	// supplied.
	ID uuid.UUID `json:"id"`
}

// partCursor is the decoded form of an upload part listing cursor.
type partCursor struct {
	// PartNo is the part number of the last part on the page, so the next page
	// starts after it; zero means no usable cursor was supplied.
	PartNo int32 `json:"part_no"`
}

// byteRange is a resolved byte range of a download: Offset is the first byte to
// send and Length is the number of bytes, not the inclusive end offset.
type byteRange struct {
	// Offset and Length are the zero-based start and the byte count of the range;
	// a full-file request is Offset 0 with Length equal to the file size.
	Offset, Length int64
}

// parseRange resolves an optional Range header against a known file size. It
// returns the byte range to send, whether the client explicitly asked for a range
// (and therefore expects 206 or 416 semantics), and transfer.ErrRangeNotSatisfiable
// for anything it cannot serve.
//
// A missing or blank header yields the whole file. Only a single "bytes=" range is
// accepted: the suffix form ("bytes=-N") and the open-ended form ("bytes=N-") are
// supported and an end beyond the file size is clamped to the last byte, while
// multiple ranges, a start at or past the end, or a negative file size are
// rejected. A negative size also reports the range as requested when the client
// sent one.
func parseRange(value gen.OptString, size int64) (byteRange, bool, error) {
	if size < 0 {
		return byteRange{}, value.IsSet(), transfer.ErrRangeNotSatisfiable
	}
	raw, ok := value.Get()
	if !ok || strings.TrimSpace(raw) == "" {
		return byteRange{Offset: 0, Length: size}, false, nil
	}
	if !strings.HasPrefix(raw, "bytes=") || strings.Contains(raw, ",") {
		return byteRange{}, true, transfer.ErrRangeNotSatisfiable
	}
	parts := strings.SplitN(strings.TrimPrefix(raw, "bytes="), "-", 2)
	if len(parts) != 2 {
		return byteRange{}, true, transfer.ErrRangeNotSatisfiable
	}
	if parts[0] == "" {
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 || size == 0 {
			return byteRange{}, true, transfer.ErrRangeNotSatisfiable
		}
		if suffix > size {
			suffix = size
		}
		return byteRange{Offset: size - suffix, Length: suffix}, true, nil
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return byteRange{}, true, transfer.ErrRangeNotSatisfiable
	}
	if parts[1] == "" {
		return byteRange{Offset: start, Length: size - start}, true, nil
	}
	end, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || end < start {
		return byteRange{}, true, transfer.ErrRangeNotSatisfiable
	}
	if end >= size {
		end = size - 1
	}
	return byteRange{Offset: start, Length: end - start + 1}, true, nil
}

// contentDisposition renders the Content-Disposition header for a file name,
// choosing attachment when the client asked for a download and inline otherwise.
// mime.FormatMediaType percent-encodes any name as an RFC 2231 parameter and
// returns an empty value only for a media type or parameter name that is not a
// token, so the fallback to the bare disposition is unreachable for the
// hard-coded disposition and the "filename" parameter used here.
func contentDisposition(name string, attachment bool) string {
	disposition := "inline"
	if attachment {
		disposition = "attachment"
	}
	value := mime.FormatMediaType(disposition, map[string]string{"filename": name})
	if value == "" {
		return disposition
	}
	return value
}
