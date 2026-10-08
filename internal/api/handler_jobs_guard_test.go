package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/zhz8888/teldrive/v2/internal/api/gen"
	"github.com/zhz8888/teldrive/v2/internal/principal"
)

// assertUnavailable fails unless err is the 503 problem built from
// ErrOperationUnavailable, which is how a handler reports a dependency that was
// never wired.
func assertUnavailable(t *testing.T, err error) {
	t.Helper()

	if !errors.Is(err, ErrOperationUnavailable) {
		t.Fatalf("error = %v, want ErrOperationUnavailable", err)
	}
	var problem *Problem
	if !errors.As(err, &problem) {
		t.Fatalf("error = %T, want *Problem", err)
	}
	if problem.Status != http.StatusServiceUnavailable || problem.Code != "service_unavailable" {
		t.Fatalf("problem = (%d, %q), want (503, service_unavailable)", problem.Status, problem.Code)
	}
}

// TestJobHandlersUnavailableWithoutRuntime covers every job and periodic-job
// handler that must answer 503 instead of dereferencing a nil Jobs runtime.
func TestJobHandlersUnavailableWithoutRuntime(t *testing.T) {
	t.Parallel()

	handler := &Handler{}
	tests := []struct {
		name string
		call func(context.Context) error
	}{
		{name: "ListJobs", call: func(ctx context.Context) error {
			_, err := handler.ListJobs(ctx, gen.ListJobsParams{})
			return err
		}},
		{name: "CreateJob", call: func(ctx context.Context) error {
			_, err := handler.CreateJob(ctx, &gen.JobCreate{})
			return err
		}},
		{name: "CreateUploadImport", call: func(ctx context.Context) error {
			_, err := handler.CreateUploadImport(ctx, &gen.UploadImportRequest{})
			return err
		}},
		{name: "GetJobStatistics", call: func(ctx context.Context) error {
			_, err := handler.GetJobStatistics(ctx)
			return err
		}},
		{name: "ListJobQueues", call: func(ctx context.Context) error {
			_, err := handler.ListJobQueues(ctx)
			return err
		}},
		{name: "GetJob", call: func(ctx context.Context) error {
			_, err := handler.GetJob(ctx, gen.GetJobParams{JobId: "1"})
			return err
		}},
		{name: "CancelJob", call: func(ctx context.Context) error {
			_, err := handler.CancelJob(ctx, gen.CancelJobParams{JobId: "1"})
			return err
		}},
		{name: "RetryJob", call: func(ctx context.Context) error {
			_, err := handler.RetryJob(ctx, gen.RetryJobParams{JobId: "1"})
			return err
		}},
		{name: "DeleteJob", call: func(ctx context.Context) error {
			_, err := handler.DeleteJob(ctx, gen.DeleteJobParams{JobId: "1"})
			return err
		}},
		{name: "PurgeJobs", call: func(ctx context.Context) error {
			_, err := handler.PurgeJobs(ctx, gen.PurgeJobsParams{})
			return err
		}},
		{name: "PauseJobQueue", call: func(ctx context.Context) error {
			_, err := handler.PauseJobQueue(ctx, gen.PauseJobQueueParams{Queue: "default"})
			return err
		}},
		{name: "ResumeJobQueue", call: func(ctx context.Context) error {
			_, err := handler.ResumeJobQueue(ctx, gen.ResumeJobQueueParams{Queue: "default"})
			return err
		}},
		{name: "ListPeriodicJobs", call: func(ctx context.Context) error {
			_, err := handler.ListPeriodicJobs(ctx)
			return err
		}},
		{name: "GetPeriodicJobCatalog", call: func(ctx context.Context) error {
			_, err := handler.GetPeriodicJobCatalog(ctx)
			return err
		}},
		{name: "ResetPeriodicJobs", call: func(ctx context.Context) error {
			_, err := handler.ResetPeriodicJobs(ctx)
			return err
		}},
		{name: "CreatePeriodicJob", call: func(ctx context.Context) error {
			_, err := handler.CreatePeriodicJob(ctx, &gen.PeriodicJobCreate{})
			return err
		}},
		{name: "UpdatePeriodicJob", call: func(ctx context.Context) error {
			_, err := handler.UpdatePeriodicJob(ctx, &gen.PeriodicJobUpdate{}, gen.UpdatePeriodicJobParams{PeriodicJobId: "nightly"})
			return err
		}},
		{name: "DeletePeriodicJob", call: func(ctx context.Context) error {
			_, err := handler.DeletePeriodicJob(ctx, gen.DeletePeriodicJobParams{PeriodicJobId: "nightly"})
			return err
		}},
		{name: "PausePeriodicJob", call: func(ctx context.Context) error {
			_, err := handler.PausePeriodicJob(ctx, gen.PausePeriodicJobParams{PeriodicJobId: "nightly"})
			return err
		}},
		{name: "ResumePeriodicJob", call: func(ctx context.Context) error {
			_, err := handler.ResumePeriodicJob(ctx, gen.ResumePeriodicJobParams{PeriodicJobId: "nightly"})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assertUnavailable(t, test.call(context.Background()))
		})
	}
}

// TestSessionHandlersUnavailableWithoutAuthService checks that an unwired auth
// service is reported as 503 instead of a 404 pretending the session is gone.
func TestSessionHandlersUnavailableWithoutAuthService(t *testing.T) {
	t.Parallel()

	handler := &Handler{}
	ctx := principal.WithIdentity(context.Background(), principal.Identity{UserID: 1001, SessionID: uuid.New()})

	_, err := handler.ListSessions(ctx, gen.ListSessionsParams{})
	assertUnavailable(t, err)

	_, err = handler.RevokeSession(ctx, gen.RevokeSessionParams{SessionId: gen.UUID(uuid.New())})
	assertUnavailable(t, err)
}
