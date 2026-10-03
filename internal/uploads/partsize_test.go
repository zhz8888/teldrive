package uploads

import (
	"testing"
)

// TestNewServiceStoresTheConfiguredPartBounds is the point of the two settings:
// a host that cannot afford the shipped 512 MiB default lowers it, and every
// session that does not request a size then gets the smaller one.
//
// The bounds are read from the service rather than observed through Create,
// because Create needs a pool and a nil one panics rather than reporting
// anything; the database-backed behaviour is covered by
// TestPartBoundsApplyToCreatedSessionsAgainstRealPostgres.
func TestNewServiceStoresTheConfiguredPartBounds(t *testing.T) {
	t.Parallel()
	const size = 64 << 20
	service := NewService(nil, Config{DefaultPartSize: size, MaxPartSize: size})

	if service.defaultPartSize != size {
		t.Fatalf("default part size = %d, want the configured %d", service.defaultPartSize, size)
	}
	if service.maxPartSize != size {
		t.Fatalf("max part size = %d, want the configured %d", service.maxPartSize, size)
	}
}

// TestNewServiceFallsBackToTheShippedDefaults keeps a caller that passes nothing
// on the behaviour it has always had, so adding the settings cannot silently
// change an existing deployment.
func TestNewServiceFallsBackToTheShippedDefaults(t *testing.T) {
	t.Parallel()
	for _, service := range []*Service{NewService(nil), NewService(nil, Config{})} {
		if service.defaultPartSize != defaultPartSize {
			t.Fatalf("default part size = %d, want %d", service.defaultPartSize, defaultPartSize)
		}
		if service.maxPartSize != maxPartSize {
			t.Fatalf("max part size = %d, want %d", service.maxPartSize, maxPartSize)
		}
		if service.sessionTTL != defaultSessionTTL {
			t.Fatalf("session TTL = %s, want %s", service.sessionTTL, defaultSessionTTL)
		}
	}
}

// TestNewServiceNeverLetsTheCeilingSitUnderTheDefault covers the pair a
// misconfiguration can produce. A cap below the default would refuse every
// session that asks for neither, which looks like the upload path being broken
// rather than like a configuration error, so the ceiling is raised instead.
func TestNewServiceNeverLetsTheCeilingSitUnderTheDefault(t *testing.T) {
	t.Parallel()
	service := NewService(nil, Config{DefaultPartSize: 128 << 20, MaxPartSize: 64 << 20})
	if service.maxPartSize != 128<<20 {
		t.Fatalf("max part size = %d, want it raised to the default", service.maxPartSize)
	}
}

// TestNewServiceKeepsTheShippedCeilingWhenOnlyTheDefaultMoves guards the other
// half: lowering the default on a small host must not also lower the ceiling a
// client may explicitly request, or an operator who still wants a few large
// parts loses that without being told.
func TestNewServiceKeepsTheShippedCeilingWhenOnlyTheDefaultMoves(t *testing.T) {
	t.Parallel()
	service := NewService(nil, Config{DefaultPartSize: 64 << 20})
	if service.defaultPartSize != 64<<20 {
		t.Fatalf("default part size = %d, want 64MiB", service.defaultPartSize)
	}
	if service.maxPartSize != maxPartSize {
		t.Fatalf("max part size = %d, want the shipped ceiling %d", service.maxPartSize, maxPartSize)
	}
}
