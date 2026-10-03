package jobs

import (
	"context"
	"errors"
	"testing"
)

// TestRuntimeManagementRejectsAnUnconfiguredRuntime keeps every management entry
// point behind the same guard: an operator action issued against a runtime with
// no client or no pool must report the missing configuration rather than panic or
// silently succeed.
func TestRuntimeManagementRejectsAnUnconfiguredRuntime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var missing *Runtime

	queues, err := missing.ListQueues(ctx)
	if !errors.Is(err, ErrRuntimeNotConfigured) || queues != nil {
		t.Fatalf("ListQueues() = %v, %v, want ErrRuntimeNotConfigured", queues, err)
	}
	userQueues, err := missing.ListQueuesForUser(ctx, 1001)
	if !errors.Is(err, ErrRuntimeNotConfigured) || userQueues != nil {
		t.Fatalf("ListQueuesForUser() = %v, %v, want ErrRuntimeNotConfigured", userQueues, err)
	}
	if err := missing.PauseQueue(ctx, CleanupQueue); !errors.Is(err, ErrRuntimeNotConfigured) {
		t.Fatalf("PauseQueue() error = %v, want ErrRuntimeNotConfigured", err)
	}
	if err := missing.ResumeQueue(ctx, CleanupQueue); !errors.Is(err, ErrRuntimeNotConfigured) {
		t.Fatalf("ResumeQueue() error = %v, want ErrRuntimeNotConfigured", err)
	}
	purged, err := missing.Purge(ctx, "completed")
	if !errors.Is(err, ErrRuntimeNotConfigured) || purged != 0 {
		t.Fatalf("Purge() = %d, %v, want ErrRuntimeNotConfigured", purged, err)
	}
	purgedForUser, err := missing.PurgeForUser(ctx, 1001, "completed")
	if !errors.Is(err, ErrRuntimeNotConfigured) || purgedForUser != 0 {
		t.Fatalf("PurgeForUser() = %d, %v, want ErrRuntimeNotConfigured", purgedForUser, err)
	}
	if err := missing.DeletePeriodicJob(ctx, "sweep"); !errors.Is(err, ErrRuntimeNotConfigured) {
		t.Fatalf("DeletePeriodicJob() error = %v, want ErrRuntimeNotConfigured", err)
	}
	if _, err := missing.PausePeriodicJob(ctx, "sweep"); !errors.Is(err, ErrRuntimeNotConfigured) {
		t.Fatalf("PausePeriodicJob() error = %v, want ErrRuntimeNotConfigured", err)
	}
	if _, err := missing.ResumePeriodicJob(ctx, "sweep"); !errors.Is(err, ErrRuntimeNotConfigured) {
		t.Fatalf("ResumePeriodicJob() error = %v, want ErrRuntimeNotConfigured", err)
	}
}
