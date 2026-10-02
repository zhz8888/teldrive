package telegramstore

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/gotd/td/tg"
)

// countingManageRunner serves management operations without Telegram calls and
// counts how often it was asked to run, so a test can assert that validation
// rejected a request before any work started.
type countingManageRunner struct{ runs atomic.Int32 }

func (r *countingManageRunner) Run(ctx context.Context, _ int64, operation Operation, fn func(context.Context, *tg.Client) error) error {
	if operation != OperationManage {
		return ErrInvalidRequest
	}
	r.runs.Add(1)
	return fn(ctx, new(tg.Client))
}

func TestGotdStorageMetadataValidatesRequest(t *testing.T) {
	runner := &countingManageRunner{}
	storage := &GotdStorage{runner: runner}
	for _, request := range []MetadataRequest{
		{UserID: 0, ChannelID: 7},
		{UserID: 1, ChannelID: 0},
		{UserID: 1, ChannelID: 7, MessageID: -1},
	} {
		if _, err := storage.Metadata(context.Background(), request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Metadata(%+v) error = %v, want ErrInvalidRequest", request, err)
		}
	}
	if got := runner.runs.Load(); got != 0 {
		t.Fatalf("runner calls = %d, want 0", got)
	}
}

func TestGotdStorageOpenRangeValidatesRequest(t *testing.T) {
	runner := &countingManageRunner{}
	storage := &GotdStorage{runner: runner}
	for _, request := range []RangeRequest{
		{UserID: 0, ChannelID: 7, MessageID: 1},
		{UserID: 1, ChannelID: 0, MessageID: 1},
		{UserID: 1, ChannelID: 7, MessageID: 0},
		{UserID: 1, ChannelID: 7, MessageID: 1, Offset: -1},
		{UserID: 1, ChannelID: 7, MessageID: 1, Length: -2},
	} {
		reader, err := storage.OpenRange(context.Background(), request)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("OpenRange(%+v) error = %v, want ErrInvalidRequest", request, err)
		}
		if reader != nil {
			t.Fatalf("OpenRange(%+v) returned a reader with an error", request)
		}
	}
	if got := runner.runs.Load(); got != 0 {
		t.Fatalf("runner calls = %d, want 0", got)
	}
}

func TestGotdDownloadSessionValidatesRequestLikeStorage(t *testing.T) {
	session, err := (&GotdStorage{runner: &sessionCountingRunner{}}).OpenDownloadSession(context.Background(), 7)
	if err != nil {
		t.Fatalf("OpenDownloadSession() error = %v", err)
	}
	defer func() {
		if err := session.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	for _, request := range []MetadataRequest{
		{UserID: 0, ChannelID: 7},
		{UserID: 1, ChannelID: 0},
		{UserID: 1, ChannelID: 7, MessageID: -1},
	} {
		if _, err := session.Metadata(context.Background(), request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Metadata(%+v) error = %v, want ErrInvalidRequest", request, err)
		}
	}
	for _, request := range []RangeRequest{
		{UserID: 0, ChannelID: 7, MessageID: 1},
		{UserID: 1, ChannelID: 0, MessageID: 1},
		{UserID: 1, ChannelID: 7, MessageID: 0},
		{UserID: 1, ChannelID: 7, MessageID: 1, Offset: -1},
		{UserID: 1, ChannelID: 7, MessageID: 1, Length: -2},
	} {
		reader, err := session.OpenRange(context.Background(), request)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("OpenRange(%+v) error = %v, want ErrInvalidRequest", request, err)
		}
		if reader != nil {
			t.Fatalf("OpenRange(%+v) returned a reader with an error", request)
		}
	}
}

func TestGotdDownloadSessionReportsClientUnavailableAfterClose(t *testing.T) {
	session, err := (&GotdStorage{runner: &sessionCountingRunner{}}).OpenDownloadSession(context.Background(), 7)
	if err != nil {
		t.Fatalf("OpenDownloadSession() error = %v", err)
	}
	if _, err := session.(*gotdDownloadSession).client(); err != nil {
		t.Fatalf("client() before Close error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := session.(*gotdDownloadSession).client(); !errors.Is(err, ErrClientUnavailable) {
		t.Fatalf("client() after Close error = %v, want ErrClientUnavailable", err)
	}
	if _, err := session.Metadata(context.Background(), MetadataRequest{UserID: 7, ChannelID: 1, MessageID: 1}); !errors.Is(err, ErrClientUnavailable) {
		t.Fatalf("Metadata() after Close error = %v, want ErrClientUnavailable", err)
	}
	if _, err := session.OpenRange(context.Background(), RangeRequest{UserID: 7, ChannelID: 1, MessageID: 1, Length: -1}); !errors.Is(err, ErrClientUnavailable) {
		t.Fatalf("OpenRange() after Close error = %v, want ErrClientUnavailable", err)
	}
}

func TestGotdStorageDeleteMessagesRejectsInvalidIDBeforeDeleting(t *testing.T) {
	runner := &countingManageRunner{}
	storage := &GotdStorage{runner: runner}
	ids := make([]int64, 0, 150)
	for id := int64(1); id <= 150; id++ {
		ids = append(ids, id)
	}
	ids[120] = 0 // Invalid ID in the second batch.

	if err := storage.DeleteMessages(context.Background(), 42, 7, ids); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("DeleteMessages() error = %v, want ErrInvalidRequest", err)
	}
	if got := runner.runs.Load(); got != 0 {
		t.Fatalf("runner calls = %d, want 0 before any deletion", got)
	}
}

func TestGotdStorageDeleteMessagesKeepsEmptySliceNoop(t *testing.T) {
	runner := &countingManageRunner{}
	storage := &GotdStorage{runner: runner}
	if err := storage.DeleteMessages(context.Background(), 42, 7, nil); err != nil {
		t.Fatalf("DeleteMessages(nil) error = %v", err)
	}
	if got := runner.runs.Load(); got != 0 {
		t.Fatalf("runner calls = %d, want 0", got)
	}
}
