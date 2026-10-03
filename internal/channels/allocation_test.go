package channels

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// stubCreator answers Create with whatever the test set up, so the allocation
// branches can be driven without a Telegram connection.
type stubCreator struct {
	channel RemoteChannel
	err     error
	created []string
	deleted []int64
}

func (s *stubCreator) Create(_ context.Context, _ int64, name string) (RemoteChannel, error) {
	s.created = append(s.created, name)
	if s.err != nil {
		return RemoteChannel{}, s.err
	}
	return s.channel, nil
}

func (s *stubCreator) Delete(_ context.Context, _ int64, id int64) error {
	s.deleted = append(s.deleted, id)
	return nil
}

func newStubbedService(creator Creator) *Service {
	return &Service{
		creator: creator,
		config:  Config{NamePrefix: "storage", PartLimit: 100},
		now:     func() time.Time { return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC) },
	}
}

// TestCreateSelectedChannelRefusesToRunWithoutACreator keeps the allocation path
// explicit: a service built without a Telegram backend must say so instead of
// panicking on a nil creator.
func TestCreateSelectedChannelRefusesToRunWithoutACreator(t *testing.T) {
	t.Parallel()
	service := newStubbedService(nil)
	_, err := service.createSelectedChannel(context.Background(), nil, nil, 1001)
	if err == nil {
		t.Fatal("createSelectedChannel() error = nil, want a missing-creator error")
	}
	if !strings.Contains(err.Error(), "creator") {
		t.Fatalf("createSelectedChannel() error = %v, want it to name the missing creator", err)
	}
}

// TestCreateSelectedChannelStopsOnAnEmptyTelegramID keeps a Telegram response with
// no channel id from reaching the database as a channel row that could never be
// used, and asks Telegram to remove whatever it did create.
func TestCreateSelectedChannelStopsOnAnEmptyTelegramID(t *testing.T) {
	t.Parallel()
	creator := &stubCreator{channel: RemoteChannel{ID: 0, Name: "nameless"}}
	service := newStubbedService(creator)

	if _, err := service.createSelectedChannel(context.Background(), nil, nil, 1001); err == nil {
		t.Fatal("createSelectedChannel() error = nil, want the empty id reported")
	}
}

// TestCreateSelectedChannelReportsACreatorFailure unchanged from Telegram
// verbatim enough to diagnose: the caller logs this, and "create Telegram
// channel: ..." is what makes the line readable.
func TestCreateSelectedChannelReportsACreatorFailureUnchangedFromTelegram(t *testing.T) {
	t.Parallel()
	cause := errors.New("CHANNELS_TOO_MUCH")
	creator := &stubCreator{err: cause}
	service := newStubbedService(creator)

	_, err := service.createSelectedChannel(context.Background(), nil, nil, 1001)
	if !errors.Is(err, cause) {
		t.Fatalf("createSelectedChannel() error = %v, want it to wrap the Telegram failure", err)
	}
	if !strings.Contains(err.Error(), "create Telegram channel") {
		t.Fatalf("createSelectedChannel() error = %v, want it to name the failing step", err)
	}
}
