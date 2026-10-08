package channels

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/zhz8888/teldrive/v2/internal/telegramstore"
)

func TestTelegramCreator(t *testing.T) {
	t.Parallel()
	storage := &creatorStorage{created: telegramstore.Channel{ID: 99, Name: "storage"}}
	creator := TelegramCreator{Storage: storage}
	created, err := creator.Create(context.Background(), 1, "storage")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.ID != 99 || created.Name != "storage" {
		t.Fatalf("Create() = %#v", created)
	}
	if err := creator.Delete(context.Background(), 1, 99); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if storage.deleted != 99 {
		t.Fatalf("deleted channel = %d", storage.deleted)
	}
}

func TestTelegramCreatorRejectsMissingStorage(t *testing.T) {
	t.Parallel()
	creator := TelegramCreator{}
	if _, err := creator.Create(context.Background(), 1, "storage"); err == nil {
		t.Fatal("expected Create error")
	}
	if err := creator.Delete(context.Background(), 1, 99); err == nil {
		t.Fatal("expected Delete error")
	}
}

// creatorStorage is a telegramstore.Storage stand-in for TelegramCreator: it reports
// a scripted channel from CreateChannel and remembers the channel DeleteChannel was
// asked to remove. Every method the channel lifecycle does not use fails loudly
// instead of quietly succeeding.
type creatorStorage struct {
	// created is the channel CreateChannel reports.
	created telegramstore.Channel
	// deleted is the channel ID DeleteChannel last received.
	deleted int64
}

// Upload reports an error: TelegramCreator must never publish an upload.
func (*creatorStorage) Upload(context.Context, telegramstore.UploadRequest) (telegramstore.StoredPart, error) {
	return telegramstore.StoredPart{}, errors.New("not used")
}

// OpenRange reports an error: TelegramCreator must never read stored bytes.
func (*creatorStorage) OpenRange(context.Context, telegramstore.RangeRequest) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

// DeleteMessages is a no-op: the channel lifecycle only creates and deletes channels.
func (*creatorStorage) DeleteMessages(context.Context, int64, int64, []int64) error { return nil }

// CopyPart reports an error: TelegramCreator must never copy a part.
func (s *creatorStorage) CopyPart(context.Context, int64, int64, int64, int64) (telegramstore.StoredPart, error) {
	return telegramstore.StoredPart{}, errors.New("not implemented")
}

// CreateChannel reports the scripted channel, standing in for the Telegram API.
func (s *creatorStorage) CreateChannel(context.Context, int64, string) (telegramstore.Channel, error) {
	return s.created, nil
}

// DeleteChannel records the channel ID so the test can prove Delete forwarded it.
func (s *creatorStorage) DeleteChannel(_ context.Context, _ int64, channelID int64) error {
	s.deleted = channelID
	return nil
}
