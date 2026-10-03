package jobs

import (
	"errors"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestValidContentRangeOnlyAcceptsTheExactRequestRange guards the resume check.
// The value decides whether a cached or partially downloaded byte range may be
// reused, so a range that is merely close is as bad as one that is wrong.
func TestValidContentRangeOnlyAcceptsTheExactRequestRange(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name                string
		header              string
		offset, size, total int64
		want                bool
	}{
		{name: "exact", header: "bytes 100-199/1000", offset: 100, size: 100, total: 1000, want: true},
		{name: "surrounding whitespace", header: "  bytes 0-9/10  ", offset: 0, size: 10, total: 10, want: true},
		{name: "wrong start", header: "bytes 99-198/1000", offset: 100, size: 100, total: 1000},
		{name: "wrong end", header: "bytes 100-198/1000", offset: 100, size: 100, total: 1000},
		{name: "wrong total", header: "bytes 100-199/999", offset: 100, size: 100, total: 1000},
		{name: "one byte short", header: "bytes 100-199/1000", offset: 100, size: 101, total: 1000},
		{name: "unknown total", header: "bytes 100-199/*", offset: 100, size: 100, total: 1000},
		{name: "empty", header: "", offset: 100, size: 100, total: 1000},
		{name: "wrong unit", header: "items 100-199/1000", offset: 100, size: 100, total: 1000},
		{name: "missing total", header: "bytes 100-199", offset: 100, size: 100, total: 1000},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := validContentRange(testCase.header, testCase.offset, testCase.size, testCase.total); got != testCase.want {
				t.Fatalf("validContentRange(%q, %d, %d, %d) = %t, want %t",
					testCase.header, testCase.offset, testCase.size, testCase.total, got, testCase.want)
			}
		})
	}
}

// TestParseOptionalUUIDTreatsBlankAsAbsent keeps a blank parent id in an upload
// payload meaning "the drive root" rather than a malformed request, while a
// non-blank value that is not a UUID is still an error the caller must report.
func TestParseOptionalUUIDTreatsBlankAsAbsent(t *testing.T) {
	t.Parallel()
	for _, blank := range []string{"", "   ", "\t"} {
		parsed, err := parseOptionalUUID(blank)
		if err != nil || parsed != nil {
			t.Fatalf("parseOptionalUUID(%q) = %v, %v, want nil, nil", blank, parsed, err)
		}
	}
	want := uuid.New()
	parsed, err := parseOptionalUUID(want.String())
	if err != nil {
		t.Fatalf("parseOptionalUUID() error = %v", err)
	}
	if parsed == nil || *parsed != want {
		t.Fatalf("parseOptionalUUID() = %v, want %v", parsed, want)
	}
	if _, err := parseOptionalUUID("not-a-uuid"); !errors.Is(err, errInvalidUploadSource) {
		t.Fatalf("parseOptionalUUID() error = %v, want errInvalidUploadSource", err)
	}
}

// TestOptionalStringStoresBlankAsNull keeps an unset text column NULL rather than
// an empty string, so a later query can tell "not given" from "given as empty".
func TestOptionalStringStoresBlankAsNull(t *testing.T) {
	t.Parallel()
	for _, blank := range []string{"", "   ", "\n\t"} {
		if got := optionalString(blank); got != nil {
			t.Fatalf("optionalString(%q) = %q, want nil", blank, *got)
		}
	}
	// The value itself is deliberately not trimmed.
	if got := optionalString(" spaced "); got == nil || *got != " spaced " {
		t.Fatalf("optionalString() = %v, want the untrimmed value", got)
	}
}

// TestUploadJobInsertOptsAndTimeouts pin the scheduling contract both upload
// workers rely on: the batch expands on the queue River drains first, and a single
// file gets the long budget a large upload needs.
func TestUploadJobInsertOptsAndTimeouts(t *testing.T) {
	t.Parallel()
	batch := UploadBatchArgs{}.InsertOpts()
	if batch.Queue != UploadQueue {
		t.Fatalf("UploadBatchArgs InsertOpts().Queue = %q, want %q", batch.Queue, UploadQueue)
	}
	if batch.MaxAttempts != 3 {
		t.Fatalf("UploadBatchArgs InsertOpts().MaxAttempts = %d, want 3", batch.MaxAttempts)
	}
	source := UploadSourceArgs{}.InsertOpts()
	if source.MaxAttempts < 1 {
		t.Fatalf("UploadSourceArgs InsertOpts().MaxAttempts = %d, want at least 1", source.MaxAttempts)
	}
	// The batch job is deliberately not deduplicated by arguments: a failed batch
	// may be submitted again, and it is the per-file jobs that deduplicate on
	// (BatchID, SourceIndex).
	if batch.UniqueOpts.ByArgs {
		t.Fatal("UploadBatchArgs dedupe by args, want a failed batch to be resubmittable")
	}

	if got := (&UploadBatchWorker{}).Timeout(nil); got != time.Hour {
		t.Fatalf("UploadBatchWorker.Timeout() = %s, want 1h", got)
	}
	if got := (&UploadSourceWorker{}).Timeout(nil); got != 24*time.Hour {
		t.Fatalf("UploadSourceWorker.Timeout() = %s, want 24h", got)
	}
}

