package transfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/telegramstore"
	"github.com/zhz8888/teldrive/v2/internal/treehash"
	"github.com/zhz8888/teldrive/v2/internal/uploads"
)

func TestExactReader(t *testing.T) {
	t.Parallel()

	t.Run("exact", func(t *testing.T) {
		r := newExactReader(context.Background(), bytes.NewBufferString("abcd"), 4)
		got, err := io.ReadAll(r)
		if err != nil || string(got) != "abcd" {
			t.Fatalf("ReadAll() = %q, %v", got, err)
		}
		if err := r.Verify(); err != nil {
			t.Fatalf("Verify() error = %v", err)
		}
	})

	t.Run("short", func(t *testing.T) {
		r := newExactReader(context.Background(), bytes.NewBufferString("abc"), 4)
		_, err := io.ReadAll(r)
		if !errors.Is(err, ErrBodyTooShort) {
			t.Fatalf("ReadAll() error = %v", err)
		}
		if err := r.Verify(); !errors.Is(err, ErrBodyTooShort) {
			t.Fatalf("Verify() error = %v", err)
		}
	})

	t.Run("long", func(t *testing.T) {
		r := newExactReader(context.Background(), bytes.NewBufferString("abcde"), 4)
		got, err := io.ReadAll(r)
		if err != nil || string(got) != "abcd" {
			t.Fatalf("ReadAll() = %q, %v", got, err)
		}
		if err := r.Verify(); !errors.Is(err, ErrBodyTooLong) {
			t.Fatalf("Verify() error = %v", err)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r := newExactReader(ctx, bytes.NewBufferString("abcd"), 4)
		var b [1]byte
		if _, err := r.Read(b[:]); !errors.Is(err, context.Canceled) {
			t.Fatalf("Read() error = %v", err)
		}
	})
}

func TestGenerateSaltDeterministic(t *testing.T) {
	t.Parallel()
	seed := bytes.Repeat([]byte{1}, 32)
	first, err := generateSalt(bytes.NewReader(seed))
	if err != nil {
		t.Fatal(err)
	}
	second, err := generateSalt(bytes.NewReader(seed))
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first == "" {
		t.Fatalf("salt values = %q and %q", first, second)
	}
	if _, err := generateSalt(nil); err == nil {
		t.Fatal("expected nil random source error")
	}
}

func TestPartName(t *testing.T) {
	t.Parallel()
	pipeline := &Pipeline{}
	cases := []struct {
		partNo int32
		want   string
	}{
		{partNo: 1, want: "movie.mkv.001"},
		{partNo: 2, want: "movie.mkv.002"},
		{partNo: 999, want: "movie.mkv.999"},
		{partNo: 1000, want: "movie.mkv.1000"},
	}
	for _, tc := range cases {
		if got := pipeline.partName("movie.mkv", tc.partNo); got != tc.want {
			t.Fatalf("partName(%d) = %q, want %q", tc.partNo, got, tc.want)
		}
	}
}

func TestStaticKeyProvider(t *testing.T) {
	t.Parallel()
	provider := StaticKeyProvider{1: "secret"}
	if key, err := provider.Key(context.Background(), 1, 1); err != nil || key != "secret" {
		t.Fatalf("Key() = %q, %v", key, err)
	}
	if _, err := provider.Key(context.Background(), 1, 2); !errors.Is(err, ErrEncryptionKey) {
		t.Fatalf("missing key error = %v", err)
	}
}

func TestChecksumMatches(t *testing.T) {
	t.Parallel()
	value := "abc"
	if !checksumMatches("", false, nil) {
		t.Fatal("nil assertion should match")
	}
	if !checksumMatches(value, true, &value) {
		t.Fatal("equal checksum should match")
	}
	if checksumMatches(value, false, &value) {
		t.Fatal("invalid stored checksum should not match")
	}
	other := "def"
	if checksumMatches(value, true, &other) {
		t.Fatal("different checksum should not match")
	}
}

func TestDeleteUploadedCompensation(t *testing.T) {
	t.Parallel()
	storage := &deleteStorage{}
	pipeline := &Pipeline{storage: storage}
	if err := pipeline.deleteUploaded(context.Background(), 1, telegramstore.StoredPart{}); err != nil {
		t.Fatalf("empty part deletion error = %v", err)
	}
	part := telegramstore.StoredPart{ChannelID: 9, MessageID: 7}
	if err := pipeline.deleteUploaded(context.Background(), 1, part); err != nil {
		t.Fatalf("deleteUploaded() error = %v", err)
	}
	if len(storage.deleted) != 1 || storage.deleted[0] != 7 {
		t.Fatalf("deleted messages = %#v", storage.deleted)
	}
	storage.err = errors.New("delete failed")
	if err := pipeline.deleteUploaded(context.Background(), 1, part); err == nil {
		t.Fatal("expected compensation error")
	}
}

func TestFailPartDetachesCleanupFromCanceledRequest(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	catalog := &cleanupCatalog{}
	pipeline := &Pipeline{catalog: catalog}
	cause := context.Canceled

	err := pipeline.failPart(ctx, UploadPartRequest{UploadID: uuid.New(), PartNo: 1}, uuid.New(), "request_canceled", cause)

	if !errors.Is(err, cause) {
		t.Fatalf("failPart() error = %v, want %v", err, cause)
	}
	if catalog.cleanupErr != nil {
		t.Fatalf("FailPart() context error = %v", catalog.cleanupErr)
	}
	if !catalog.hadDeadline {
		t.Fatal("FailPart() cleanup context has no deadline")
	}
}

func TestUploadPartRenewsLeaseDuringTransfer(t *testing.T) {
	t.Parallel()
	catalog := newLeaseCatalog()
	storage := &leaseStorage{waitForRenewal: catalog.renewed}
	pipeline := NewPipeline(catalog, fixedChannelResolver(9), storage, nil, Config{LeaseRenewInterval: time.Millisecond})

	result, err := pipeline.UploadPart(context.Background(), UploadPartRequest{
		UserID: 1, UploadID: catalog.uploadID, PartNo: 1, PlainSize: 4, Body: bytes.NewBufferString("data"),
	})
	if err != nil {
		t.Fatalf("UploadPart() error = %v", err)
	}
	if result.Part.State != sqlcgen.UploadPartStateStored || catalog.renewCount() == 0 {
		t.Fatalf("result = %#v, renewals = %d", result, catalog.renewCount())
	}
}

func TestUploadPartFinalizesPublishedMessageAfterRequestCancellation(t *testing.T) {
	t.Parallel()
	catalog := newLeaseCatalog()
	ctx, cancel := context.WithCancel(context.Background())
	storage := &leaseStorage{cancelAfterUpload: cancel}
	pipeline := NewPipeline(catalog, fixedChannelResolver(9), storage, nil, Config{})

	result, err := pipeline.UploadPart(ctx, UploadPartRequest{
		UserID: 1, UploadID: catalog.uploadID, PartNo: 1, PlainSize: 4, Body: bytes.NewBufferString("data"),
	})
	if err != nil {
		t.Fatalf("UploadPart() error = %v", err)
	}
	if result.Part.State != sqlcgen.UploadPartStateStored || catalog.storeContextErr != nil {
		t.Fatalf("result = %#v, store context error = %v", result, catalog.storeContextErr)
	}
}

func TestUploadPartDeletesPartPublishedWithFailedUpload(t *testing.T) {
	t.Parallel()
	catalog := newLeaseCatalog()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	storage := &publishedFailureStorage{cancelRequest: cancel}
	pipeline := NewPipeline(catalog, fixedChannelResolver(9), storage, nil, Config{})

	_, err := pipeline.UploadPart(ctx, UploadPartRequest{
		UserID: 1, UploadID: catalog.uploadID, PartNo: 1, PlainSize: 4, Body: bytes.NewBufferString("data"),
	})
	if !errors.Is(err, telegramstore.ErrSizeMismatch) {
		t.Fatalf("UploadPart() error = %v, want ErrSizeMismatch", err)
	}
	if len(storage.deleted) != 1 || storage.deleted[0] != 7 {
		t.Fatalf("deleted messages = %#v, want [7]", storage.deleted)
	}
	if storage.deleteErr != nil || !storage.deleteDeadline {
		t.Fatalf("compensating delete context error = %v, deadline = %v, want a live bounded context", storage.deleteErr, storage.deleteDeadline)
	}
}

// fixedChannelResolver is a ChannelResolver whose own value is the channel it
// answers with, so a test can name the target channel in the constructor.
type fixedChannelResolver int64

// Resolve returns the resolver's value as the channel, ignoring the user and any
// requested channel.
func (r fixedChannelResolver) Resolve(context.Context, int64, int64) (int64, error) {
	return int64(r), nil
}

// leaseCatalog is the permissive UploadCatalog used by the lease tests: it grants
// a fresh lease on every claim, counts renewals, and reports what the store step
// saw, so a test can prove the lease was kept alive and the commit ran on a live
// context.
type leaseCatalog struct {
	// mu guards renewals, which the pipeline's renewer goroutine writes while
	// the test goroutine reads it.
	mu sync.Mutex
	// uploadID is the session ID the tests put in every request; the catalog
	// answers with empty rows, so only its stability matters.
	uploadID uuid.UUID
	// leaseToken is returned by ClaimPart and must survive the round trip
	// through RenewPart and FailPart.
	leaseToken uuid.UUID
	// renewed is closed by the first RenewPart call, which lets a storage stub
	// hold an upload open until the lease has actually been renewed.
	renewed chan struct{}
	// renewOnce closes renewed exactly once, however often the renewer ticks;
	// the lease is renewed as many times as the interval allows, and a second
	// close would panic.
	renewOnce sync.Once
	// renewals counts RenewPart calls; renewCount reads it under mu.
	renewals int
	// storeContextErr records the context error StorePart was called with, which
	// proves the commit did not inherit a cancelled request context.
	storeContextErr error
}

// newLeaseCatalog returns a catalog with fresh upload and lease tokens and an
// open renewal signal.
func newLeaseCatalog() *leaseCatalog {
	return &leaseCatalog{uploadID: uuid.New(), leaseToken: uuid.New(), renewed: make(chan struct{})}
}

// Get returns an empty session for any ID: the pipeline only needs the session to
// exist before it claims a part.
func (c *leaseCatalog) Get(context.Context, int64, uuid.UUID) (*sqlcgen.UploadSession, error) {
	return &sqlcgen.UploadSession{}, nil
}

// GetPart reports every part as unknown, so UploadPart always takes the fresh
// claim path instead of the already-stored shortcut.
func (c *leaseCatalog) GetPart(context.Context, int64, uuid.UUID, int32) (*sqlcgen.UploadPart, error) {
	return nil, uploads.ErrNotFound
}

// ClaimPart always grants a lease carrying the catalog's token.
func (c *leaseCatalog) ClaimPart(context.Context, uploads.ClaimPartInput) (*uploads.ClaimPartResult, error) {
	return &uploads.ClaimPartResult{Part: &sqlcgen.UploadPart{}, LeaseToken: c.leaseToken}, nil
}

// RenewPart counts the renewal and closes renewed on the first call, which is how
// a test learns that the renewer goroutine has run at least once.
func (c *leaseCatalog) RenewPart(context.Context, uploads.RenewPartInput) error {
	c.mu.Lock()
	c.renewals++
	c.mu.Unlock()
	c.renewOnce.Do(func() { close(c.renewed) })
	return nil
}

// StorePart records the context error it was called with and reports the part as
// stored.
func (c *leaseCatalog) StorePart(ctx context.Context, _ uploads.StorePartInput) (*sqlcgen.UploadPart, error) {
	c.storeContextErr = ctx.Err()
	return &sqlcgen.UploadPart{State: sqlcgen.UploadPartStateStored}, nil
}

// FailPart is a no-op returning an empty part; these tests never inspect the
// failure row.
func (*leaseCatalog) FailPart(context.Context, uploads.FailPartInput) (*sqlcgen.UploadPart, error) {
	return &sqlcgen.UploadPart{}, nil
}

// renewCount returns the number of renewals recorded so far. It locks because the
// renewer goroutine may still be running when a test asks.
func (c *leaseCatalog) renewCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.renewals
}

