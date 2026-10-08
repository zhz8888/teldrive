package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-faster/jx"
	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/zhz8888/teldrive/v2/internal/api/gen"
	"github.com/zhz8888/teldrive/v2/internal/jobs"
)

// ListJobs returns the job page visible to the caller: administrators see every
// user's jobs while ordinary users only see their own. A malformed cursor is
// reported as 422 rather than through mapServiceError.
func (h *Handler) ListJobs(ctx context.Context, params gen.ListJobsParams) (gen.ListJobsRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	userID, err := jobScopeUserID(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	cursor, _ := params.Cursor.Get()
	status, _ := params.Status.Get()
	kind, _ := params.Type.Get()
	queue, _ := params.Queue.Get()
	items, next, err := h.Jobs.List(ctx, jobs.ListInput{
		UserID: userID, Cursor: string(cursor), Limit: params.Limit.Or(100), State: string(status), Kind: kind, Queue: queue,
	})
	if err != nil {
		return nil, mapServiceError(err)
	}
	response := gen.JobPage{Tasks: make([]gen.Job, 0, len(items)), Meta: gen.JobPageMeta{}}
	for _, item := range items {
		response.Tasks = append(response.Tasks, jobResponse(item))
	}
	if next != "" {
		response.Meta.NextCursor = gen.NewOptCursor(gen.Cursor(next))
	}
	return &response, nil
}

// CreateJob enqueues an arbitrary job on behalf of an administrator. The admin or
// owner role is required, and an unknown kind or invalid args are rejected with
// 400.
func (h *Handler) CreateJob(ctx context.Context, req *gen.JobCreate) (gen.CreateJobRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if !HasAdminRole(ctx) {
		return nil, problem(http.StatusForbidden, "forbidden", "administrator access is required", nil)
	}
	queue, _ := req.Queue.Get()
	priority, _ := req.Priority.Get()
	maxAttempts, _ := req.MaxAttempts.Get()
	item, err := h.Jobs.Create(ctx, jobs.CreateInput{
		Kind: req.Type, Args: jsonRawMap(req.Args), Queue: queue,
		Priority: int(priority), MaxAttempts: int(maxAttempts), Tags: append([]string(nil), req.Tags...),
	})
	if err != nil {
		// The kind is checked by the runtime and reported as ErrInvalidJobKind
		// (422), a runtime without a client as ErrRuntimeNotConfigured (503), and
		// a failed insert as an internal error that is logged: folding all three
		// into a 400 hid a database failure from both the client and the log.
		return nil, mapServiceError(err)
	}
	response := jobResponse(item)
	return &response, nil
}

// CreateUploadImport queues a batch import from local paths or HTTP URLs after
// validating the destination (folder UUID or absolute path) and each source.
// Local sources additionally require the admin or owner role.
func (h *Handler) CreateUploadImport(ctx context.Context, req *gen.UploadImportRequest) (gen.CreateUploadImportRes, error) {
	if h.Jobs == nil || h.Catalog == nil || req == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if len(req.Sources) == 0 {
		return nil, problem(http.StatusUnprocessableEntity, "invalid_upload_import", "at least one upload source is required", nil)
	}
	args := jobs.UploadBatchArgs{
		UserID: userID, Destination: strings.TrimSpace(req.Destination), Exclude: append([]string(nil), req.Exclude...),
		PartConcurrency: int(req.PartConcurrency.Or(4)), ChunkSize: req.ChunkSize.Or(512 * 1024 * 1024), Encryption: req.Encryption.Or(false),
	}
	if args.Destination == "" {
		return nil, problem(http.StatusUnprocessableEntity, "invalid_upload_import", "destination must be a folder UUID or absolute drive path", nil)
	}
	if !strings.HasPrefix(args.Destination, "/") {
		if _, err := uuid.Parse(args.Destination); err != nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid_upload_import", "destination must be a folder UUID or absolute drive path", nil)
		}
	}
	if headers, ok := req.Headers.Get(); ok {
		args.Headers = cloneHeaders(headers)
	}
	if value, ok := req.MinSize.Get(); ok {
		args.MinSize = value
	}
	if value, ok := req.MaxSize.Get(); ok {
		args.MaxSize = value
	}
	args.Sources = make([]jobs.UploadSource, 0, len(req.Sources))
	for _, source := range req.Sources {
		item := jobs.UploadSource{Type: string(source.Type), Exclude: append([]string(nil), source.Exclude...)}
		if value, ok := source.Path.Get(); ok {
			item.Path = value
		}
		if value, ok := source.URL.Get(); ok {
			item.URL = value.String()
		}
		if value, ok := source.DestinationPath.Get(); ok {
			item.DestinationPath = value
		}
		if headers, ok := source.Headers.Get(); ok {
			item.Headers = cloneHeaders(headers)
		}
		if (item.Type == "local" && item.Path == "") || (item.Type == "http" && item.URL == "") {
			return nil, problem(http.StatusUnprocessableEntity, "invalid_upload_import", "source does not contain the required path or URL", nil)
		}
		if item.Type == "local" && !HasAdminRole(ctx) {
			return nil, problem(http.StatusForbidden, "forbidden", "local imports require administrator access", nil)
		}
		args.Sources = append(args.Sources, item)
	}
	item, err := h.Jobs.InsertUploadBatch(ctx, args)
	if err != nil {
		return nil, mapServiceError(err)
	}
	response := jobResponse(item)
	return &response, nil
}

