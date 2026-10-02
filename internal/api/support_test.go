package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ogen-go/ogen/ogenerrors"

	"github.com/tgdrive/teldrive/v2/internal/api/gen"
	"github.com/tgdrive/teldrive/v2/internal/authn"
	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/channels"
	"github.com/tgdrive/teldrive/v2/internal/jobs"
	"github.com/tgdrive/teldrive/v2/internal/shares"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
	"github.com/tgdrive/teldrive/v2/internal/transfer"
	"github.com/tgdrive/teldrive/v2/internal/uploads"
)

func TestMapServiceErrorContextLifecycle(t *testing.T) {
	t.Parallel()

	if err := mapServiceError(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}

	err := mapServiceError(context.DeadlineExceeded)
	var problem *Problem
	if !errors.As(err, &problem) {
		t.Fatalf("deadline error = %T, want *Problem", err)
	}
	if problem.Status != http.StatusGatewayTimeout || problem.Code != "request_timeout" {
		t.Fatalf("deadline problem = %#v", problem)
	}
}

func TestMapServiceErrorTelegramPasswordInvalid(t *testing.T) {
	t.Parallel()

	err := mapServiceError(authn.ErrPasswordInvalid)
	var problem *Problem
	if !errors.As(err, &problem) {
		t.Fatalf("password error = %T, want *Problem", err)
	}
	if problem.Status != http.StatusUnauthorized || problem.Code != "telegram_password_invalid" || problem.Message != "Telegram two-step password is invalid" {
		t.Fatalf("password problem = %#v", problem)
	}
}

func TestParseRangeResolvesFullContentLength(t *testing.T) {
	t.Parallel()

	got, partial, err := parseRange(gen.OptString{}, 557123)
	if err != nil {
		t.Fatalf("parseRange() error = %v", err)
	}
	if partial || got.Offset != 0 || got.Length != 557123 {
		t.Fatalf("parseRange() = (%+v, %t), want full content", got, partial)
	}
}

func TestContentDisposition(t *testing.T) {
	t.Parallel()

	if got := contentDisposition("report 2026.pdf", false); got != `inline; filename="report 2026.pdf"` {
		t.Fatalf("inline disposition = %q", got)
	}
	if got := contentDisposition("report 2026.pdf", true); got != `attachment; filename="report 2026.pdf"` {
		t.Fatalf("attachment disposition = %q", got)
	}
}

func TestErrorHandlerIgnoresClientCancellation(t *testing.T) {
	t.Parallel()

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/files", nil)
	ErrorHandler(request.Context(), response, request, context.Canceled)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want untouched recorder status %d", response.Code, http.StatusOK)
	}
	if response.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", response.Body.String())
	}
}

// TestMapServiceErrorStatuses pins the status and code of the sentinels whose
// mapping this package owns, including the three distinct expired-resource codes
// that used to share "upload_expired".
func TestMapServiceErrorStatuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "expired upload session", err: uploads.ErrExpired, wantStatus: http.StatusGone, wantCode: "upload_expired"},
		{name: "expired login flow", err: authn.ErrFlowNotFound, wantStatus: http.StatusGone, wantCode: "login_flow_expired"},
		{name: "expired share", err: shares.ErrExpired, wantStatus: http.StatusGone, wantCode: "share_expired"},
		{name: "unknown account", err: authn.ErrUserNotFound, wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "invalid channel owner", err: channels.ErrInvalidOwner, wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "owner protected", err: authn.ErrOwnerProtected, wantStatus: http.StatusForbidden, wantCode: "forbidden"},
		{name: "unhealthy channel", err: channels.ErrChannelUnhealthy, wantStatus: http.StatusConflict, wantCode: "conflict"},
		{name: "full channel", err: channels.ErrChannelFull, wantStatus: http.StatusConflict, wantCode: "conflict"},
		{name: "automatic creation disabled", err: channels.ErrAutoCreateOff, wantStatus: http.StatusConflict, wantCode: "conflict"},
		{name: "no selected channel", err: channels.ErrNoSelected, wantStatus: http.StatusConflict, wantCode: "conflict"},
		{name: "invalid job state", err: jobs.ErrInvalidJobState, wantStatus: http.StatusConflict, wantCode: "conflict"},
		{name: "missing encryption key", err: transfer.ErrEncryptionKey, wantStatus: http.StatusServiceUnavailable, wantCode: "service_unavailable"},
		{name: "invalid login state", err: authn.ErrLoginStateInvalid, wantStatus: http.StatusUnprocessableEntity, wantCode: "invalid_request"},
		{name: "password required", err: authn.ErrPasswordRequired, wantStatus: http.StatusUnprocessableEntity, wantCode: "invalid_request"},
		{name: "invalid list filter", err: catalog.ErrInvalidFilter, wantStatus: http.StatusUnprocessableEntity, wantCode: "invalid_request"},
		{name: "unsupported conflict policy", err: catalog.ErrUnsupportedConflictPolicy, wantStatus: http.StatusUnprocessableEntity, wantCode: "invalid_request"},
		{name: "profile photo too large", err: telegramstore.ErrProfilePhotoTooLarge, wantStatus: http.StatusRequestEntityTooLarge, wantCode: "profile_photo_too_large"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := mapServiceError(test.err)
			var problem *Problem
			if !errors.As(err, &problem) {
				t.Fatalf("mapServiceError(%v) = %T, want *Problem", test.err, err)
			}
			if problem.Status != test.wantStatus || problem.Code != test.wantCode {
				t.Fatalf("mapServiceError(%v) = (%d, %q), want (%d, %q)", test.err, problem.Status, problem.Code, test.wantStatus, test.wantCode)
			}
			if !errors.Is(err, test.err) {
				t.Fatalf("mapServiceError(%v) lost the cause", test.err)
			}
		})
	}
}

// TestErrorHandlerDecodeErrorCode checks that a body or parameter that cannot be
// decoded is reported as "malformed_request", so the 400 stays distinguishable
// from the 422 "invalid_request" of failed validation.
func TestErrorHandlerDecodeErrorCode(t *testing.T) {
	t.Parallel()

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/uploads", nil)
	decodeErr := &ogenerrors.DecodeRequestError{
		OperationContext: ogenerrors.OperationContext{Name: "createUpload", ID: "createUpload"},
		Err:              errors.New("unexpected EOF"),
	}
	ErrorHandler(request.Context(), response, request, decodeErr)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
	if body.Error.Code != "malformed_request" {
		t.Fatalf("code = %q, want malformed_request", body.Error.Code)
	}
}