// leaseStorage is the storage side of the lease tests. Its knobs pin the upload
// in flight long enough for a renewal to happen, or cancel the request once the
// document has been published.
type leaseStorage struct {
	// waitForRenewal, when non-nil, holds Upload until the channel closes or the
	// upload context ends, so the transfer is still running when the lease
	// renewer ticks.
	waitForRenewal <-chan struct{}
	// cancelAfterUpload cancels the request after the body has been read, so the
	// document is published while the request context is already dying.
	cancelAfterUpload context.CancelFunc
}

// Upload waits for the renewal signal when one is configured, reads the whole
// body, optionally cancels the request, and publishes the part as message 7 with
// the size it actually read.
func (s *leaseStorage) Upload(ctx context.Context, request telegramstore.UploadRequest) (telegramstore.StoredPart, error) {
	if s.waitForRenewal != nil {
		select {
		case <-s.waitForRenewal:
		case <-ctx.Done():
			return telegramstore.StoredPart{}, context.Cause(ctx)
		}
	}
	data, err := io.ReadAll(request.Reader)
	if err != nil {
		return telegramstore.StoredPart{}, err
	}
	if s.cancelAfterUpload != nil {
		s.cancelAfterUpload()
	}
	return telegramstore.StoredPart{ChannelID: request.ChannelID, MessageID: 7, Size: int64(len(data))}, nil
}

