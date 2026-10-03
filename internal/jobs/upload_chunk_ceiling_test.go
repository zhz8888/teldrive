package jobs

import (
	"strings"
	"testing"
)

// TestNormalizeUploadChunkSizeHonoursTheDeploymentCeiling is what makes
// uploads.max-part-size reach the background import path. A chunk becomes the
// session's part size, so a chunk above the ceiling the operator configured would
// make every background import fail at session creation instead of using smaller
// parts.
func TestNormalizeUploadChunkSizeHonoursTheDeploymentCeiling(t *testing.T) {
	t.Parallel()
	const ceiling = 64 << 20

	// The shipped default is larger than the ceiling, so an import that names no
	// chunk has to come down to it rather than to 512 MiB.
	got, err := normalizeUploadChunkSize(0, ceiling)
	if err != nil {
		t.Fatalf("normalizeUploadChunkSize(0, %d) error = %v", ceiling, err)
	}
	if got != ceiling {
		t.Fatalf("default chunk = %d, want the ceiling %d", got, ceiling)
	}

	// An explicit request above the ceiling is refused rather than quietly
	// clamped, because the job arguments come from a user and silently accepting
	// a size the server will refuse later hides the real problem.
	_, err = normalizeUploadChunkSize(200<<20, ceiling)
	if err == nil {
		t.Fatal("normalizeUploadChunkSize() above the ceiling succeeded, want a rejection")
	}
	if !strings.Contains(err.Error(), "64 MiB") {
		t.Fatalf("error = %v, want it to name the ceiling in MiB", err)
	}

	// A request inside the ceiling is still aligned as before.
	aligned, err := normalizeUploadChunkSize(80<<20, 512<<20)
	if err != nil {
		t.Fatalf("normalizeUploadChunkSize(80MiB, 512MiB) error = %v", err)
	}
	if aligned != 80<<20 {
		t.Fatalf("chunk = %d, want 80MiB aligned", aligned)
	}
}

// TestNormalizeUploadChunkSizeKeepsThePackageDefaultWithoutACeiling covers the
// deployments that set nothing: a large ceiling must not lower the chunk the
// worker picks by default.
func TestNormalizeUploadChunkSizeKeepsThePackageDefaultWithoutACeiling(t *testing.T) {
	t.Parallel()
	got, err := normalizeUploadChunkSize(0, 0)
	if err != nil {
		t.Fatalf("normalizeUploadChunkSize(0, 0) error = %v", err)
	}
	if got != defaultUploadChunk {
		t.Fatalf("default chunk = %d, want the package default %d", got, defaultUploadChunk)
	}
}
