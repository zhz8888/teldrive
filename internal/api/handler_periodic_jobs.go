package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/divyam234/riverpro"
	"github.com/go-faster/jx"
	"github.com/riverqueue/river"

	"github.com/zhz8888/teldrive/v2/internal/api/gen"
	"github.com/zhz8888/teldrive/v2/internal/jobs"
)

// ListPeriodicJobs returns every configured periodic job with its schedule, queue
// and pause state. The admin or owner role is required, because a schedule drives
// maintenance for every account on the instance. Service failures are mapped by
// mapServiceError.
func (h *Handler) ListPeriodicJobs(ctx context.Context) (gen.ListPeriodicJobsRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	items, err := h.Jobs.ListPeriodicJobs(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	response := gen.PeriodicJobList{Jobs: make([]gen.PeriodicJob, 0, len(items))}
	for _, item := range items {
		response.Jobs = append(response.Jobs, periodicJobResponse(item))
	}
	return &response, nil
}

// GetPeriodicJobCatalog returns the built-in schedule templates with their kind,
// label, default args, queue and recommended cron expression, which clients use to
// create periodic jobs. The admin or owner role is required, matching the endpoints
// that consume the catalog.
func (h *Handler) GetPeriodicJobCatalog(ctx context.Context) (gen.GetPeriodicJobCatalogRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	templates := h.Jobs.PeriodicJobCatalog()
	response := gen.PeriodicJobCatalog{Templates: make([]gen.PeriodicJobTemplate, 0, len(templates))}
	for _, template := range templates {
		response.Templates = append(response.Templates, gen.PeriodicJobTemplate{
			Kind: template.Kind, Label: template.Label, Description: template.Description,
			DefaultId: template.ID, DefaultArgs: rawMap[gen.PeriodicJobTemplateDefaultArgs](template.DefaultArgs),
			DefaultQueue: template.DefaultQueue, RecommendedCron: template.DefaultCronExpression,
		})
	}
	return &response, nil
}

// ResetPeriodicJobs restores the built-in catalog of schedules and returns the
// resulting list, discarding any user modifications. The admin or owner role is
// required: the reset re-enables and rewrites schedules for the whole instance.
func (h *Handler) ResetPeriodicJobs(ctx context.Context) (gen.ResetPeriodicJobsRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	items, err := h.Jobs.ResetPeriodicJobs(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	response := gen.PeriodicJobList{Jobs: make([]gen.PeriodicJob, 0, len(items))}
	for _, item := range items {
		response.Jobs = append(response.Jobs, periodicJobResponse(item))
	}
	return &response, nil
}

// CreatePeriodicJob registers a cron schedule and returns it. The admin or owner
// role is required: a schedule runs maintenance for every account, so it is not a
// per-user resource. A duplicate ID is reported as 409 through mapPeriodicJobError.
func (h *Handler) CreatePeriodicJob(ctx context.Context, req *gen.PeriodicJobCreate) (gen.CreatePeriodicJobRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	paused, _ := req.Paused.Get()
	item, err := h.Jobs.CreatePeriodicJob(ctx, jobs.PeriodicJobInput{
		ID: req.ID, Kind: req.Kind, Args: jsonRawMap(req.Args), Queue: req.Queue,
		Priority: int(req.Priority), MaxAttempts: int(req.MaxAttempts), Tags: append([]string(nil), req.Tags...),
		Schedule: jobs.PeriodicSchedule{CronExpression: req.CronExpression, CronTimezone: req.CronTimezone},
		Paused:   paused,
	})
	if err != nil {
		return nil, mapPeriodicJobError(err)
	}
	response := periodicJobResponse(item)
	return &response, nil
}

// UpdatePeriodicJob replaces the schedule identified by the path ID and returns
// the stored job; unknown IDs map to 404 through mapPeriodicJobError. The admin or
// owner role is required.
func (h *Handler) UpdatePeriodicJob(ctx context.Context, req *gen.PeriodicJobUpdate, params gen.UpdatePeriodicJobParams) (gen.UpdatePeriodicJobRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	paused, _ := req.Paused.Get()
	item, err := h.Jobs.UpdatePeriodicJob(ctx, params.PeriodicJobId, jobs.PeriodicJobInput{
		ID: params.PeriodicJobId, Kind: req.Kind, Args: jsonRawMap(req.Args), Queue: req.Queue,
		Priority: int(req.Priority), MaxAttempts: int(req.MaxAttempts), Tags: append([]string(nil), req.Tags...),
		Schedule: jobs.PeriodicSchedule{CronExpression: req.CronExpression, CronTimezone: req.CronTimezone},
		Paused:   paused,
	})
	if err != nil {
		return nil, mapPeriodicJobError(err)
	}
	response := periodicJobResponse(item)
	return &response, nil
}

// DeletePeriodicJob removes a schedule, returning 404 through mapPeriodicJobError
// when the ID does not exist. The admin or owner role is required.
func (h *Handler) DeletePeriodicJob(ctx context.Context, params gen.DeletePeriodicJobParams) (gen.DeletePeriodicJobRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := h.Jobs.DeletePeriodicJob(ctx, params.PeriodicJobId); err != nil {
		return nil, mapPeriodicJobError(err)
	}
	return &gen.DeletePeriodicJobNoContent{}, nil
}

// PausePeriodicJob suspends a schedule, which stops future runs but keeps its
// configuration, and returns the updated job. The admin or owner role is required.
func (h *Handler) PausePeriodicJob(ctx context.Context, params gen.PausePeriodicJobParams) (gen.PausePeriodicJobRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	item, err := h.Jobs.PausePeriodicJob(ctx, params.PeriodicJobId)
	if err != nil {
		return nil, mapPeriodicJobError(err)
	}
	response := periodicJobResponse(item)
	return &response, nil
}

// ResumePeriodicJob reactivates a paused schedule and returns the updated job.
// The admin or owner role is required.
func (h *Handler) ResumePeriodicJob(ctx context.Context, params gen.ResumePeriodicJobParams) (gen.ResumePeriodicJobRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	item, err := h.Jobs.ResumePeriodicJob(ctx, params.PeriodicJobId)
	if err != nil {
		return nil, mapPeriodicJobError(err)
	}
	response := periodicJobResponse(item)
	return &response, nil
}

// periodicJobResponse converts a runtime periodic job into the API model, copying
// the args, tags and schedule and attaching the pause timestamp when set.
func periodicJobResponse(item jobs.PeriodicJob) gen.PeriodicJob {
	response := gen.PeriodicJob{
		ID: item.ID, Kind: item.Kind, Args: rawMap[gen.PeriodicJobArgs](item.Args), Queue: item.Queue,
		Priority: int32(item.Priority), MaxAttempts: int32(item.MaxAttempts), Tags: append([]string(nil), item.Tags...),
		CronExpression: item.Schedule.CronExpression, CronTimezone: item.Schedule.CronTimezone,
		NextRunAt: item.NextRunAt, Paused: item.Paused, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
	if item.PausedAt != nil {
		response.PausedAt = gen.NewOptDateTime(*item.PausedAt)
	}
	return response
}

// jsonRawMap converts a jx.Raw map into JSON messages with copied buffers; it is
// the inverse of rawMap and keeps the runtime args independent of the request.
func jsonRawMap[T ~map[string]jx.Raw](input T) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(input))
	for key, value := range input {
		result[key] = json.RawMessage(append([]byte(nil), value...))
	}
	return result
}

// mapPeriodicJobError maps riverpro's duplicate-ID error to 409 and River's
// not-found error to 404, falling back to mapServiceError for everything else.
func mapPeriodicJobError(err error) error {
	if errors.Is(err, riverpro.ErrPeriodicJobAlreadyExists) {
		return problem(http.StatusConflict, "already_exists", "periodic job already exists", err)
	}
	if errors.Is(err, river.ErrNotFound) {
		return problem(http.StatusNotFound, "not_found", "periodic job was not found", err)
	}
	return mapServiceError(err)
}
