package transfer

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
)

// countingCatalog serves one small file so every test here reaches the slot logic
// rather than a validation branch.
type countingCatalog struct {
	file   *sqlcgen.File
	fileID uuid.UUID
	parts  []*sqlcgen.FilePart
	mu     sync.Mutex
	calls  int
}

func (c *countingCatalog) Get(context.Context, int64, uuid.UUID) (*sqlcgen.File, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.file, nil
}

func (c *countingCatalog) Parts(context.Context, int64, uuid.UUID) ([]*sqlcgen.FilePart, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.parts, nil
}

func newCountingDownloader(maxConcurrent int) (*Downloader, *countingCatalog) {
	fileID := uuid.New()
	catalog := &countingCatalog{
		fileID: fileID,
		file: &sqlcgen.File{
			ID: pgtype.UUID{Bytes: fileID, Valid: true}, UserID: 7, Name: "file.bin",
			Kind: sqlcgen.FileKindFile, Size: pgtype.Int8{Int64: 4, Valid: true},
			Status: sqlcgen.FileStatusActive,
		},
		parts: []*sqlcgen.FilePart{
			{PartNo: 1, ChannelID: 11, MessageID: 101, PlainSize: pgtype.Int8{Int64: 4, Valid: true}, StoredSize: pgtype.Int8{Int64: 4, Valid: true}},
		},
	}
	return NewDownloader(catalog, &downloadStorage{data: map[int64][]byte{101: []byte("abcd")}}, nil, maxConcurrent), catalog
}

// TestDownloaderAdmitsNoMoreThanItsSlotCount is the guarantee the setting exists
// for: however many clients ask at once, the number of ranges holding memory
// never exceeds the configured limit. Without it a handful of concurrent
// downloads is enough to drive a small host into the OOM killer.
func TestDownloaderAdmitsNoMoreThanItsSlotCount(t *testing.T) {
	t.Parallel()
	const limit = 2
	downloader, _ := newCountingDownloader(limit)

	// Hold every slot open, then prove the next caller waits rather than being
	// admitted: a refused download would be a visible outage, and an admitted one
	// is the memory spike this bounds.
	held := make([]*Download, 0, limit)
	for range limit {
		download, err := downloader.Open(t.Context(), DownloadRequest{UserID: 7, FileID: downloader.catalog.(*countingCatalog).fileID, Length: 4})
		if err != nil {
			t.Fatalf("Open() within the limit error = %v", err)
		}
		held = append(held, download)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := downloader.Open(ctx, DownloadRequest{UserID: 7, FileID: downloader.catalog.(*countingCatalog).fileID, Length: 4})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Open() beyond the limit error = %v, want it to wait for a free slot", err)
	}

	// Returning a slot admits the waiting caller again, which is what proves the
	// bound is a queue rather than a refusal.
	for _, download := range held {
		if err := download.Reader.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}
	next, err := downloader.Open(t.Context(), DownloadRequest{UserID: 7, FileID: downloader.catalog.(*countingCatalog).fileID, Length: 4})
	if err != nil {
		t.Fatalf("Open() after a slot was returned error = %v", err)
	}
	if err := next.Reader.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// TestDownloaderReturnsTheSlotWhenOpeningFails covers the paths that never hand a
// reader to the caller: a leaked slot there would shrink capacity on every failed
// request until downloads stopped entirely.
func TestDownloaderReturnsTheSlotWhenOpeningFails(t *testing.T) {
	t.Parallel()
	downloader, catalog := newCountingDownloader(1)

	// A range past the end of the file is rejected during planning, after the
	// slot has been taken.
	catalog.file.Size = pgtype.Int8{Int64: 4, Valid: true}
	fileID := catalog.fileID
	for range 3 {
		if _, err := downloader.Open(t.Context(), DownloadRequest{
			UserID: 7, FileID: fileID, Offset: 100, Length: 4,
		}); err == nil {
			t.Fatal("Open() past the end error = nil, want a rejection")
		}
	}
	// The single slot is still available, so the failures gave it back.
	if _, err := downloader.Open(t.Context(), DownloadRequest{UserID: 7, FileID: fileID, Length: 4}); err != nil {
		t.Fatalf("Open() after failed opens error = %v, want the slot to have been returned", err)
	}
}

// TestNewDownloaderFallsBackToTheDefaultLimit keeps a caller that does not set a
// limit from building a Downloader that admits unbounded memory, which is what
// happens when the zero value is taken literally.
func TestNewDownloaderFallsBackToTheDefaultLimit(t *testing.T) {
	t.Parallel()
	catalog := &countingCatalog{}
	for _, requested := range []int{0, -1} {
		downloader := NewDownloader(catalog, nil, nil, requested)
		if got := cap(downloader.slots); got != defaultMaxConcurrentDownloads {
			t.Fatalf("NewDownloader(maxConcurrent=%d) capacity = %d, want %d",
				requested, got, defaultMaxConcurrentDownloads)
		}
	}
	explicit := NewDownloader(catalog, nil, nil, 9)
	if got := cap(explicit.slots); got != 9 {
		t.Fatalf("NewDownloader(maxConcurrent=9) capacity = %d, want 9", got)
	}
}

// TestDownloadReaderCloseReturnsTheSlotOnlyOnce covers the double-close guard: a
// reader that is closed twice, which a retrying client does, must not release a
// slot that another download is now holding.
func TestDownloadReaderCloseReturnsTheSlotOnlyOnce(t *testing.T) {
	t.Parallel()
	downloader, catalog := newCountingDownloader(1)
	download, err := downloader.Open(t.Context(), DownloadRequest{
		UserID: 7, FileID: catalog.fileID, Length: 4,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := download.Reader.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := download.Reader.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	// A second release would drain a slot the reader never took, so the next
	// download would proceed while the count says otherwise. The wait is
	// bounded because an over-release blocks on the slot channel rather than
	// returning an error, and a hung suite is a far worse failure report than a
	// named one.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	next, err := downloader.Open(ctx, DownloadRequest{UserID: 7, FileID: catalog.fileID, Length: 4})
	if err != nil {
		t.Fatalf("Open() after a double close error = %v, want the slot to survive one close", err)
	}
	if _, err := io.Copy(io.Discard, next.Reader); err != nil {
		t.Fatalf("read error = %v", err)
	}
	if err := next.Reader.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// TestZeroLengthDownloadDoesNotHoldASlot covers the empty range: it reads nothing
// and opens no storage range, so it must not occupy capacity that a real
// download needs.
func TestZeroLengthDownloadDoesNotHoldASlot(t *testing.T) {
	t.Parallel()
	downloader, catalog := newCountingDownloader(1)
	empty, err := downloader.Open(t.Context(), DownloadRequest{UserID: 7, FileID: catalog.fileID, Offset: 4, Length: 4})
	if err != nil {
		t.Fatalf("Open() of an empty range error = %v", err)
	}
	if err := empty.Reader.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	// The single slot has to be free, otherwise the empty range is serialising
	// against every real download.
	done := make(chan error, 1)
	go func() {
		next, err := downloader.Open(t.Context(), DownloadRequest{UserID: 7, FileID: catalog.fileID, Length: 4})
		if err == nil {
			err = next.Reader.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Open() after an empty range error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an empty range held the only download slot")
	}
}
