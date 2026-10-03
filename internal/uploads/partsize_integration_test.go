//go:build integration

package uploads_test

import (
	"context"
	"errors"
	"testing"

	testpostgres "github.com/tgdrive/teldrive/v2/internal/testutil/postgres"
	"github.com/tgdrive/teldrive/v2/internal/uploads"
)

// TestPartBoundsApplyToCreatedSessionsAgainstRealPostgres is what makes the two
// settings real rather than decorative: the default reaches the stored session,
// and the ceiling is enforced against what a client asks for.
func TestPartBoundsApplyToCreatedSessionsAgainstRealPostgres(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users (user_id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	const ceiling = 64 << 20
	service := uploads.NewService(db.Pool, uploads.Config{
		DefaultPartSize: ceiling, MaxPartSize: ceiling,
	})

	// A session that requests nothing gets the configured default.
	implicit, err := service.Create(ctx, uploads.CreateInput{
		UserID: 1, Name: "implicit.bin", ExpectedSize: 200 << 20,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if implicit.PartSize != ceiling {
		t.Fatalf("part size = %d, want the configured %d", implicit.PartSize, ceiling)
	}

	// An explicit request inside the ceiling is kept as asked.
	explicit, err := service.Create(ctx, uploads.CreateInput{
		UserID: 1, Name: "explicit.bin", ExpectedSize: 200 << 20, PartSize: 32 << 20,
	})
	if err != nil {
		t.Fatalf("Create() with an explicit size error = %v", err)
	}
	if explicit.PartSize != 32<<20 {
		t.Fatalf("part size = %d, want the requested %d", explicit.PartSize, 32<<20)
	}

	// One above the ceiling is refused rather than silently clamped, so a client
	// is never told its upload is smaller than the range it will send.
	if _, err := service.Create(ctx, uploads.CreateInput{
		UserID: 1, Name: "toobig.bin", ExpectedSize: 10 << 20, PartSize: 128 << 20,
	}); !errors.Is(err, uploads.ErrInvalidInput) {
		t.Fatalf("Create() above the ceiling error = %v, want ErrInvalidInput", err)
	}
}

// TestShippedDefaultsStillApplyWithoutConfiguration guards the default path: a
// deployment that sets nothing keeps the 512 MiB parts it has always created,
// even though the ceiling it enforces is now configurable.
func TestShippedDefaultsStillApplyWithoutConfiguration(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users (user_id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	service := uploads.NewService(db.Pool)

	session, err := service.Create(ctx, uploads.CreateInput{
		UserID: 1, Name: "default.bin", ExpectedSize: 2 << 30,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if session.PartSize != 512<<20 {
		t.Fatalf("part size = %d, want the shipped 512MiB", session.PartSize)
	}
}
