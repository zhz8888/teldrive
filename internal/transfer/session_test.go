package transfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/zhz8888/teldrive/v2/internal/telegramstore"
)

// stubMetadataStorage implements Storage without the optional MetadataReader, so
// the adapter has to report that the capability is missing rather than reporting a
// download that cannot work.
type stubPlainStorage struct {
	opener func() (io.ReadCloser, error)
}

func (s stubPlainStorage) Upload(context.Context, telegramstore.UploadRequest) (telegramstore.StoredPart, error) {
	return telegramstore.StoredPart{}, errors.New("not used")
}

func (s stubPlainStorage) OpenRange(context.Context, telegramstore.RangeRequest) (io.ReadCloser, error) {
	if s.opener == nil {
		return nil, errors.New("no opener")
	}
	return s.opener()
}

func (s stubPlainStorage) DeleteMessages(context.Context, int64, int64, []int64) error { return nil }

func (s stubPlainStorage) CopyPart(context.Context, int64, int64, int64, int64) (telegramstore.StoredPart, error) {
	return telegramstore.StoredPart{}, errors.New("not used")
}

func (s stubPlainStorage) CreateChannel(context.Context, int64, string) (telegramstore.Channel, error) {
	return telegramstore.Channel{}, errors.New("not used")
}

func (s stubPlainStorage) DeleteChannel(context.Context, int64, int64) error { return nil }

// metadataCapableStorage adds the optional reader on top of the plain storage.
type metadataCapableStorage struct {
	stubPlainStorage
	part telegramstore.StoredPart
	err  error
}

func (s metadataCapableStorage) Metadata(context.Context, telegramstore.MetadataRequest) (telegramstore.StoredPart, error) {
	return s.part, s.err
}

// TestStorageDownloadSessionReportsAMissingMetadataReader keeps a storage that
// cannot describe a document from being handed to the download path: the caller
// has to learn the capability is absent, not receive an empty part that would be
// stored as a zero-byte file.
func TestStorageDownloadSessionReportsAMissingMetadataReader(t *testing.T) {
	t.Parallel()
	session := storageDownloadSession{storage: stubPlainStorage{}}

	part, err := session.Metadata(context.Background(), telegramstore.MetadataRequest{})
	if !errors.Is(err, ErrDownloadNotConfigured) {
		t.Fatalf("Metadata() error = %v, want ErrDownloadNotConfigured", err)
	}
	if part.Size != 0 || part.MessageID != 0 {
		t.Fatalf("Metadata() = %+v, want the zero value alongside the error", part)
	}
}

// TestStorageDownloadSessionForwardsToAMetadataReader checks the delegation: when
// the storage does support the capability, its answer has to reach the caller
// unchanged, including its failure.
func TestStorageDownloadSessionForwardsToAMetadataReader(t *testing.T) {
	t.Parallel()
	want := telegramstore.StoredPart{ChannelID: 9001, MessageID: 42, Size: 120}
	session := storageDownloadSession{storage: metadataCapableStorage{part: want}}

	got, err := session.Metadata(context.Background(), telegramstore.MetadataRequest{})
	if err != nil {
		t.Fatalf("Metadata() error = %v", err)
	}
	if got != want {
		t.Fatalf("Metadata() = %+v, want %+v", got, want)
	}

	cause := errors.New("document gone")
	failing := storageDownloadSession{storage: metadataCapableStorage{err: cause}}
	if _, err := failing.Metadata(context.Background(), telegramstore.MetadataRequest{}); !errors.Is(err, cause) {
		t.Fatalf("Metadata() error = %v, want the storage's failure passed through", err)
	}
}

// TestStorageDownloadSessionOpensRangesThroughTheStorage keeps the adapter from
// doing anything clever: a range request is the storage's job, including the
// reader it hands back and the caller that must close it.
func TestStorageDownloadSessionOpensRangesThroughTheStorage(t *testing.T) {
	t.Parallel()
	body := "downloaded bytes"
	session := storageDownloadSession{storage: stubPlainStorage{
		opener: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil },
	}}

	reader, err := session.OpenRange(context.Background(), telegramstore.RangeRequest{
		UserID: 1001, ChannelID: 9001, MessageID: 42, Offset: 0, Length: int64(len(body)),
	})
	if err != nil {
		t.Fatalf("OpenRange() error = %v", err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read error = %v", err)
	}
	if string(got) != body {
		t.Fatalf("range body = %q, want %q", got, body)
	}
}

// TestNopDownloadReaderClosesCleanly covers the zero-length download, which reads
// from an empty source and holds no session at all. Its Close has to be a safe
// no-op, because the download path closes whatever reader it is given.
func TestNopDownloadReaderClosesCleanly(t *testing.T) {
	t.Parallel()
	// The embedded reader is the empty source a zero-length download reads from.
	reader := nopDownloadReader{bytes.NewReader(nil)}
	got, err := io.ReadAll(&reader)
	if err != nil {
		t.Fatalf("read error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("zero-length download read %q, want nothing", got)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
}
