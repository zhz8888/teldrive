//go:build integration

package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"

	"github.com/zhz8888/teldrive/v2/internal/database"
	"github.com/zhz8888/teldrive/v2/internal/jobs"
	"github.com/zhz8888/teldrive/v2/internal/telegramstore"
	testpostgres "github.com/zhz8888/teldrive/v2/internal/testutil/postgres"
)

func TestOrphanCleanupDeletesOnlyExpiredUnreferencedDocuments(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedCleanupOwner(t, db.Pool)
	if _, err := db.Pool.Exec(ctx, `
WITH session AS (
  INSERT INTO upload_sessions (user_id, name, expected_size, mod_time, part_size, expires_at)
  VALUES (1001, 'active.bin', 1, now(), 1, now() + interval '7 days') RETURNING id
)
INSERT INTO upload_parts (upload_id, part_no, channel_id, message_id, plain_size, stored_size, state)
SELECT id, 1, 9001, 12, 1, 1, 'stored' FROM session`); err != nil {
		t.Fatal(err)
	}
	brokenID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO files (id, user_id, name, kind, size, mod_time)
VALUES ($1, 1001, 'broken.bin', 'file', 5, now())`, brokenID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO file_parts (file_id, part_no, channel_id, message_id)
VALUES ($1, 1, 9001, 99)`, brokenID.String()); err != nil {
		t.Fatal(err)
	}
	storage := &orphanStorage{messages: []telegramstore.DocumentMessage{
		{ID: 10, CreatedAt: time.Now().Add(-8 * 24 * time.Hour)},
		{ID: 11, CreatedAt: time.Now().Add(-6 * 24 * time.Hour)},
		{ID: 12, CreatedAt: time.Now().Add(-8 * 24 * time.Hour)},
	}}
	runtime, err := jobs.NewRuntime(db.Pool, storage)
	if err != nil {
		t.Fatal(err)
	}
	var template jobs.PeriodicTemplate
	for _, candidate := range runtime.PeriodicJobCatalog() {
		if candidate.Kind == jobs.OrphanCleanupKind {
			template = candidate
		}
	}
	if template.DefaultCronExpression != "@every 336h" || template.DefaultMaxAttempts != 3 || len(template.DefaultTags) != 0 {
		t.Fatalf("orphan cleanup template = %#v", template)
	}
	if err := runtime.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		if err := runtime.Stop(context.Background()); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	}()
	periodicJobs, err := runtime.ListPeriodicJobs(ctx)
	if err != nil {
		t.Fatalf("ListPeriodicJobs() error = %v", err)
	}
	found := false
	for _, periodicJob := range periodicJobs {
		if periodicJob.Kind == jobs.OrphanCleanupKind {
			found = true
			if periodicJob.Schedule.CronExpression != "@every 336h" {
				t.Fatalf("orphan cleanup schedule = %q", periodicJob.Schedule.CronExpression)
			}
			if len(periodicJob.Args) != 0 {
				t.Fatalf("orphan cleanup args = %#v", periodicJob.Args)
			}
		}
	}
	if !found {
		t.Fatal("orphan cleanup periodic job not persisted")
	}
	worker := jobs.NewOrphanedTelegramPartsCleanupWorker(db.Pool, storage, storage, 7*24*time.Hour)
	if got := worker.Timeout(nil); got != 4*time.Hour {
		t.Fatalf("Timeout() = %s", got)
	}
	if err := worker.Work(ctx, &river.Job[jobs.OrphanCleanupArgs]{Args: jobs.OrphanCleanupArgs{}}); err != nil {
		t.Fatal(err)
	}
	if len(storage.deleted) != 1 || storage.deleted[0] != 10 {
		t.Fatalf("deleted messages = %v, want [10]", storage.deleted)
	}
	for _, id := range storage.deleted {
		if id == 99 {
			t.Fatalf("deleted messages = %v, must not delete referenced message 99", storage.deleted)
		}
	}
	if len(storage.limits) != 1 || storage.limits[0] != 100 {
		t.Fatalf("Telegram page limits = %v, want [100]", storage.limits)
	}
}

