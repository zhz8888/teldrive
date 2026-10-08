package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/uuid"

	"github.com/zhz8888/teldrive/v2/internal/api/gen"
	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/dbtypes"
	"github.com/zhz8888/teldrive/v2/internal/transfer"
)

// RawHandler implements ogen raw response operations. The generated router owns
// routing, parameter decoding, security, and error serialization; this type only
// writes successful streaming responses directly to the ResponseWriter.
type RawHandler struct {
	// handler supplies the services and the access checks shared with the JSON
	// handlers; it is never nil when the value came from NewRawHandler.
	handler *Handler
}

// NewRawHandler returns the raw-response handler for a Handler. The raw handler
// borrows the Handler and its services, so it must not outlive the server that
// owns them.
func NewRawHandler(handler *Handler) *RawHandler {
	return &RawHandler{handler: handler}
}

// DownloadFile streams a file the caller is allowed to read, honouring a single
// Range request and answering 304 for a full-file request whose If-None-Match
// matches the content ETag. Authentication is enforced by the generated security
// layer; the body is written directly, so a failure after the headers are
// committed can only be logged.
func (h *RawHandler) DownloadFile(ctx context.Context, params gen.DownloadFileParams, w http.ResponseWriter) error {
	if h.handler == nil || h.handler.Catalog == nil || h.handler.Downloader == nil {
		return mapServiceError(ErrOperationUnavailable)
	}
	fileID := googleUUID(params.FileId)
	access, err := h.handler.resolveAuthenticatedFileAccess(ctx, fileID, false)
	if err != nil {
		return mapServiceError(err)
	}
	file, err := h.handler.Catalog.Get(ctx, access.OwnerID, fileID)
	if err != nil {
		return mapServiceError(err)
	}
	return h.streamFile(ctx, w, access.OwnerID, fileID, file, params.Range, params.IfNoneMatch, params.Download.IsSet())
}

// DownloadFileLegacy serves the download URL form that omits the filename
// segment; it forwards to DownloadFile with the same options, so both routes
// share one implementation and one set of semantics.
func (h *RawHandler) DownloadFileLegacy(ctx context.Context, params gen.DownloadFileLegacyParams, w http.ResponseWriter) error {
	return h.DownloadFile(ctx, gen.DownloadFileParams{
		Range: params.Range, IfNoneMatch: params.IfNoneMatch, Download: params.Download, FileId: params.FileId,
	}, w)
}