// TestOpenPartReadsOnlyTheRequestedLocalRange is the property the whole chunked
// upload rests on: a part must be exactly the byte range the job asked for, never
// the whole file and never a range that runs past the end.
func TestOpenPartReadsOnlyTheRequestedLocalRange(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := &UploadSourceWorker{}

	reader, release, err := worker.openPart(t.Context(), UploadFileSource{Type: "local", Location: path}, 3, 4)
	if err != nil {
		t.Fatalf("openPart() error = %v", err)
	}
	got, err := io.ReadAll(reader)
	release()
	if err != nil {
		t.Fatalf("read part error = %v", err)
	}
	if string(got) != "3456" {
		t.Fatalf("part = %q, want %q", got, "3456")
	}

	// A range that starts past the end is empty rather than an error, and reading
	// it must not spill into the previous part.
	reader, release, err = worker.openPart(t.Context(), UploadFileSource{Type: "local", Location: path}, 8, 50)
	if err != nil {
		t.Fatalf("openPart() for a short tail error = %v", err)
	}
	tail, err := io.ReadAll(reader)
	release()
	if err != nil {
		t.Fatalf("read tail error = %v", err)
	}
	if string(tail) != "89" {
		t.Fatalf("tail = %q, want %q", tail, "89")
	}

	// A missing file is reported rather than silently producing an empty part that
	// would be stored as a zero-byte upload.
	if _, release, err = worker.openPart(t.Context(),
		UploadFileSource{Type: "local", Location: filepath.Join(t.TempDir(), "absent.bin")}, 0, 10); err == nil {
		release()
		t.Fatal("openPart() for a missing file error = nil, want a failure")
	}
}

// TestNewUploadHTTPClientIgnoresTheEnvironmentProxy is the SSRF backstop: an
// operator's HTTP_PROXY must not be able to redirect a user-supplied URL at an
// address the dialer would otherwise refuse.
func TestNewUploadHTTPClientIgnoresTheEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	client := NewUploadHTTPClient()
	if client == nil {
		t.Fatal("NewUploadHTTPClient() = nil")
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T, want the hardened upload transport", client.Transport)
	}
	// A nil Proxy means the environment is never consulted, whatever the
	// operator's shell exports.
	if transport.Proxy != nil {
		t.Fatal("transport has a proxy function, want nil so the environment proxy is ignored")
	}
	if transport.DialContext == nil {
		t.Fatal("transport DialContext is nil, want the address-checking dialer")
	}
	if client.Timeout != 0 {
		t.Fatalf("client timeout = %s, want none: a large upload manages its own deadlines", client.Timeout)
	}
}

// TestSafeUploadAddressRefusesEveryNonRoutableRange keeps the dialer from being
// talked into reaching loopback, private, link-local or otherwise special
// addresses through a hostname that resolves to them. A user-supplied URL naming
// the cloud metadata endpoint must never produce a connection.
func TestSafeUploadAddressRefusesEveryNonRoutableRange(t *testing.T) {
	t.Parallel()
	for _, address := range []string{
		"127.0.0.1",        // loopback
		"127.1.2.3",        // the rest of 127/8 is loopback too
		"10.0.0.1",         // private
		"192.168.1.1",      // private
		"172.16.0.1",       // private
		"169.254.169.254",  // link-local, the classic metadata endpoint
		"0.0.0.0",          // unspecified
		"224.0.0.1",        // multicast
		"::1",              // IPv6 loopback
		"fe80::1",          // IPv6 link-local
		"fc00::1",          // IPv6 unique local
		"::ffff:127.0.0.1", // IPv4-mapped loopback
	} {
		parsed := netip.MustParseAddr(address)
		if safeUploadAddress(parsed) {
			t.Fatalf("safeUploadAddress(%s) = true, want it refused", address)
		}
	}
	for _, address := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		parsed := netip.MustParseAddr(address)
		if !safeUploadAddress(parsed) {
			t.Fatalf("safeUploadAddress(%s) = false, want a public address admitted", address)
		}
	}
}