// OpenRange is unused by the lease tests and reports an error.
func (*leaseStorage) OpenRange(context.Context, telegramstore.RangeRequest) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

// DeleteMessages is a no-op; the lease tests never delete.
func (*leaseStorage) DeleteMessages(context.Context, int64, int64, []int64) error { return nil }

// CopyPart is unused by the lease tests and reports an error.
func (*leaseStorage) CopyPart(context.Context, int64, int64, int64, int64) (telegramstore.StoredPart, error) {
	return telegramstore.StoredPart{}, errors.New("not used")
}

// CreateChannel is unused by the lease tests and reports an error.
func (*leaseStorage) CreateChannel(context.Context, int64, string) (telegramstore.Channel, error) {
	return telegramstore.Channel{}, errors.New("not used")
}

// DeleteChannel is a no-op, like DeleteMessages.
func (*leaseStorage) DeleteChannel(context.Context, int64, int64) error { return nil }

// cleanupCatalog is the UploadCatalog used to check how the pipeline prepares its
// bookkeeping context when a request is already dead: only FailPart is
// implemented, and it records what that context looked like.
type cleanupCatalog struct {
	// cleanupErr records the context error FailPart was called with; nil means
	// the cleanup context survived the cancelled request.
	cleanupErr error
	// hadDeadline records whether that context carried a deadline, which is what
	// distinguishes the detached, bounded cleanup context from the request's own.
	hadDeadline bool
}

