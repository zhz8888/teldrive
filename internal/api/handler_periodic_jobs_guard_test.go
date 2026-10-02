package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/tgdrive/teldrive/v2/internal/api/gen"
	"github.com/tgdrive/teldrive/v2/internal/jobs"
	"github.com/tgdrive/teldrive/v2/internal/principal"
)

// TestPeriodicJobHandlersRequireAdmin covers the authorization guard on every
// periodic-job endpoint. A schedule drives maintenance for the whole instance, so
// a caller without the admin or owner role must be refused before the runtime is
// touched; the zero Runtime would otherwise answer 503, which is what makes the
// 403 assertion prove the guard and not a missing dependency.
func TestPeriodicJobHandlersRequireAdmin(t *testing.T) {
	t.Parallel()

	handler := &Handler{Jobs: &jobs.Runtime{}}
	ctx := principal.WithIdentity(context.Background(), principal.Identity{UserID: 1001, Roles: []string{"user"}})
	tests := []struct {
		name string
		call func() error
	}{
		{name: "ListPeriodicJobs", call: func() error {
			_, err := handler.ListPeriodicJobs(ctx)
			return err
		}},
		{name: "GetPeriodicJobCatalog", call: func() error {
			_, err := handler.GetPeriodicJobCatalog(ctx)
			return err
		}},
		{name: "ResetPeriodicJobs", call: func() error {
			_, err := handler.ResetPeriodicJobs(ctx)
			return err
		}},
		{name: "CreatePeriodicJob", call: func() error {
			_, err := handler.CreatePeriodicJob(ctx, &gen.PeriodicJobCreate{})
			return err
		}},
		{name: "UpdatePeriodicJob", call: func() error {
			_, err := handler.UpdatePeriodicJob(ctx, &gen.PeriodicJobUpdate{}, gen.UpdatePeriodicJobParams{PeriodicJobId: "nightly"})
			return err
		}},
		{name: "DeletePeriodicJob", call: func() error {
			_, err := handler.DeletePeriodicJob(ctx, gen.DeletePeriodicJobParams{PeriodicJobId: "nightly"})
			return err
		}},
		{name: "PausePeriodicJob", call: func() error {
			_, err := handler.PausePeriodicJob(ctx, gen.PausePeriodicJobParams{PeriodicJobId: "nightly"})
			return err
		}},
		{name: "ResumePeriodicJob", call: func() error {
			_, err := handler.ResumePeriodicJob(ctx, gen.ResumePeriodicJobParams{PeriodicJobId: "nightly"})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var problem *Problem
			err := test.call()
			if !errors.As(err, &problem) {
				t.Fatalf("error = %v, want a *Problem", err)
			}
			if problem.Status != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", problem.Status, http.StatusForbidden)
			}
		})
	}
}
