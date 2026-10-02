package fileops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
)

func TestServiceValidationAndUUIDConversion(t *testing.T) {
	t.Parallel()
	if _, err := NewService(nil, nil, nil, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("NewService() error = %v", err)
	}
	s := &Service{}
	if _, err := s.Copy(context.Background(), CopyInput{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Copy() error = %v", err)
	}
	blank := " \t "
	if _, err := s.Copy(context.Background(), CopyInput{UserID: 1, FileID: uuid.New(), Name: &blank}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Copy() with a blank name error = %v", err)
	}
	if err := s.Purge(context.Background(), 0, uuid.Nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Purge() error = %v", err)
	}

	if _, err := s.CleanTrash(context.Background(), 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("CleanTrash() error = %v", err)
	}
	ids := []uuid.UUID{uuid.New(), uuid.New()}
	converted := pgUUIDs(ids)
	if len(converted) != len(ids) {
		t.Fatalf("pgUUIDs() length = %d", len(converted))
	}
	for index, id := range ids {
		if !converted[index].Valid || uuid.UUID(converted[index].Bytes) != id {
			t.Fatalf("pgUUIDs()[%d] = %#v", index, converted[index])
		}
	}
}

// copyPartResponse is one scripted storage answer: the part CopyPart reports, which
// stays zero when the storage cannot identify the message it published, and the error
// it reports with it.
type copyPartResponse struct {
	part telegramstore.StoredPart
	err  error
}

// deletedMessages is one recorded DeleteMessages call, so the tests can prove that a
// published message really is handed to the compensating delete.
type deletedMessages struct {
	channelID  int64
	messageIDs []int64
}

// copyPartStorage is a telegramstore.Storage that replays scripted CopyPart responses
// and records the deletes issued afterwards. It lets the copy loop and its
// compensation run without a database or a Telegram account; every method the copy
// path does not use fails loudly instead of silently succeeding.
type copyPartStorage struct {
	responses []copyPartResponse
	copyCalls int
	deletes   []deletedMessages
}

func (*copyPartStorage) Upload(context.Context, telegramstore.UploadRequest) (telegramstore.StoredPart, error) {
	return telegramstore.StoredPart{}, errors.New("not used")
}

func (*copyPartStorage) OpenRange(context.Context, telegramstore.RangeRequest) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

func (s *copyPartStorage) DeleteMessages(_ context.Context, _ int64, channelID int64, messageIDs []int64) error {
	s.deletes = append(s.deletes, deletedMessages{channelID: channelID, messageIDs: append([]int64(nil), messageIDs...)})
	return nil
}

func (s *copyPartStorage) CopyPart(context.Context, int64, int64, int64, int64) (telegramstore.StoredPart, error) {
	if s.copyCalls >= len(s.responses) {
		return telegramstore.StoredPart{}, errors.New("unexpected CopyPart call")
	}
	response := s.responses[s.copyCalls]
	s.copyCalls++
	return response.part, response.err
}

func (*copyPartStorage) CreateChannel(context.Context, int64, string) (telegramstore.Channel, error) {
	return telegramstore.Channel{}, errors.New("not used")
}

func (*copyPartStorage) DeleteChannel(context.Context, int64, int64) error { return nil }

// deletionsByChannel flattens the recorded deletes into the grouped form the
// compensation builds, so a test does not depend on the order channels are visited.
func (s *copyPartStorage) deletionsByChannel() map[int64][]int64 {
	grouped := make(map[int64][]int64)
	for _, call := range s.deletes {
		grouped[call.channelID] = append(grouped[call.channelID], call.messageIDs...)
	}
	return grouped
}

// TestCopyPartsCompensatesIdentifiedMessages pins the rule that a message the storage
// names is tracked even when the copy fails, so the compensating delete is not a
// no-op: the size mismatch branch deletes the copy the storage just published, a
// storage that reports the part alongside the error is compensated too, and only a
// response that names no message has nothing to delete and is left to the cleanup
// sweep.
func TestCopyPartsCompensatesIdentifiedMessages(t *testing.T) {
	t.Parallel()

	fileID := uuid.New()
	mismatch := fmt.Errorf("%w: copied 5, source 4", telegramstore.ErrSizeMismatch)
	recordedSize := pgtype.Int8{Int64: 4, Valid: true}
	sourcePart := func(fileID pgtype.UUID, storedSize pgtype.Int8) *sqlcgen.FilePart {
		return &sqlcgen.FilePart{
			FileID: fileID, PartNo: 1, ChannelID: 9001, MessageID: 77,
			PlainSize: pgtype.Int8{Int64: 4, Valid: true}, StoredSize: storedSize,
		}
	}

	for _, test := range []struct {
		name        string
		part        *sqlcgen.FilePart
		response    copyPartResponse
		wantErr     error
		wantCopied  int
		wantDeleted map[int64][]int64
	}{
		{
			name:       "copied part is kept without a failing delete",
			part:       sourcePart(dbtypes.UUID(fileID), recordedSize),
			response:   copyPartResponse{part: telegramstore.StoredPart{ChannelID: 9002, MessageID: 501, Size: 4}},
			wantCopied: 1,
		},
		{
			name:        "size mismatch deletes the published copy",
			part:        sourcePart(dbtypes.UUID(fileID), recordedSize),
			response:    copyPartResponse{part: telegramstore.StoredPart{ChannelID: 9002, MessageID: 501, Size: 5}},
			wantErr:     telegramstore.ErrSizeMismatch,
			wantDeleted: map[int64][]int64{9002: {501}},
		},
		{
			name:        "unrecorded stored size deletes the published copy",
			part:        sourcePart(dbtypes.UUID(fileID), pgtype.Int8{}),
			response:    copyPartResponse{part: telegramstore.StoredPart{ChannelID: 9002, MessageID: 501, Size: 4}},
			wantErr:     telegramstore.ErrSizeMismatch,
			wantDeleted: map[int64][]int64{9002: {501}},
		},
		{
			name:        "part returned with the size mismatch error is deleted",
			part:        sourcePart(dbtypes.UUID(fileID), recordedSize),
			response:    copyPartResponse{part: telegramstore.StoredPart{ChannelID: 9002, MessageID: 501, Size: 5}, err: mismatch},
			wantErr:     telegramstore.ErrSizeMismatch,
			wantDeleted: map[int64][]int64{9002: {501}},
		},
		{
			name:     "unnamed published message has nothing to delete",
			part:     sourcePart(dbtypes.UUID(fileID), recordedSize),
			response: copyPartResponse{err: mismatch},
			wantErr:  telegramstore.ErrSizeMismatch,
		},
		{
			name:     "undecodable source file id is not copied",
			part:     sourcePart(pgtype.UUID{}, recordedSize),
			response: copyPartResponse{},
			wantErr:  ErrNotFound,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			storage := &copyPartStorage{responses: []copyPartResponse{test.response}}
			service := &Service{storage: storage}

			copied, published, err := service.copyParts(context.Background(), 42, []*sqlcgen.FilePart{test.part}, []int64{9002})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("copyParts() error = %v, want %v", err, test.wantErr)
			}
			if len(copied) != test.wantCopied {
				t.Fatalf("copyParts() copied files = %d, want %d", len(copied), test.wantCopied)
			}
			if test.wantErr == nil {
				if len(copied[fileID]) != 1 || len(published) != 1 {
					t.Fatalf("copyParts() copied = %#v, published = %#v", copied, published)
				}
			}
			// Copy compensates on the error path only; a successful copy keeps its
			// messages until a later step fails.
			if test.wantErr != nil {
				service.deleteCopiedParts(42, published)
			}

			grouped := storage.deletionsByChannel()
			if len(grouped) != len(test.wantDeleted) || !maps.EqualFunc(grouped, test.wantDeleted, slices.Equal) {
				t.Fatalf("compensating deletes = %#v, want %#v", grouped, test.wantDeleted)
			}
		})
	}
}

// TestDeleteCopiedPartsGroupsMessagesByChannel pins the batching and the skip rule:
// every message of a channel is deleted in one call and an entry without a channel or
// message ID never reaches the storage, which would reject it as an invalid request.
func TestDeleteCopiedPartsGroupsMessagesByChannel(t *testing.T) {
	t.Parallel()

	storage := &copyPartStorage{}
	service := &Service{storage: storage}
	service.deleteCopiedParts(42, []telegramstore.StoredPart{
		{ChannelID: 9002, MessageID: 5},
		{ChannelID: 9003, MessageID: 7},
		{ChannelID: 9002, MessageID: 6},
		{MessageID: 9},
		{ChannelID: 9003},
	})

	grouped := storage.deletionsByChannel()
	want := map[int64][]int64{9002: {5, 6}, 9003: {7}}
	if !maps.EqualFunc(grouped, want, slices.Equal) {
		t.Fatalf("compensating deletes = %#v, want %#v", grouped, want)
	}
	if len(storage.deletes) != len(want) {
		t.Fatalf("DeleteMessages calls = %d, want %d", len(storage.deletes), len(want))
	}
}
