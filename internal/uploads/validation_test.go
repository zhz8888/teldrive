package uploads

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/treehash"
)

func TestServiceRejectsInvalidInputsBeforeDatabaseAccess(t *testing.T) {
	t.Parallel()
	svc := NewService(nil)
	ctx := context.Background()
	id := uuid.New()
	algorithm := "blake3"
	keyVersion := int32(1)

	tests := []struct {
		name string
		call func() error
		want error
	}{
		{name: "create owner", call: func() error { _, err := svc.Create(ctx, CreateInput{Name: "x"}); return err }, want: ErrInvalidInput},
		{name: "create size", call: func() error {
			_, err := svc.Create(ctx, CreateInput{UserID: 1, Name: "x", ExpectedSize: -2})
			return err
		}, want: ErrInvalidInput},
		{name: "create blank name", call: func() error {
			_, err := svc.Create(ctx, CreateInput{UserID: 1, Name: " \t "})
			return err
		}, want: ErrInvalidInput},
		{name: "create hash pair", call: func() error {
			_, err := svc.Create(ctx, CreateInput{UserID: 1, Name: "x", ExpectedHashAlgorithm: &algorithm})
			return err
		}, want: ErrInvalidInput},
		{name: "create missing key version", call: func() error {
			_, err := svc.Create(ctx, CreateInput{UserID: 1, Name: "x", Encryption: true})
			return err
		}, want: ErrInvalidInput},
		{name: "create unexpected key version", call: func() error {
			_, err := svc.Create(ctx, CreateInput{UserID: 1, Name: "x", Encryption: false, EncryptionKeyVersion: &keyVersion})
			return err
		}, want: ErrInvalidInput},
		{name: "create bad policy", call: func() error {
			_, err := svc.Create(ctx, CreateInput{UserID: 1, Name: "x", ConflictPolicy: "bad"})
			return err
		}, want: ErrUnsupportedConflictPolicy},
		{name: "get owner", call: func() error { _, err := svc.Get(ctx, 0, id); return err }, want: ErrInvalidInput},
		{name: "claim basic", call: func() error { _, err := svc.ClaimPart(ctx, ClaimPartInput{}); return err }, want: ErrInvalidInput},
		{name: "store basic", call: func() error { _, err := svc.StorePart(ctx, StorePartInput{}); return err }, want: ErrInvalidInput},
		{name: "complete owner", call: func() error { _, err := svc.Complete(ctx, 0, id); return err }, want: ErrInvalidInput},
		{name: "abort owner", call: func() error { _, err := svc.Abort(ctx, 0, id); return err }, want: ErrInvalidInput},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestValidatePartShapeRejectsUnaddressableParts checks that a part number outside
// the bound is refused for a session of known and unknown size alike. A part number
// that huge used to be accepted and then multiplied into an offset, which could wrap
// into a valid-looking range and pin the session at a part completion can never
// accept.
func TestValidatePartShapeRejectsUnaddressableParts(t *testing.T) {
	t.Parallel()
	for _, session := range []*sqlcgen.UploadSession{
		{PartSize: 1 << 30, ExpectedSize: 4 << 30},
		{PartSize: 1 << 30, ExpectedSize: -1},
	} {
		if err := validatePartShape(session, math.MaxInt32, 1<<30); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("validatePartShape(partNo = MaxInt32) error = %v, want ErrInvalidInput", err)
		}
		if err := validatePartShape(session, 0, 1<<30); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("validatePartShape(partNo = 0) error = %v, want ErrInvalidInput", err)
		}
	}
	known := &sqlcgen.UploadSession{PartSize: 1 << 30, ExpectedSize: 2 << 30}
	if err := validatePartShape(known, 2, 1<<30); err != nil {
		t.Fatalf("validatePartShape(last part of a known size) error = %v", err)
	}
}

func TestNormalizeExpectedHash(t *testing.T) {
	t.Parallel()
	valid := strings.Repeat("ab", treehash.DigestSize)
	algorithm, value, err := normalizeExpectedHash(" BLAKE3 ", " "+strings.ToUpper(valid)+" ")
	if err != nil {
		t.Fatalf("normalizeExpectedHash() error = %v", err)
	}
	if algorithm != "blake3" || value != valid {
		t.Fatalf("normalizeExpectedHash() = %q, %q", algorithm, value)
	}
	for _, test := range []struct {
		algorithm string
		value     string
	}{
		{"sha256", valid},
		{"blake3", "not-hex"},
		{"blake3", "abcd"},
	} {
		if _, _, err := normalizeExpectedHash(test.algorithm, test.value); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("normalizeExpectedHash(%q, %q) error = %v", test.algorithm, test.value, err)
		}
	}
}