// Get is unused by the cleanup test and reports an error.
func (*cleanupCatalog) Get(context.Context, int64, uuid.UUID) (*sqlcgen.UploadSession, error) {
	return nil, errors.New("not used")
}

// GetPart is unused by the cleanup test and reports an error.
func (*cleanupCatalog) GetPart(context.Context, int64, uuid.UUID, int32) (*sqlcgen.UploadPart, error) {
	return nil, errors.New("not used")
}

// ClaimPart is unused by the cleanup test and reports an error.
func (*cleanupCatalog) ClaimPart(context.Context, uploads.ClaimPartInput) (*uploads.ClaimPartResult, error) {
	return nil, errors.New("not used")
}

// RenewPart is unused by the cleanup test and reports an error.
func (*cleanupCatalog) RenewPart(context.Context, uploads.RenewPartInput) error {
	return errors.New("not used")
}

// StorePart is unused by the cleanup test and reports an error.
func (*cleanupCatalog) StorePart(context.Context, uploads.StorePartInput) (*sqlcgen.UploadPart, error) {
	return nil, errors.New("not used")
}

// FailPart records the context error and whether the context was bounded, then
// reports an empty part. It never fails, so it cannot mask what the test checks.
func (c *cleanupCatalog) FailPart(ctx context.Context, _ uploads.FailPartInput) (*sqlcgen.UploadPart, error) {
	c.cleanupErr = ctx.Err()
	_, c.hadDeadline = ctx.Deadline()
	return &sqlcgen.UploadPart{}, nil
}

// deleteStorage is the storage side of the compensation tests: DeleteMessages
// records the message IDs it was asked to remove, or fails when err is set.
type deleteStorage struct {
	// deleted collects every message ID passed to DeleteMessages, in call order.
	deleted []int64
	// err, when set, makes DeleteMessages fail instead of recording, which
	// exercises the path where compensation itself reports an error.
	err error
}

// Upload is unused by the compensation tests and reports an error.
func (*deleteStorage) Upload(context.Context, telegramstore.UploadRequest) (telegramstore.StoredPart, error) {
	return telegramstore.StoredPart{}, errors.New("not used")
}

// OpenRange is unused by the compensation tests and reports an error.
func (*deleteStorage) OpenRange(context.Context, telegramstore.RangeRequest) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

