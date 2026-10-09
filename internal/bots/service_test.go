package bots

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestServiceValidationHelpers(t *testing.T) {
	t.Parallel()
	if _, err := NewService(nil, nil, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("NewService() error = %v", err)
	}
	s := &Service{}
	if _, err := s.Create(context.Background(), 0, "token"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Create(user=0) error = %v", err)
	}
	if _, err := s.Create(context.Background(), 1, " "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Create(empty) error = %v", err)
	}
	if _, err := s.List(context.Background(), ListInput{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("List() error = %v", err)
	}
	if err := s.Delete(context.Background(), 0, 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := s.VerifyPending(context.Background(), 0, 1); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("VerifyPending(user=0) error = %v", err)
	}
	if _, err := s.VerifyPending(context.Background(), 1, 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("VerifyPending(bot=0) error = %v", err)
	}
	if _, err := s.ActivateVerified(context.Background(), 0, 1, "bot"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ActivateVerified(user=0) error = %v", err)
	}
	if _, err := s.ActivateVerified(context.Background(), 1, 0, "bot"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ActivateVerified(bot=0) error = %v", err)
	}
	// Activation stores the username Telegram reported, so a blank one would write
	// a row no caller could address the bot by.
	if _, err := s.ActivateVerified(context.Background(), 1, 1, "  "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ActivateVerified(blank username) error = %v", err)
	}
	if nonEmpty(" ") != nil || nonEmpty(" bot ") == nil {
		t.Fatal("nonEmpty validation failed")
	}
	_ = uuid.Nil
}
