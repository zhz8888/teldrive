//go:build integration

package channels_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zhz8888/teldrive/v2/internal/channels"
	testpostgres "github.com/zhz8888/teldrive/v2/internal/testutil/postgres"
)

func TestChannelAdminLifecycleAgainstRealPostgres(t *testing.T) {
	db := testpostgres.New(t)
	seedChannelOwner(t, db.Pool, 1001)
	creator := &fakeCreator{nextID: 9100}
	service := channels.NewService(db.Pool, creator, channels.Config{PartLimit: 100, AutoCreate: true, NamePrefix: "storage"})
	ctx := context.Background()

	first, err := service.Create(ctx, 1001, "first", true)
	if err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	second, err := service.Create(ctx, 1001, "second", false)
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	if !first.Selected || second.Selected {
		t.Fatalf("created selections = first %v second %v", first.Selected, second.Selected)
	}
	rows, err := service.List(ctx, channels.ListInput{UserID: 1001, Limit: 10})
	if err != nil || len(rows) != 2 {
		t.Fatalf("List() = %#v, %v", rows, err)
	}
	page, err := service.List(ctx, channels.ListInput{UserID: 1001, Limit: 1})
	if err != nil || len(page) != 1 {
		t.Fatalf("List(page) = %#v, %v", page, err)
	}
	afterCreatedAt := page[0].CreatedAt.Time
	afterChannelID := page[0].ChannelID
	nextPage, err := service.List(ctx, channels.ListInput{
		UserID: 1001, AfterCreatedAt: &afterCreatedAt, AfterChannelID: &afterChannelID, Limit: 10,
	})
	if err != nil || len(nextPage) != 1 {
		t.Fatalf("List(next page) = %#v, %v", nextPage, err)
	}
	selected, err := service.Select(ctx, 1001, second.ChannelID)
	if err != nil || !selected.Selected {
		t.Fatalf("Select() = %#v, %v", selected, err)
	}
	if err := service.Delete(ctx, 1001, first.ChannelID); err != nil {
		t.Fatalf("Delete(first) error = %v", err)
	}
	if creator.deleteCalls() != 1 {
		t.Fatalf("remote deletes = %d", creator.deleteCalls())
	}
	if err := service.Delete(ctx, 1001, first.ChannelID); !errors.Is(err, channels.ErrInvalidChannel) {
		t.Fatalf("Delete(gone) error = %v", err)
	}
	if creator.deleteCalls() != 1 {
		t.Fatalf("remote deletes after refused row delete = %d, want 1", creator.deleteCalls())
	}
	if err := service.Delete(ctx, 1001, second.ChannelID); !errors.Is(err, channels.ErrSelectedChannel) {
		t.Fatalf("Delete(selected) error = %v", err)
	}
	third, err := service.Create(ctx, 1001, "referenced", false)
	if err != nil {
		t.Fatalf("Create(third) error = %v", err)
	}
	insertStoredPart(t, db.Pool, 1001, third.ChannelID, 123)
	if err := service.Delete(ctx, 1001, third.ChannelID); !errors.Is(err, channels.ErrChannelInUse) {
		t.Fatalf("Delete(referenced) error = %v", err)
	}
	unnamed, err := service.Create(ctx, 1001, "", false)
	if err != nil || unnamed.Name == "" {
		t.Fatalf("Create(default name) = %#v, %v", unnamed, err)
	}

	insertChannel(t, db.Pool, 1001, 9999, false)
	conflictingCreator := &fakeCreator{fixedID: 9999}
	conflictingService := channels.NewService(db.Pool, conflictingCreator, channels.Config{PartLimit: 100, AutoCreate: true, NamePrefix: "storage"})
	if _, err := conflictingService.Create(ctx, 1001, "conflict", false); err == nil {
		t.Fatal("expected create conflict")
	}
	if conflictingCreator.deleteCalls() != 1 {
		t.Fatalf("create compensation deletes = %d", conflictingCreator.deleteCalls())
	}
}

// TestChannelCreateReportsASelectionRaceAsConflict covers the concurrent path:
// two callers may both create a selected channel for the same user and both clear
// the previous selection before either selects, so the later one collides with the
// partial unique index that keeps one selection per user. That collision is a
// conflict rather than a fault and has to be reported as
// channels.ErrSelectedChannel, the sentinel the API answers 409 with, instead of
// escaping as an unmapped error. The Telegram channel the losing creation had
// already made must be deleted again, because the rollback leaves no row that
// could ever point at it.
//
// The race is arranged deterministically. A second connection inserts a selected
// channel for the same user and holds the transaction open: the creation cannot
// see that row, so its clear step finds nothing to deselect, and its own selection
// then waits on the index entry the holder wrote. Committing the holder is what
// every real race does a moment later, and it turns the wait into the unique
// violation.
func TestChannelCreateReportsASelectionRaceAsConflict(t *testing.T) {
	db := testpostgres.New(t)
	seedChannelOwner(t, db.Pool, 1001)
	creator := &fakeCreator{nextID: 9100}
	service := channels.NewService(db.Pool, creator, channels.Config{PartLimit: 100, AutoCreate: true, NamePrefix: "storage"})
	ctx := context.Background()

	holder, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	holderTx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holderTx.Rollback(ctx)
	if _, err := holderTx.Exec(ctx,
		"INSERT INTO channels (channel_id, user_id, name, selected) VALUES (9999, 1001, 'holder', true)"); err != nil {
		t.Fatal(err)
	}

	createErr := make(chan error, 1)
	go func() {
		_, err := service.Create(ctx, 1001, "raced", true)
		createErr <- err
	}()
	waitForBlockedBackend(t, ctx, db.Pool)
	if err := holderTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-createErr; !errors.Is(err, channels.ErrSelectedChannel) {
		t.Fatalf("Create() error = %v, want channels.ErrSelectedChannel", err)
	}
	if creator.deleteCalls() != 1 {
		t.Fatalf("compensating deletes = %d, want 1 for the channel the failed creation made", creator.deleteCalls())
	}
	var rows int
	if err := db.Pool.QueryRow(ctx,
		"SELECT count(*) FROM channels WHERE user_id = 1001 AND channel_id <> 9999").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("channels left behind = %d, want the failed creation rolled back", rows)
	}
}

// waitForBlockedBackend waits until a PostgreSQL backend is blocked on a lock,
// which in these tests is the creation that collided on the selected-channel
// index, and fails instead of hanging when that never happens.
func waitForBlockedBackend(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the concurrent creation never waited on the selected-channel index")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