// DeleteMessages records the IDs unless err is set, in which case it reports the
// failure without recording anything.
func (s *deleteStorage) DeleteMessages(_ context.Context, _ int64, _ int64, ids []int64) error {
	if s.err != nil {
		return s.err
	}
	s.deleted = append(s.deleted, ids...)
	return nil
}

// CopyPart is unused by the compensation tests and reports an error.
func (*deleteStorage) CopyPart(context.Context, int64, int64, int64, int64) (telegramstore.StoredPart, error) {
	return telegramstore.StoredPart{}, errors.New("not implemented")
}

// CreateChannel is unused by the compensation tests and reports an error.
func (*deleteStorage) CreateChannel(context.Context, int64, string) (telegramstore.Channel, error) {
	return telegramstore.Channel{}, errors.New("not used")
}

// DeleteChannel is a no-op; the compensation tests never delete a channel.
func (*deleteStorage) DeleteChannel(context.Context, int64, int64) error { return nil }

// publishedFailureStorage mirrors the storage contract for a failed upload that
// already published a document: it reports the part next to the error, cancels
// the request first, and records what the compensating delete sees.
type publishedFailureStorage struct {
	// cancelRequest cancels the request before Upload returns, so the published
	// part has to be deleted through a context that outlives it.
	cancelRequest context.CancelFunc
	// deleted collects the message IDs the compensating delete was asked to
	// remove.
	deleted []int64
	// deleteErr records the context error DeleteMessages saw; nil proves the
	// compensation context was still live.
	deleteErr error
	// deleteDeadline records whether that context carried a deadline, which shows
	// the cleanup was bounded rather than left to run forever.
	deleteDeadline bool
}

// Upload cancels the request and returns a published part together with
// ErrSizeMismatch, the combination that forces the caller to delete message 7.
func (s *publishedFailureStorage) Upload(context.Context, telegramstore.UploadRequest) (telegramstore.StoredPart, error) {
	if s.cancelRequest != nil {
		s.cancelRequest()
	}
	return telegramstore.StoredPart{ChannelID: 9, MessageID: 7, Size: 4}, telegramstore.ErrSizeMismatch
}

// OpenRange is unused by this test and reports an error.
func (*publishedFailureStorage) OpenRange(context.Context, telegramstore.RangeRequest) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

// DeleteMessages records the context it ran under and the IDs it removed; it
// always succeeds so the test sees the real cleanup context.
func (s *publishedFailureStorage) DeleteMessages(ctx context.Context, _ int64, _ int64, ids []int64) error {
	s.deleteErr = ctx.Err()
	_, s.deleteDeadline = ctx.Deadline()
	s.deleted = append(s.deleted, ids...)
	return nil
}

// CopyPart is unused by this test and reports an error.
func (*publishedFailureStorage) CopyPart(context.Context, int64, int64, int64, int64) (telegramstore.StoredPart, error) {
	return telegramstore.StoredPart{}, errors.New("not used")
}

// CreateChannel is unused by this test and reports an error.
func (*publishedFailureStorage) CreateChannel(context.Context, int64, string) (telegramstore.Channel, error) {
	return telegramstore.Channel{}, errors.New("not used")
}

// DeleteChannel is a no-op; this test never deletes a channel.
func (*publishedFailureStorage) DeleteChannel(context.Context, int64, int64) error { return nil }

func TestNormalizeOptionalChecksum(t *testing.T) {
	t.Parallel()
	if value, err := normalizeOptionalChecksum(nil); err != nil || value != nil {
		t.Fatalf("nil checksum = %#v, %v", value, err)
	}
	valid := strings.Repeat("AB", treehash.DigestSize)
	normalized, err := normalizeOptionalChecksum(&valid)
	if err != nil || normalized == nil || *normalized != strings.ToLower(valid) {
		t.Fatalf("valid checksum = %#v, %v", normalized, err)
	}
	invalid := "xyz"
	if _, err := normalizeOptionalChecksum(&invalid); !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("invalid checksum error = %v", err)
	}
}

func TestRandomizedPartName(t *testing.T) {
	t.Parallel()
	pipeline := &Pipeline{config: Config{RandomizePartNames: true}}
	first := pipeline.partName("movie.mkv", 1)
	second := pipeline.partName("movie.mkv", 1)
	if first == second || len(first) != 64 || len(second) != 64 {
		t.Fatalf("randomized names = %q, %q", first, second)
	}
}
