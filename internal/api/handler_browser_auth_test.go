package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zhz8888/teldrive/v2/internal/authn"
	"github.com/zhz8888/teldrive/v2/internal/principal"
)

func TestCookieSecureFromContext(t *testing.T) {
	t.Parallel()
	handler := &Handler{}
	insecure := handler.cookie(context.Background(), "name", "value", time.Minute).String()
	if strings.Contains(insecure, "Secure") {
		t.Fatalf("insecure cookie = %q", insecure)
	}
	secure := handler.cookie(WithCookieSecure(context.Background(), true), "name", "value", time.Minute).String()
	if !strings.Contains(secure, "Secure") {
		t.Fatalf("secure cookie = %q", secure)
	}
}

// TestLogoutCookieSessionRejectsSessionlessCredential covers an API key calling
// logout: the credential is not tied to a login session, so the request is
// refused with 401 instead of the 404 that ErrSessionNotFound would produce.
func TestLogoutCookieSessionRejectsSessionlessCredential(t *testing.T) {
	t.Parallel()

	handler := &Handler{Auth: &authn.Service{}}
	ctx := principal.WithIdentity(context.Background(), principal.Identity{UserID: 1001, Source: "api_key"})
	_, err := handler.LogoutCookieSession(ctx)

	var problem *Problem
	if !errors.As(err, &problem) {
		t.Fatalf("error = %T, want *Problem", err)
	}
	if problem.Status != http.StatusUnauthorized || problem.Code != "unauthorized" {
		t.Fatalf("problem = (%d, %q), want (401, unauthorized)", problem.Status, problem.Code)
	}
}