// cloneHeaders copies a header map so the queued job args do not alias a map the
// caller may still mutate after the request returns.
func cloneHeaders[T ~map[string]string](values T) map[string]string {
	result := make(map[string]string, len(values))
	maps.Copy(result, values)
	return result
}

// GetJobStatistics returns job counters: cluster-wide for administrators and
// scoped to the caller's own jobs for everyone else.
func (h *Handler) GetJobStatistics(ctx context.Context) (gen.GetJobStatisticsRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	var (
		stats jobs.Statistics
		err   error
	)
	if HasAdminRole(ctx) {
		stats, err = h.Jobs.Statistics(ctx)
	} else {
		userID, userErr := UserIDFromContext(ctx)
		if userErr != nil {
			return nil, mapServiceError(userErr)
		}
		stats, err = h.Jobs.StatisticsForUser(ctx, userID)
	}
	if err != nil {
		return nil, mapServiceError(err)
	}
	return &gen.JobStatistics{
		Available: stats.Available, Cancelled: stats.Cancelled, Completed: stats.Completed,
		Discarded: stats.Discarded, Pending: stats.Pending, Retryable: stats.Retryable,
		Running: stats.Running, Scheduled: stats.Scheduled,
	}, nil
}

// GetJob returns one job after parsing its decimal ID, which must be positive or
// the request is answered with 404. Non-admins only see their own jobs.
func (h *Handler) GetJob(ctx context.Context, params gen.GetJobParams) (gen.GetJobRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	userID, err := jobScopeUserID(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	id, err := parseJobID(params.JobId)
	if err != nil {
		return nil, problem(http.StatusNotFound, "not_found", "job was not found", err)
	}
	var item jobs.Job
	if userID == 0 {
		item, err = h.Jobs.Get(ctx, id)
	} else {
		item, err = h.Jobs.GetForUser(ctx, id, userID)
	}
	if err != nil {
		return nil, mapJobError(err)
	}
	response := jobResponse(item)
	return &response, nil
}

// CancelJob cancels an active job and returns its updated record. The same scoping
// as GetJob applies, so non-admins can only cancel their own jobs.
func (h *Handler) CancelJob(ctx context.Context, params gen.CancelJobParams) (gen.CancelJobRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	userID, err := jobScopeUserID(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	id, err := parseJobID(params.JobId)
	if err != nil {
		return nil, problem(http.StatusNotFound, "not_found", "job was not found", err)
	}
	var item jobs.Job
	if userID == 0 {
		item, err = h.Jobs.Cancel(ctx, id)
	} else {
		item, err = h.Jobs.CancelForUser(ctx, id, userID)
	}
	if err != nil {
		return nil, mapJobError(err)
	}
	response := jobResponse(item)
	return &response, nil
}

// RetryJob schedules a finalized job for another attempt and returns the updated
// record. Non-admins can only retry their own jobs.
func (h *Handler) RetryJob(ctx context.Context, params gen.RetryJobParams) (gen.RetryJobRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	userID, err := jobScopeUserID(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	id, err := parseJobID(params.JobId)
	if err != nil {
		return nil, problem(http.StatusNotFound, "not_found", "job was not found", err)
	}
	var item jobs.Job
	if userID == 0 {
		item, err = h.Jobs.Retry(ctx, id)
	} else {
		item, err = h.Jobs.RetryForUser(ctx, id, userID)
	}
	if err != nil {
		return nil, mapJobError(err)
	}
	response := jobResponse(item)
	return &response, nil
}

// DeleteJob removes a finalized job and cancels an active one instead, since River
// refuses to delete running jobs. It returns 204 and reports an active job that
// cannot be cancelled as 409.
func (h *Handler) DeleteJob(ctx context.Context, params gen.DeleteJobParams) (gen.DeleteJobRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	userID, err := jobScopeUserID(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	id, err := parseJobID(params.JobId)
	if err != nil {
		return nil, problem(http.StatusNotFound, "not_found", "job was not found", err)
	}
	var item jobs.Job
	if userID == 0 {
		item, err = h.Jobs.Get(ctx, id)
	} else {
		item, err = h.Jobs.GetForUser(ctx, id, userID)
	}
	if err != nil {
		return nil, mapJobError(err)
	}
	if isActiveJobState(item.State) {
		if userID == 0 {
			_, err = h.Jobs.Cancel(ctx, id)
		} else {
			_, err = h.Jobs.CancelForUser(ctx, id, userID)
		}
	} else if userID == 0 {
		err = h.Jobs.Delete(ctx, id)
	} else {
		err = h.Jobs.DeleteForUser(ctx, id, userID)
	}
	if err != nil {
		return nil, mapJobError(err)
	}
	return &gen.DeleteJobNoContent{}, nil
}

// PurgeJobs bulk-deletes finalized jobs in one state and returns how many rows
// were removed. Non-final states are rejected with 409; administrators purge
// across all users.
func (h *Handler) PurgeJobs(ctx context.Context, params gen.PurgeJobsParams) (gen.PurgeJobsRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	userID, err := jobScopeUserID(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	var count int64
	if userID == 0 {
		count, err = h.Jobs.Purge(ctx, string(params.Status))
	} else {
		count, err = h.Jobs.PurgeForUser(ctx, userID, string(params.Status))
	}
	if err != nil {
		if errors.Is(err, jobs.ErrInvalidJobState) {
			return nil, problem(http.StatusConflict, "invalid_state", "only finalized jobs can be purged", err)
		}
		return nil, mapServiceError(err)
	}
	return &gen.JobPurgeResult{Count: count}, nil
}

// jobScopeUserID returns the user ID every job operation should be scoped to, or
// zero for administrators, which is the sentinel that switches each operation to
// its cluster-wide variant.
func jobScopeUserID(ctx context.Context) (int64, error) {
	if HasAdminRole(ctx) {
		return 0, nil
	}
	return UserIDFromContext(ctx)
}

// ListJobQueues returns the River queues with their state and counters: all queues
// for administrators, only the queues holding the caller's jobs for everyone else.
func (h *Handler) ListJobQueues(ctx context.Context) (gen.ListJobQueuesRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	var (
		queues []jobs.Queue
		err    error
	)
	if HasAdminRole(ctx) {
		queues, err = h.Jobs.ListQueues(ctx)
	} else {
		userID, userErr := UserIDFromContext(ctx)
		if userErr != nil {
			return nil, mapServiceError(userErr)
		}
		queues, err = h.Jobs.ListQueuesForUser(ctx, userID)
	}
	if err != nil {
		return nil, mapServiceError(err)
	}
	response := gen.JobQueueList{Queues: make([]gen.JobQueue, 0, len(queues))}
	for _, queue := range queues {
		response.Queues = append(response.Queues, gen.JobQueue{
			Name: queue.Name, Paused: queue.Paused, Available: queue.Available, Running: queue.Running,
			Retryable: queue.Retryable, Scheduled: queue.Scheduled,
		})
	}
	return &response, nil
}

// PauseJobQueue stops a queue from handing out new work and requires the admin or
// owner role; unknown queues map to 404 through mapJobError.
func (h *Handler) PauseJobQueue(ctx context.Context, params gen.PauseJobQueueParams) (gen.PauseJobQueueRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if !HasAdminRole(ctx) {
		return nil, problem(http.StatusForbidden, "forbidden", "administrator access is required", nil)
	}
	if err := h.Jobs.PauseQueue(ctx, params.Queue); err != nil {
		return nil, mapJobError(err)
	}
	return &gen.PauseJobQueueNoContent{}, nil
}

// ResumeJobQueue lets a paused queue hand out work again and requires the admin or
// owner role; unknown queues map to 404 through mapJobError.
func (h *Handler) ResumeJobQueue(ctx context.Context, params gen.ResumeJobQueueParams) (gen.ResumeJobQueueRes, error) {
	if h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	if !HasAdminRole(ctx) {
		return nil, problem(http.StatusForbidden, "forbidden", "administrator access is required", nil)
	}
	if err := h.Jobs.ResumeQueue(ctx, params.Queue); err != nil {
		return nil, mapJobError(err)
	}
	return &gen.ResumeJobQueueNoContent{}, nil
}

// parseJobID parses the decimal job ID from the path. A non-numeric or
// non-positive value becomes river.ErrNotFound so callers answer 404 without
// leaking which IDs exist.
func parseJobID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, river.ErrNotFound
	}
	return id, nil
}

// isActiveJobState reports whether a River state still occupies a worker, which
// decides if DeleteJob must cancel the job instead of deleting it.
func isActiveJobState(state string) bool {
	switch state {
	case "available", "pending", "retryable", "running", "scheduled":
		return true
	default:
		return false
	}
}

// mapJobError translates River-specific failures into HTTP problems: 404 for a
// missing job and 409 for a job that is still running. Everything else falls back
// to mapServiceError.
func mapJobError(err error) error {
	if errors.Is(err, river.ErrNotFound) {
		return problem(http.StatusNotFound, "not_found", "job was not found", err)
	}
	if errors.Is(err, rivertype.ErrJobRunning) {
		return problem(http.StatusConflict, "job_running", "running jobs cannot be deleted", err)
	}
	return mapServiceError(err)
}

// jobResponse converts a runtime job into the API model, redacting its args and
// copying tags and worker lists so the response does not alias mutable runtime
// state.
func jobResponse(item jobs.Job) gen.Job {
	response := gen.Job{
		ID: strconv.FormatInt(item.ID, 10), Status: gen.JobState(item.State), Type: item.Kind,
		Queue: item.Queue, Attempt: int32(item.Attempt), MaxAttempts: int32(item.MaxAttempts),
		Priority: int32(item.Priority), Tags: append([]string(nil), item.Tags...),
		Args:   rawMap[gen.JobArgs](redactJobArgs(item.Args)),
		Errors: make([]gen.JobAttemptError, 0, len(item.Errors)), AttemptedBy: append([]string(nil), item.AttemptedBy...),
		CreatedAt: item.CreatedAt, ScheduledAt: item.ScheduledAt,
	}
	for _, attemptError := range item.Errors {
		errorResponse := gen.JobAttemptError{Attempt: int32(attemptError.Attempt), At: attemptError.At, Error: attemptError.Error}
		if attemptError.Trace != "" {
			errorResponse.Trace = gen.NewOptString(attemptError.Trace)
		}
		response.Errors = append(response.Errors, errorResponse)
	}
	if item.AttemptedAt != nil {
		response.StartedAt = gen.NewOptDateTime(*item.AttemptedAt)
	}
	if item.FinalizedAt != nil {
		response.CompletedAt = gen.NewOptDateTime(*item.FinalizedAt)
	}
	if value := rawString(item.Metadata, "parentId", "parent_id"); value != "" {
		response.ParentId = gen.NewOptString(value)
	}
	if value := rawString(item.Metadata, "description"); value != "" {
		response.Description = gen.NewOptString(value)
	}
	if value := rawString(item.Metadata, "message"); value != "" {
		response.Message = gen.NewOptString(value)
	} else if item.LastError != "" {
		response.Message = gen.NewOptString(item.LastError)
	}
	if len(item.Output) > 0 {
		var values map[string]json.RawMessage
		if json.Unmarshal(item.Output, &values) == nil {
			response.Output = gen.NewOptJobOutput(rawMap[gen.JobOutput](values))
		}
	}
	return response
}

// redactJobArgs rewrites job args before they are exposed to clients. Malformed
// entries that cannot be decoded are copied through unchanged rather than dropped.
func redactJobArgs(input map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(input))
	for key, raw := range input {
		var value any
		if json.Unmarshal(raw, &value) != nil {
			result[key] = append(json.RawMessage(nil), raw...)
			continue
		}
		encoded, err := json.Marshal(redactJobValue(key, value))
		if err != nil {
			result[key] = append(json.RawMessage(nil), raw...)
			continue
		}
		result[key] = encoded
	}
	return result
}

// redactJobValue replaces sensitive values with a placeholder: headers are masked
// wholesale, keys containing password, secret, token, authorization, cookie or
// api_key are redacted, and user_id is preserved while nested values recurse.
func redactJobValue(key string, value any) any {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	if normalized == "headers" {
		if values, ok := value.(map[string]any); ok {
			redacted := make(map[string]any, len(values))
			for header := range values {
				redacted[header] = "[redacted]"
			}
			return redacted
		}
		return "[redacted]"
	}
	if normalized != "user_id" && (strings.Contains(normalized, "password") || strings.Contains(normalized, "secret") || strings.Contains(normalized, "token") || strings.Contains(normalized, "authorization") || strings.Contains(normalized, "cookie") || strings.Contains(normalized, "api_key")) {
		return "[redacted]"
	}
	switch typed := value.(type) {
	case map[string]any:
		for childKey, childValue := range typed {
			typed[childKey] = redactJobValue(childKey, childValue)
		}
	case []any:
		for index, childValue := range typed {
			typed[index] = redactJobValue("", childValue)
		}
	}
	return value
}

// rawMap copies JSON messages into a jx.Raw map with fresh backing arrays, so the
// generated response never aliases the source map.
func rawMap[T ~map[string]jx.Raw](input map[string]json.RawMessage) T {
	result := make(T, len(input))
	for key, value := range input {
		result[key] = jx.Raw(append([]byte(nil), value...))
	}
	return result
}

// rawString returns the value of the first metadata key that decodes as a JSON
// string, or the empty string when none does.
func rawString(values map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		var value string
		if raw, ok := values[key]; ok && json.Unmarshal(raw, &value) == nil {
			return value
		}
	}
	return ""
}
