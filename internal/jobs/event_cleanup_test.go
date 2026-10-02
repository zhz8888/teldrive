package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/riverqueue/river"
)

func TestEventCleanupWorkerRejectsMissingPool(t *testing.T) {
	t.Parallel()
	worker := NewEventCleanupWorker(nil)
	err := worker.Work(context.Background(), &river.Job[EventCleanupArgs]{})
	if !errors.Is(err, ErrEventCleanupNotConfigured) {
		t.Fatalf("Work() error = %v, want ErrEventCleanupNotConfigured", err)
	}
}