// DownloadPublicShare streams the file a public share token points at and is
// reachable without authentication. The optional share password is verified by
// the shares service, and a download reservation is consumed before any bytes
// are sent so concurrent requests cannot exceed the share's download limit.
func (h *RawHandler) DownloadPublicShare(ctx context.Context, params gen.DownloadPublicShareParams, w http.ResponseWriter) error {
	if h.handler == nil || h.handler.Shares == nil || h.handler.Downloader == nil {
		return mapServiceError(ErrOperationUnavailable)
	}
	password := params.XSharePassword.Or("")
	resolved, err := h.handler.Shares.Resolve(ctx, params.Token, password)
	if err != nil {
		return mapServiceError(err)
	}
	file := resolved.File
	etag := contentETag(file)
	if !params.Range.IsSet() && ifNoneMatch(params.IfNoneMatch) == string(etag) {
		w.Header().Set("ETag", string(etag))
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	resolved, err = h.handler.Shares.ReserveResolvedDownload(ctx, resolved)
	if err != nil {
		return mapServiceError(err)
	}
	file = resolved.File
	fileID, ok := dbtypes.GoogleUUID(file.ID)
	if !ok {
		return mapServiceError(transfer.ErrInvalidDownload)
	}
	return h.streamFile(ctx, w, resolved.Share.OwnerID, fileID, file, params.Range, params.IfNoneMatch, params.Download.IsSet())
}

// DownloadPublicShareLegacy serves the public share URL form that omits the
// filename segment and forwards to DownloadPublicShare.
func (h *RawHandler) DownloadPublicShareLegacy(ctx context.Context, params gen.DownloadPublicShareLegacyParams, w http.ResponseWriter) error {
	return h.DownloadPublicShare(ctx, gen.DownloadPublicShareParams{
		XSharePassword: params.XSharePassword, Range: params.Range, IfNoneMatch: params.IfNoneMatch,
		Download: params.Download, Token: params.Token,
	}, w)
}

// DownloadPublicShareFile streams a single file from inside a shared folder and
// requires the file to live in the shared subtree. Like DownloadPublicShare it
// reserves a download before streaming and honours Range and If-None-Match.
func (h *RawHandler) DownloadPublicShareFile(ctx context.Context, params gen.DownloadPublicShareFileParams, w http.ResponseWriter) error {
	if h.handler == nil || h.handler.Shares == nil || h.handler.Downloader == nil {
		return mapServiceError(ErrOperationUnavailable)
	}
	password := params.XSharePassword.Or("")
	fileID := googleUUID(params.FileId)
	resolved, err := h.handler.Shares.ResolveFile(ctx, params.Token, password, fileID)
	if err != nil {
		return mapServiceError(err)
	}
	file := resolved.File
	etag := contentETag(file)
	if !params.Range.IsSet() && ifNoneMatch(params.IfNoneMatch) == string(etag) {
		w.Header().Set("ETag", string(etag))
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	resolved, err = h.handler.Shares.ReserveFileDownload(ctx, params.Token, password, fileID)
	if err != nil {
		return mapServiceError(err)
	}
	return h.streamFile(ctx, w, resolved.Share.OwnerID, fileID, resolved.File, params.Range, params.IfNoneMatch, params.Download.IsSet())
}

// DownloadPublicShareFileLegacy serves the public single-file URL form that omits
// the filename segment and forwards to DownloadPublicShareFile.
func (h *RawHandler) DownloadPublicShareFileLegacy(ctx context.Context, params gen.DownloadPublicShareFileLegacyParams, w http.ResponseWriter) error {
	return h.DownloadPublicShareFile(ctx, gen.DownloadPublicShareFileParams{
		XSharePassword: params.XSharePassword, Range: params.Range, IfNoneMatch: params.IfNoneMatch,
		Download: params.Download, Token: params.Token, FileId: params.FileId,
	}, w)
}

// streamFile validates that the file is an active regular file with a known size,
// resolves the requested byte range and writes one download to w.
//
// It answers 304 when the ETag matches a full-file request, sets the range,
// length, disposition and caching headers, and copies exactly the number of bytes
// announced in Content-Length. Once the status line is written the response
// cannot be replaced, so a failed copy is only visible to the client as a short
// body and is logged instead; the content reader is always closed.
func (h *RawHandler) streamFile(ctx context.Context, w http.ResponseWriter, userID int64, fileID uuid.UUID, file *sqlcgen.File, rangeValue gen.OptString, noneMatch gen.OptETag, attachment bool) error {
	if file.Kind != sqlcgen.FileKindFile || file.Status != sqlcgen.FileStatusActive || !file.Size.Valid || file.Size.Int64 < 0 {
		return rejectUndownloadable()
	}
	rangeSpec, partial, err := parseRange(rangeValue, file.Size.Int64)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", file.Size.Int64))
		return mapServiceError(err)
	}
	etag := contentETag(file)
	if !partial && ifNoneMatch(noneMatch) == string(etag) {
		w.Header().Set("ETag", string(etag))
		w.WriteHeader(http.StatusNotModified)
		return nil
	}

	download, err := h.handler.Downloader.Open(ctx, transfer.DownloadRequest{
		UserID: userID, FileID: fileID, Offset: rangeSpec.Offset, Length: rangeSpec.Length,
	})
	if err != nil {
		return mapServiceError(err)
	}
	defer download.Reader.Close()

	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rangeSpec.Offset, rangeSpec.Offset+rangeSpec.Length-1, download.TotalSize))
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Disposition", contentDisposition(file.Name, attachment))
	w.Header().Set("Content-Length", strconv.FormatInt(rangeSpec.Length, 10))
	w.Header().Set("Content-Type", download.ContentType)
	w.Header().Set("ETag", string(etag))
	w.Header().Set("Last-Modified", file.ModTime.Time.UTC().Format(http.TimeFormat))
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(status)
	_, err = io.CopyN(w, download.Reader, rangeSpec.Length)
	if err != nil && !isExpectedStreamEnd(ctx, err) {
		// Headers are already committed, so a JSON error response cannot replace
		// this stream. The Content-Length mismatch tells the client it was truncated.
		slog.ErrorContext(ctx, "api.stream_failed", "file_id", fileID, "offset", rangeSpec.Offset, "length", rangeSpec.Length, "error", err)
	}
	return nil
}

// isExpectedStreamEnd reports whether a copy error is the client going away (the
// request context was cancelled or the connection was reset) rather than a
// storage or network failure worth logging at error level.
func isExpectedStreamEnd(ctx context.Context, err error) bool {
	return ctx.Err() != nil || isClientDisconnect(err)
}

// isClientDisconnect reports whether err is one of the write errors a client
// disconnect produces: a cancelled context, ECONNRESET or EPIPE.
func isClientDisconnect(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE)
}

// ifNoneMatch returns the trimmed If-None-Match header value, or an empty string
// when the header is absent. Only the exact-match form is understood, which is
// what the streaming handlers compare against the content ETag.
func ifNoneMatch(value gen.OptETag) string {
	if value, ok := value.Get(); ok {
		return strings.TrimSpace(string(value))
	}
	return ""
}