// TestOrphanCleanupKeepsReferencedPartsAcrossPages drives the multi-page walk a
// real channel needs. The sweep deletes the orphans of every page, must not report
// a referenced part as broken only because it was listed on a later page, and must
// still report a part the channel never returned. It also pins the page cursor, so
// a walk that stopped after the first page would fail instead of passing quietly.
func TestOrphanCleanupKeepsReferencedPartsAcrossPages(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	seedCleanupOwner(t, db.Pool)
	present := uuid.New()
	missing := uuid.New()
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO files (id, user_id, name, kind, size, mod_time)
VALUES ($1, 1001, 'present.bin', 'file', 2, now()), ($2, 1001, 'missing.bin', 'file', 1, now())`,
		present.String(), missing.String()); err != nil {
		t.Fatal(err)
	}
	// present.bin has one part on the first page and one on the second; missing.bin
	// refers to a message the channel does not return at all.
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO file_parts (file_id, part_no, channel_id, message_id)
VALUES ($1, 1, 9001, 12), ($1, 2, 9001, 5), ($2, 1, 9001, 20)`, present.String(), missing.String()); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	storage := &pagedOrphanStorage{pages: map[int64]telegramstore.DocumentMessagePage{
		0: {Messages: []telegramstore.DocumentMessage{{ID: 50, CreatedAt: old}, {ID: 40, CreatedAt: old}}, BeforeID: 40},
		40: {
			Messages: []telegramstore.DocumentMessage{{ID: 12, CreatedAt: old}, {ID: 5, CreatedAt: old}},
			BeforeID: 5,
		},
		5: {Messages: []telegramstore.DocumentMessage{{ID: 3, CreatedAt: old}}, BeforeID: 3, Exhausted: true},
	}}
	worker := jobs.NewOrphanedTelegramPartsCleanupWorker(db.Pool, storage, storage, 7*24*time.Hour)
	testWorker := rivertest.NewWorker(t, riverpgxv5.New(db.Pool), &river.Config{Schema: database.DefaultSchema}, worker)
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	result, err := testWorker.Work(ctx, t, tx, jobs.OrphanCleanupArgs{}, nil)
	if err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	if result.EventKind != river.EventKindJobCompleted {
		t.Fatalf("Work() event = %v, want a completed job", result.EventKind)
	}
	raw := result.Job.Output()
	if len(raw) == 0 {
		t.Fatal("the completed job carries no recorded output")
	}
	var output jobs.OrphanCleanupOutput
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatalf("decode recorded output: %v", err)
	}
	if output.Channels != 1 || output.Scanned != 5 || output.Deleted != 3 {
		t.Fatalf("output counters = channels %d, scanned %d, deleted %d, want 1/5/3", output.Channels, output.Scanned, output.Deleted)
	}
	// Both referenced parts of present.bin were listed, one of them on the second
	// page, so only missing.bin may be reported with its unlisted part.
	if output.BrokenTotal != 1 || len(output.BrokenFiles) != 1 {
		t.Fatalf("broken files = %+v (total %d), want only missing.bin", output.BrokenFiles, output.BrokenTotal)
	}
	broken := output.BrokenFiles[0]
	if broken.FileID != missing.String() || broken.Name != "missing.bin" || len(broken.MissingMessageIDs) != 1 || broken.MissingMessageIDs[0] != 20 {
		t.Fatalf("broken file = %+v, want missing.bin with message 20", broken)
	}
	// The orphans of every page are deleted, and only those.
	if len(storage.deleted) != 3 {
		t.Fatalf("deleted messages = %v, want the three unreferenced ones", storage.deleted)
	}
	for index, want := range []int64{50, 40, 3} {
		if storage.deleted[index] != want {
			t.Fatalf("deleted messages = %v, want %v in page order", storage.deleted, []int64{50, 40, 3})
		}
	}
	if len(storage.requests) != 3 || storage.requests[0] != 0 || storage.requests[1] != 40 || storage.requests[2] != 5 {
		t.Fatalf("listing requests = %v, want the cursor to advance 0, 40, 5", storage.requests)
	}
}

