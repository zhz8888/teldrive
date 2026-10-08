//go:build integration

package transfer_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zhz8888/teldrive/v2/internal/contentcrypto"
	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/dbtypes"
	"github.com/zhz8888/teldrive/v2/internal/telegramstore"
	testpostgres "github.com/zhz8888/teldrive/v2/internal/testutil/postgres"
	"github.com/zhz8888/teldrive/v2/internal/transfer"
	"github.com/zhz8888/teldrive/v2/internal/treehash"
	"github.com/zhz8888/teldrive/v2/internal/uploads"
)

func TestUploadPipelinePlaintextAndRetry(t *testing.T) {
	db := testpostgres.New(t)
	seedTransferOwner(t, db.Pool, 1001, 9001)
	catalog := uploads.NewService(db.Pool)
	body := []byte("plain Telegram upload")
	session, err := catalog.Create(context.Background(), uploads.CreateInput{
		UserID: 1001, Name: "plain.bin", ExpectedSize: int64(len(body)), PartSize: int64(len(body)),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	uploadID, ok := dbtypes.GoogleUUID(session.ID)
	if !ok {
		t.Fatal("missing upload id")
	}
	checksum := checksumFor(body)
	storage := &memoryStorage{}
	resolver := &fixedResolver{channelID: 9001}
	pipeline := transfer.NewPipeline(catalog, resolver, storage, nil, transfer.Config{})

	result, err := pipeline.UploadPart(context.Background(), transfer.UploadPartRequest{
		UserID: 1001, UploadID: uploadID, PartNo: 1, PlainSize: int64(len(body)), Checksum: &checksum, Body: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("UploadPart() error = %v", err)
	}
	if result.Existing || result.Part.State != sqlcgen.UploadPartStateStored {
		t.Fatalf("UploadPart() result = %#v", result)
	}
	if got := storage.payload(0); !bytes.Equal(got, body) {
		t.Fatalf("stored payload = %q", got)
	}
	if !result.Part.Checksum.Valid || result.Part.Checksum.String != checksum || len(result.Part.BlockHashes) != treehash.DigestSize {
		t.Fatalf("stored metadata = %#v", result.Part)
	}

	retry, err := pipeline.UploadPart(context.Background(), transfer.UploadPartRequest{
		UserID: 1001, UploadID: uploadID, PartNo: 1, PlainSize: int64(len(body)), Body: panicReader{},
	})
	if err != nil || !retry.Existing {
		t.Fatalf("retry = %#v, %v", retry, err)
	}
	if storage.uploadCount() != 1 || resolver.calls != 1 {
		t.Fatalf("retry performed side effects: uploads=%d resolves=%d", storage.uploadCount(), resolver.calls)
	}

	file, err := catalog.Complete(context.Background(), 1001, uploadID)
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	expectedFileHash := treehash.SumToHex(treehash.ComputeTreeHash(result.Part.BlockHashes))
	if !file.HashValue.Valid || file.HashValue.String != expectedFileHash {
		t.Fatalf("file hash = %#v, want %s", file.HashValue, expectedFileHash)
	}
}

func TestUploadPipelineHashingDisabled(t *testing.T) {
	db := testpostgres.New(t)
	seedTransferOwner(t, db.Pool, 1001, 9001)
	catalog := uploads.NewService(db.Pool)
	body := []byte("upload without hashing")
	session, err := catalog.Create(context.Background(), uploads.CreateInput{
		UserID: 1001, Name: "unhashed.bin", ExpectedSize: int64(len(body)), PartSize: int64(len(body)),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	uploadID, _ := dbtypes.GoogleUUID(session.ID)
	pipeline := transfer.NewPipeline(catalog, &fixedResolver{channelID: 9001}, &memoryStorage{}, nil, transfer.Config{DisableHashing: true})
	result, err := pipeline.UploadPart(context.Background(), transfer.UploadPartRequest{
		UserID: 1001, UploadID: uploadID, PartNo: 1, PlainSize: int64(len(body)), Body: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("UploadPart() error = %v", err)
	}
	if result.Part.Checksum.Valid || len(result.Part.BlockHashes) != 0 {
		t.Fatalf("stored hash metadata = (%#v, %x), want empty", result.Part.Checksum, result.Part.BlockHashes)
	}
	file, err := catalog.Complete(context.Background(), 1001, uploadID)
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if file.HashAlgorithm.Valid || file.HashValue.Valid {
		t.Fatalf("file hash = (%#v, %#v), want empty", file.HashAlgorithm, file.HashValue)
	}
}

func TestUploadPipelineEncryptedCompatibility(t *testing.T) {
	db := testpostgres.New(t)
	seedTransferOwner(t, db.Pool, 1001, 9001)
	catalog := uploads.NewService(db.Pool)
	body := bytes.Repeat([]byte("encrypted-block-"), 6000)
	version := int32(7)
	session, err := catalog.Create(context.Background(), uploads.CreateInput{
		UserID: 1001, Name: "encrypted.bin", ExpectedSize: int64(len(body)), PartSize: int64(len(body)),
		Encryption: true, EncryptionKeyVersion: &version,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	uploadID, _ := dbtypes.GoogleUUID(session.ID)
	storage := &memoryStorage{}
	pipeline := transfer.NewPipeline(catalog, &fixedResolver{channelID: 9001}, storage, transfer.StaticKeyProvider{7: "same-key-format-as-original"}, transfer.Config{
		Random: bytes.NewReader(bytes.Repeat([]byte{9}, 32)),
	})
	result, err := pipeline.UploadPart(context.Background(), transfer.UploadPartRequest{
		UserID: 1001, UploadID: uploadID, PartNo: 1, PlainSize: int64(len(body)), Body: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("UploadPart() error = %v", err)
	}
	ciphertext := storage.payload(0)
	if int64(len(ciphertext)) != contentcrypto.EncryptedSize(int64(len(body))) || bytes.Equal(ciphertext, body) {
		t.Fatalf("ciphertext size/content invalid: %d", len(ciphertext))
	}
	if !result.Part.Salt.Valid || result.Part.Salt.String == "" {
		t.Fatalf("encrypted part salt = %#v", result.Part.Salt)
	}
	cipher, err := contentcrypto.NewCipher("same-key-format-as-original", result.Part.Salt.String)
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := cipher.DecryptData(io.NopCloser(bytes.NewReader(ciphertext)))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(decrypted)
	if err != nil || !bytes.Equal(plain, body) {
		t.Fatalf("decrypt = %d bytes, %v", len(plain), err)
	}
	if _, err := catalog.Complete(context.Background(), 1001, uploadID); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
}

func TestUploadPipelineChecksumMismatchCompensates(t *testing.T) {
	db := testpostgres.New(t)
	seedTransferOwner(t, db.Pool, 1001, 9001)
	catalog := uploads.NewService(db.Pool)
	body := []byte("checksum body")
	session, err := catalog.Create(context.Background(), uploads.CreateInput{
		UserID: 1001, Name: "bad.bin", ExpectedSize: int64(len(body)), PartSize: int64(len(body)),
	})
	if err != nil {
		t.Fatal(err)
	}
	uploadID, _ := dbtypes.GoogleUUID(session.ID)
	wrong := checksumFor([]byte("different body"))
	storage := &memoryStorage{}
	pipeline := transfer.NewPipeline(catalog, &fixedResolver{channelID: 9001}, storage, nil, transfer.Config{})
	_, err = pipeline.UploadPart(context.Background(), transfer.UploadPartRequest{
		UserID: 1001, UploadID: uploadID, PartNo: 1, PlainSize: int64(len(body)), Checksum: &wrong, Body: bytes.NewReader(body),
	})
	if !errors.Is(err, transfer.ErrChecksumMismatch) {
		t.Fatalf("UploadPart() error = %v", err)
	}
	if len(storage.deleted) != 1 || storage.deleted[0] != 1 {
		t.Fatalf("deleted messages = %#v", storage.deleted)
	}
	part, err := catalog.GetPart(context.Background(), 1001, uploadID, 1)
	if err != nil {
		t.Fatalf("GetPart() error = %v", err)
	}
	if part.State != sqlcgen.UploadPartStateFailed || !part.LastErrorCode.Valid || part.LastErrorCode.String != "checksum_mismatch" {
		t.Fatalf("failed part = %#v", part)
	}
}

func TestUploadPipelineRejectsLongBodyAndCompensates(t *testing.T) {
	db := testpostgres.New(t)
	seedTransferOwner(t, db.Pool, 1001, 9001)
	catalog := uploads.NewService(db.Pool)
	session, err := catalog.Create(context.Background(), uploads.CreateInput{
		UserID: 1001, Name: "long.bin", ExpectedSize: 4, PartSize: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	uploadID, _ := dbtypes.GoogleUUID(session.ID)
	storage := &memoryStorage{}
	pipeline := transfer.NewPipeline(catalog, &fixedResolver{channelID: 9001}, storage, nil, transfer.Config{})
	_, err = pipeline.UploadPart(context.Background(), transfer.UploadPartRequest{
		UserID: 1001, UploadID: uploadID, PartNo: 1, PlainSize: 4, Body: bytes.NewBufferString("abcde"),
	})
	if !errors.Is(err, transfer.ErrBodyTooLong) {
		t.Fatalf("UploadPart() error = %v", err)
	}
	if len(storage.deleted) != 1 {
		t.Fatalf("expected compensation, deleted=%#v", storage.deleted)
	}
}

// seedTransferOwner inserts the user and the selected storage channel a transfer
// test uploads into, so the catalog's ownership and rollover checks have real
// rows to resolve.
func seedTransferOwner(t testing.TB, pool *pgxpool.Pool, userID, channelID int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "INSERT INTO users (user_id) VALUES ($1)", userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO channels (channel_id, user_id, name, selected) VALUES ($1, $2, 'storage', true)", channelID, userID); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
}

// checksumFor returns the hex tree hash of data, the checksum the pipeline is
// expected to compute for a part holding it.
func checksumFor(data []byte) string {
	hasher := treehash.NewBlockHasher()
	_, _ = hasher.Write(data)
	return treehash.SumToHex(treehash.ComputeTreeHash(hasher.Sum()))
}

// fixedResolver is the integration ChannelResolver: it honours an explicitly
// requested channel and otherwise answers with its configured one.
type fixedResolver struct {
	// channelID is the channel returned when the request asks for none.
	channelID int64
	// calls counts Resolve calls, so a test can prove a retry reused the stored
	// part instead of resolving a channel again.
	calls int
}

// Resolve returns requested when it is non-zero, otherwise the configured
// channel, counting the call either way.
func (r *fixedResolver) Resolve(_ context.Context, _ int64, requested int64) (int64, error) {
	r.calls++
	if requested != 0 {
		return requested, nil
	}
	return r.channelID, nil
}

// memoryStorage is the in-memory Storage the integration tests run against: it
// keeps every uploaded payload, serves them back as ranges and metadata, and
// records deletes and session usage so the pipeline's behaviour can be asserted
// without Telegram.
type memoryStorage struct {
	// mu guards every other field, because the pipeline reads and writes them
	// from its own goroutines.
	mu sync.Mutex
	// uploads holds each uploaded payload in publish order, one entry per
	// Upload call.
	uploads [][]byte
	// messages maps a message ID to the payload it holds, which is what
	// metadata and range reads resolve against.
	messages map[int64][]byte
	// deleted collects the message IDs passed to DeleteMessages.
	deleted []int64
	// rangeRequests records every range read, so a test can assert how reads
	// were batched.
	rangeRequests []telegramstore.RangeRequest
	// nextID is the message ID handed to the next upload; it only ever grows, so
	// IDs are never reused after a delete.
	nextID int64
	// sessionOpens counts download sessions opened by the pipeline.
	sessionOpens int
	// sessionCloses counts their closes, which must balance the opens for a
	// download that releases what it took.
	sessionCloses int
}

// OpenDownloadSession counts the open and returns a session bound to this
// storage, so every range of one download shares it.
func (s *memoryStorage) OpenDownloadSession(context.Context, int64) (telegramstore.DownloadSession, error) {
	s.mu.Lock()
	s.sessionOpens++
	s.mu.Unlock()
	return &memoryDownloadSession{storage: s}, nil
}

// memoryDownloadSession is the single session one download holds for all of its
// metadata and range reads.
type memoryDownloadSession struct {
	// storage is the fake the session resolves payloads in.
	storage *memoryStorage
	// closed records that Close already ran, so a double close is counted once.
	closed bool
}

// Metadata looks the message up in the fake and reports its stored size, or
// ErrMessageNotFound for an unknown message.
func (s *memoryDownloadSession) Metadata(_ context.Context, request telegramstore.MetadataRequest) (telegramstore.StoredPart, error) {
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	payload, ok := s.storage.messages[request.MessageID]
	if !ok {
		return telegramstore.StoredPart{}, telegramstore.ErrMessageNotFound
	}
	return telegramstore.StoredPart{ChannelID: request.ChannelID, MessageID: request.MessageID, Size: int64(len(payload))}, nil
}

// OpenRange serves the range through the storage, which records it.
func (s *memoryDownloadSession) OpenRange(ctx context.Context, request telegramstore.RangeRequest) (io.ReadCloser, error) {
	return s.storage.OpenRange(ctx, request)
}

// Close counts the first close and ignores later ones, so session accounting
// stays balanced for a caller that closes twice.
func (s *memoryDownloadSession) Close() error {
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.storage.sessionCloses++
	}
	return nil
}

// Upload reads the body, stores a private copy under a fresh message ID, and
// reports it back as the published part.
func (s *memoryStorage) Upload(_ context.Context, request telegramstore.UploadRequest) (telegramstore.StoredPart, error) {
	payload, err := io.ReadAll(request.Reader)
	if err != nil {
		return telegramstore.StoredPart{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	copyPayload := append([]byte(nil), payload...)
	s.uploads = append(s.uploads, copyPayload)
	if s.messages == nil {
		s.messages = make(map[int64][]byte)
	}
	s.messages[s.nextID] = copyPayload
	return telegramstore.StoredPart{ChannelID: request.ChannelID, MessageID: s.nextID, Size: int64(len(payload))}, nil
}

// OpenRange returns a reader over the requested window of a stored payload,
// rejecting a negative offset or length like the storage contract, and records
// the request.
func (s *memoryStorage) OpenRange(_ context.Context, request telegramstore.RangeRequest) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, ok := s.messages[request.MessageID]
	if !ok {
		return nil, telegramstore.ErrMessageNotFound
	}
	if request.Offset < 0 || request.Offset > int64(len(payload)) || request.Length < -1 {
		return nil, telegramstore.ErrInvalidRequest
	}
	end := int64(len(payload))
	if request.Length >= 0 && request.Offset+request.Length < end {
		end = request.Offset + request.Length
	}
	s.rangeRequests = append(s.rangeRequests, request)
	return io.NopCloser(bytes.NewReader(append([]byte(nil), payload[request.Offset:end]...))), nil
}

// DeleteMessages records the IDs and removes the messages, so a later read of a
// deleted part fails like production would.
func (s *memoryStorage) DeleteMessages(_ context.Context, _ int64, _ int64, ids []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, ids...)
	for _, id := range ids {
		delete(s.messages, id)
	}
	return nil
}

// CopyPart is unused by these tests and reports an error.
func (s *memoryStorage) CopyPart(context.Context, int64, int64, int64, int64) (telegramstore.StoredPart, error) {
	return telegramstore.StoredPart{}, errors.New("not implemented")
}

// CreateChannel is unused by these tests and reports an error.
func (s *memoryStorage) CreateChannel(context.Context, int64, string) (telegramstore.Channel, error) {
	return telegramstore.Channel{}, errors.New("not implemented")
}

// DeleteChannel is a no-op; these tests never delete a channel.
func (s *memoryStorage) DeleteChannel(context.Context, int64, int64) error { return nil }

// payload returns a copy of the index-th uploaded payload, so a caller cannot
// mutate what the fake stored.
func (s *memoryStorage) payload(index int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.uploads[index]...)
}

// uploadCount returns how many uploads the fake has accepted.
func (s *memoryStorage) uploadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.uploads)
}

// panicReader is a body that must never be read: a retried part is already
// stored, so reading it would mean the pipeline uploaded the bytes twice.
type panicReader struct{}

// Read panics, turning an unwanted body read into an immediate test failure.
func (panicReader) Read([]byte) (int, error) { panic("retry body was read") }
