package shares

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

func TestServiceValidationAndTokenHash(t *testing.T) {
	t.Parallel()
	if _, err := NewService(nil, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("NewService(nil) error = %v", err)
	}
	s := &Service{now: time.Now}
	past := time.Now().Add(-time.Minute)
	zero := int64(0)
	for _, input := range []CreateInput{
		{},
		{OwnerID: 1, FileID: uuid.New(), ExpiresAt: &past},
		{OwnerID: 1, FileID: uuid.New(), MaxDownloads: &zero},
	} {
		if _, err := s.Create(context.Background(), input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("Create(%#v) error = %v", input, err)
		}
	}
	if _, err := s.List(context.Background(), ListInput{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("List() error = %v", err)
	}
	if err := s.Revoke(context.Background(), 0, uuid.Nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Revoke() error = %v", err)
	}
	if _, err := s.resolveRow(context.Background(), "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolveRow(empty) error = %v", err)
	}
	if len(tokenHash("token")) != 32 {
		t.Fatal("tokenHash must be SHA-256")
	}
}

// TestHashSharePasswordValidation checks that a password bcrypt cannot hash is
// rejected as invalid input instead of surfacing as a server error, and that the
// stored digest matches the trimmed password.
func TestHashSharePasswordValidation(t *testing.T) {
	t.Parallel()
	for _, password := range []string{"", "   ", strings.Repeat("x", maxSharePasswordLength+1)} {
		if _, err := hashSharePassword(password); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("hashSharePassword(%d bytes) error = %v", len(password), err)
		}
	}
	digest, err := hashSharePassword("  secret  ")
	if err != nil {
		t.Fatalf("hashSharePassword() error = %v", err)
	}
	if digest == nil {
		t.Fatal("hashSharePassword() returned no digest")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(*digest), []byte("secret")); err != nil {
		t.Fatalf("stored digest does not match the trimmed password: %v", err)
	}
}
