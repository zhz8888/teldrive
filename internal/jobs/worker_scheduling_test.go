package jobs

import (
	"testing"
	"time"
)

// TestSweepSchedulingPinsEveryMaintenanceWorker pins the queue, priority and attempt
// budget each maintenance sweep runs with. Priority is the whole ordering: the
// purge that releases Telegram messages runs before the cleanup sweeps, which run
// before the orphan scan, so a cheap housekeeping pass never delays a deletion the
// user is waiting on.
func TestSweepSchedulingPinsEveryMaintenanceWorker(t *testing.T) {
	t.Parallel()
	upload := (UploadCleanupSweepArgs{}).InsertOpts()
	if upload.Queue != CleanupQueue || upload.Priority != 2 || upload.MaxAttempts != 3 {
		t.Fatalf("upload cleanup scheduling = %+v, want queue %q priority 2 and 3 attempts", upload, CleanupQueue)
	}

	event := (EventCleanupArgs{}).InsertOpts()
	if event.Queue != CleanupQueue || event.Priority != 2 || event.MaxAttempts != 3 {
		t.Fatalf("event cleanup scheduling = %+v, want queue %q priority 2 and 3 attempts", event, CleanupQueue)
	}

	purge := (PurgeSweepArgs{}).InsertOpts()
	if purge.Queue != PurgeQueue || purge.Priority != 1 {
		t.Fatalf("purge scheduling = %+v, want queue %q and priority 1", purge, PurgeQueue)
	}

	orphan := (OrphanCleanupArgs{}).InsertOpts()
	if orphan.Queue != CleanupQueue || orphan.Priority != 3 {
		t.Fatalf("orphan cleanup scheduling = %+v, want queue %q and priority 3", orphan, CleanupQueue)
	}

	// The event sweep fans out over the event table, so it gets a longer budget than
	// a single-file operation.
	if got := (&EventCleanupWorker{}).Timeout(nil); got != 30*time.Minute {
		t.Fatalf("EventCleanupWorker.Timeout() = %s, want 30m", got)
	}
}

// TestKindNamesAreDistinct guards the worker registry: two workers answering the
// same kind would let one steal the other's jobs, and the loser would only fail at
// run time.
func TestKindNamesAreDistinct(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for _, worker := range []struct {
		name string
		kind string
	}{
		{name: "upload cleanup", kind: (UploadCleanupSweepArgs{}).Kind()},
		{name: "event cleanup", kind: (EventCleanupArgs{}).Kind()},
		{name: "purge", kind: (PurgeSweepArgs{}).Kind()},
		{name: "orphan cleanup", kind: (OrphanCleanupArgs{}).Kind()},
		{name: "bot provision", kind: (BotProvisionArgs{}).Kind()},
		{name: "upload batch", kind: (UploadBatchArgs{}).Kind()},
		{name: "upload source", kind: (UploadSourceArgs{}).Kind()},
	} {
		if previous, taken := seen[worker.kind]; taken {
			t.Fatalf("%s and %s share the kind %q", previous, worker.name, worker.kind)
		}
		seen[worker.kind] = worker.name
	}
}