// pagedOrphanStorage is the document lister and the deleting storage of a
// multi-page sweep: it answers each page by the BeforeID it is asked for and
// records what the sweep removed, so a test controls exactly where a message
// appears in the walk. Every other Storage method comes from the embedded
// orphanStorage and fails loudly.
type pagedOrphanStorage struct {
	// orphanStorage supplies the Storage methods this lister does not use and the
	// delete log the test reads back.
	orphanStorage
	// pages maps a requested BeforeID to the page the lister answers with; a
	// BeforeID that is missing from the map fails the sweep instead of passing an
	// unexpected cursor quietly.
	pages map[int64]telegramstore.DocumentMessagePage
	// requests records the BeforeID of every listing request.
	requests []int64
}

// ListDocumentMessages answers the page registered for the requested cursor and
// records the cursor, so a test can prove the walk advanced.
func (s *pagedOrphanStorage) ListDocumentMessages(_ context.Context, request telegramstore.ListDocumentMessagesRequest) (telegramstore.DocumentMessagePage, error) {
	s.requests = append(s.requests, request.BeforeID)
	page, ok := s.pages[request.BeforeID]
	if !ok {
		return telegramstore.DocumentMessagePage{}, fmt.Errorf("unexpected page request with BeforeID %d", request.BeforeID)
	}
	return page, nil
}

// orphanStorage is both the document lister and the deleting storage of the orphan
// sweep: it serves one fixed page of documents and records what the sweep removed,
// so the test can prove which documents were treated as orphans.
type orphanStorage struct {
	// messages is the single page the lister returns; the page is always exhausted,
	// so the sweep never pages past it.
	messages []telegramstore.DocumentMessage
	// deleted accumulates the message IDs the sweep selected for deletion.
	deleted []int64
	// limits records the page size of every listing request.
	limits []int
}

// ListDocumentMessages records the requested page size and returns the whole fixed
// page as exhausted.
func (s *orphanStorage) ListDocumentMessages(_ context.Context, request telegramstore.ListDocumentMessagesRequest) (telegramstore.DocumentMessagePage, error) {
	s.limits = append(s.limits, request.Limit)
	return telegramstore.DocumentMessagePage{Messages: s.messages, Exhausted: true}, nil
}

// Upload is unused by the sweep and reports it.
func (*orphanStorage) Upload(context.Context, telegramstore.UploadRequest) (telegramstore.StoredPart, error) {
	return telegramstore.StoredPart{}, errors.New("not used")
}

// OpenRange is unused by the sweep and reports it.
func (*orphanStorage) OpenRange(context.Context, telegramstore.RangeRequest) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

// DeleteMessages records the IDs the sweep removed.
func (s *orphanStorage) DeleteMessages(_ context.Context, _, _ int64, ids []int64) error {
	s.deleted = append(s.deleted, ids...)
	return nil
}

// CopyPart is unused by the sweep and reports it.
func (*orphanStorage) CopyPart(context.Context, int64, int64, int64, int64) (telegramstore.StoredPart, error) {
	return telegramstore.StoredPart{}, errors.New("not used")
}

// CreateChannel is unused by the sweep and reports it.
func (*orphanStorage) CreateChannel(context.Context, int64, string) (telegramstore.Channel, error) {
	return telegramstore.Channel{}, errors.New("not used")
}

// DeleteChannel is unused by the sweep and reports it, so a sweep that deleted a
// channel instead of messages would fail the test.
func (*orphanStorage) DeleteChannel(context.Context, int64, int64) error {
	return errors.New("not used")
}
