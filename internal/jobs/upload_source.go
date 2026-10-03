package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"golang.org/x/sync/errgroup"

	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
	"github.com/tgdrive/teldrive/v2/internal/transfer"
	"github.com/tgdrive/teldrive/v2/internal/uploads"
)

const (
	// skipExcluded is returned by uploadFilter.skipReason when an exclusion glob
	// matches the file's destination path.
	skipExcluded = "excluded"
	// skipBelowMinSize is returned by uploadFilter.skipReason when the file is
	// smaller than the batch minimum size.
	skipBelowMinSize = "below_min_size"
	// skipAboveMaxSize is returned by uploadFilter.skipReason when the file is
	// larger than the batch maximum size.
	skipAboveMaxSize = "above_max_size"
)

// errInvalidUploadSource is the sentinel that every validation failure of an
// upload batch wraps, so callers can classify bad input with errors.Is instead of
// matching messages. It is wrapped by errors such as "invalid parent id",
// "chunk size must be between 64 MiB and 2000 MiB" and "unsupported source type".
var errInvalidUploadSource = errors.New("invalid upload source")

const (
	// UploadBatchKind is the River job kind of the job that expands an upload
	// import request into one job per file.
	UploadBatchKind = "teldrive_upload_batch"
	// UploadSourceKind is the River job kind of the job that streams one resolved
	// file into TelDrive storage.
	UploadSourceKind = "teldrive_upload_source"
	// UploadQueue is the River queue that carries both upload kinds. Uploads are
	// kept apart from the maintenance sweeps so a long cleanup cannot starve them.
	UploadQueue = "uploads"
	// uploadChunkBlock is the granularity part sizes are aligned to (16 MiB).
	uploadChunkBlock = int64(16 * 1024 * 1024)
	// minUploadChunk is the smallest accepted part size (64 MiB).
	minUploadChunk = int64(64 * 1024 * 1024)
	// maxUploadChunk is the largest accepted part size (2000 MiB), which keeps a
	// single part inside the limits of the storage backend.
	maxUploadChunk = int64(2000 * 1024 * 1024)
	// defaultUploadChunk is the part size used when a request does not ask for one
	// (512 MiB).
	defaultUploadChunk = int64(512 * 1024 * 1024)
)

// UploadSource describes one import source inside an upload batch request. Type
// selects which fields are meaningful: "local" sources use Path, "http" sources use
// URL. Local imports are restricted to administrators by the API layer and to the
// configured import roots by this package; remote URLs are validated again by the
// upload HTTP client, which refuses private and loopback addresses.
type UploadSource struct {
	// Type is the source kind, either "local" or "http".
	Type string `json:"type"`
	// Path is the absolute filesystem path of a local source. It must resolve
	// inside one of the configured import roots.
	Path string `json:"path,omitempty"`
	// URL is the http or https address of a remote source.
	URL string `json:"url,omitempty"`
	// Headers are extra request headers for a remote source, merged over the
	// batch-level headers with these values winning.
	Headers map[string]string `json:"headers,omitempty"`
	// DestinationPath overrides the file or directory name inside the batch
	// destination; it is a slash-separated relative path.
	DestinationPath string `json:"destination_path,omitempty"`
	// Exclude lists glob patterns that skip matching files of this source. The
	// local branch of expand matches them against each file's path relative to the
	// source, the HTTP branch against the resolved destination path; the
	// batch-level Exclude and the size bounds apply on top of them.
	Exclude []string `json:"exclude,omitempty"`
}

// UploadBatchArgs is the persisted payload of one upload batch job. It is stored
// as JSON in river_job, so the field names must stay stable while jobs queued by
// older releases may still be pending.
type UploadBatchArgs struct {
	// BatchID groups the per-file jobs of one submission and keys their
	// deduplication. The server generates it per accepted request, so a retried
	// insert of the same batch is idempotent, but two separate submissions of the
	// same sources are two batches: the API does not carry a client identifier
	// that could merge them.
	BatchID string `json:"batch_id"`
	// UserID is the TelDrive user that owns the imported files.
	UserID int64 `json:"user_id"`
	// Destination is the folder UUID or the absolute drive path to import into;
	// the worker resolves it to a folder ID before expanding the sources.
	Destination string `json:"destination,omitempty"`
	// ParentID is the pre-resolution spelling of Destination, retained so jobs
	// queued before destination paths were resolved still land in the same folder.
	ParentID string `json:"parent_id,omitempty"`
	// Sources are the sources to expand into per-file jobs.
	Sources []UploadSource `json:"sources"`
	// Headers are the default request headers applied to every remote source.
	Headers map[string]string `json:"headers,omitempty"`
	// Exclude lists glob patterns applied to every expanded file.
	Exclude []string `json:"exclude,omitempty"`
	// MinSize is the smallest file size to import, written as a human-readable
	// size such as "10MiB"; smaller files are skipped.
	MinSize string `json:"min_size,omitempty"`
	// MaxSize is the largest file size to import, written like MinSize; larger
	// files are skipped, and it must not be smaller than MinSize.
	MaxSize string `json:"max_size,omitempty"`
	// PartConcurrency is how many parts of one file may transfer at once. Zero
	// selects the default of 4, and values above 16 are rejected.
	PartConcurrency int `json:"part_concurrency,omitempty"`
	// ChunkSize is the requested part size in bytes. It is rounded to the nearest
	// 16 MiB and must stay between 64 MiB and 2000 MiB; zero selects the 512 MiB
	// default.
	ChunkSize int64 `json:"chunk_size,omitempty"`
	// Encryption stores the file with the active encryption key.
	Encryption bool `json:"encryption,omitempty"`
}

// Kind reports the River job kind handled by UploadBatchWorker.
func (UploadBatchArgs) Kind() string { return UploadBatchKind }

// InsertOpts runs a batch on the upload queue with three attempts. Batches are
// not deduplicated by arguments, so a failed batch may be submitted again; it is
// the per-file jobs that deduplicate by BatchID and SourceIndex.
func (UploadBatchArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: UploadQueue, MaxAttempts: 3}
}

// UploadFileSource is one concrete file that the batch worker resolved from an
// UploadSource and handed to a per-file job. It carries the size, modification time
// and version validator that the batch worker discovered, so the upload does not
// have to probe the source again; only an unknown MIME type can still cause a short
// read of the first bytes.
type UploadFileSource struct {
	// Type is the source kind, either "local" or "http".
	Type string `json:"type"`
	// Location is the absolute path of a local file or the URL of a remote one.
	Location string `json:"location"`
	// Headers are the headers to send when reading a remote source.
	Headers map[string]string `json:"headers,omitempty"`
	// DestinationPath is the validated, slash-separated path of the file inside
	// the destination folder.
	DestinationPath string `json:"destination_path"`
	// Size is the total file size in bytes; for a remote source it comes from the
	// probe request that the batch worker issued.
	Size int64 `json:"size"`
	// ModTime is the source modification time in UTC, meaningful only when
	// HasModTime is true.
	ModTime time.Time `json:"mod_time"`
	// HasModTime reports whether ModTime was known, since a zero ModTime would
	// otherwise be indistinguishable from an unknown one.
	HasModTime bool `json:"has_mod_time"`
	// MIMEType is the media type detected from the declaration, the first bytes or
	// the file extension, and is passed on to the stored file.
	MIMEType string `json:"mime_type,omitempty"`
	// Validator is the ETag or Last-Modified value sent as If-Range, so a resumed
	// upload only reuses parts while the remote file is unchanged.
	Validator string `json:"validator,omitempty"`
}

// UploadSourceArgs is the persisted payload of one per-file upload job. The jobs
// are unique by BatchID and SourceIndex, so resubmitting a batch while its jobs
// are still pending joins the existing ones instead of uploading the file twice.
type UploadSourceArgs struct {
	// BatchID is the batch this file belongs to and one half of the unique key.
	BatchID string `json:"batch_id" river:"unique"`
	// SourceIndex is the file's position in the expanded batch and the other half
	// of the unique key; it is stable for a given batch and source layout.
	SourceIndex int `json:"source_index" river:"unique"`
	// UserID is the TelDrive user that owns the imported file.
	UserID int64 `json:"user_id"`
	// ParentID is the destination folder UUID; empty means the drive root.
	ParentID string `json:"parent_id,omitempty"`
	// Source is the resolved file to read.
	Source UploadFileSource `json:"source"`
	// PartConcurrency is how many parts may transfer at once; zero selects 4 and
	// values above 16 are rejected.
	PartConcurrency int `json:"part_concurrency"`
	// ChunkSize is the part size in bytes after normalization.
	ChunkSize int64 `json:"chunk_size"`
	// Encryption stores the file with the active encryption key; the job fails
	// with transfer.ErrEncryptionKey when no key version is configured.
	Encryption bool `json:"encryption,omitempty"`
}

// Kind reports the River job kind handled by UploadSourceWorker.
func (UploadSourceArgs) Kind() string { return UploadSourceKind }

// InsertOpts runs the file upload on the upload queue with five attempts, so a
// flaky remote source gets several chances, and deduplicates jobs by BatchID and
// SourceIndex so that resubmitting a batch does not start a second upload of the
// same file while the first one is still pending.
func (UploadSourceArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: UploadQueue, MaxAttempts: 5, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// UploadBatchWorker expands one upload batch into a per-file job for every file
// its sources resolve to, creating the destination folders first.
type UploadBatchWorker struct {
	// WorkerDefaults supplies River's no-op defaults for the hooks this worker
	// does not override.
	river.WorkerDefaults[UploadBatchArgs]
	// httpClient probes remote sources; it is the SSRF-guarded client returned by
	// NewUploadHTTPClient unless the caller supplied its own.
	httpClient *http.Client
	// catalog resolves the destination and creates missing folders.
	catalog *catalog.Service
	// localImportRoots is the allowlist of directory trees that local sources may
	// be read from; an empty list disables local imports entirely.
	localImportRoots []string
}

// NewUploadBatchWorker returns a batch worker using httpClient and catalogService.
// A nil httpClient falls back to NewUploadHTTPClient. The optional localImportRoots
// argument is copied and becomes the allowlist for local sources; without it local
// sources are rejected. catalogService is only required when a destination has to
// be resolved.
func NewUploadBatchWorker(httpClient *http.Client, catalogService *catalog.Service, localImportRoots ...[]string) *UploadBatchWorker {
	if httpClient == nil {
		httpClient = NewUploadHTTPClient()
	}
	var roots []string
	if len(localImportRoots) > 0 {
		roots = append([]string(nil), localImportRoots[0]...)
	}
	return &UploadBatchWorker{httpClient: httpClient, catalog: catalogService, localImportRoots: roots}
}

// Timeout allows one hour: the batch only probes remote sources and inserts jobs,
// it never transfers file data.
func (w *UploadBatchWorker) Timeout(*river.Job[UploadBatchArgs]) time.Duration { return time.Hour }

// Work resolves the destination folder, expands every source and enqueues one
// UploadSourceArgs job per file through the River client of the running job, so the
// inserts commit together with the batch job.
//
// Sources are expanded one after another and all jobs are inserted in a single
// call, which keeps SourceIndex stable for a given batch and lets the unique index
// absorb a resubmission. Files rejected by the exclusion globs or the size bounds
// are simply not enqueued. It returns errInvalidUploadSource for a malformed
// payload, such as a missing user, no sources, an unparsable BatchID or a part
// concurrency above 16, and stops without enqueueing anything when a source cannot
// be inspected.
func (w *UploadBatchWorker) Work(ctx context.Context, job *river.Job[UploadBatchArgs]) error {
	if job.Args.UserID <= 0 || len(job.Args.Sources) == 0 {
		return errInvalidUploadSource
	}
	parentID, err := w.resolveDestination(ctx, job.Args)
	if err != nil {
		return err
	}
	client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil {
		return fmt.Errorf("get River client: %w", err)
	}
	filter, err := newUploadFilter(job.Args.Exclude, job.Args.MinSize, job.Args.MaxSize)
	if err != nil {
		return err
	}
	partConcurrency := job.Args.PartConcurrency
	if partConcurrency <= 0 {
		partConcurrency = 4
	}
	if partConcurrency > 16 {
		return fmt.Errorf("%w: part concurrency exceeds 16", errInvalidUploadSource)
	}
	chunkSize, err := normalizeUploadChunkSize(job.Args.ChunkSize)
	if err != nil {
		return err
	}
	batchID := job.Args.BatchID
	if _, err := uuid.Parse(batchID); err != nil {
		return fmt.Errorf("%w: invalid batch id", errInvalidUploadSource)
	}
	index := 0
	insertParams := make([]river.InsertManyParams, 0)
	for _, source := range job.Args.Sources {
		files, err := w.expand(ctx, source, job.Args.Headers, filter)
		if err != nil {
			return err
		}
		for _, file := range files {
			args := UploadSourceArgs{BatchID: batchID, SourceIndex: index, UserID: job.Args.UserID, ParentID: parentID, Source: file, PartConcurrency: partConcurrency, ChunkSize: chunkSize, Encryption: job.Args.Encryption}
			insertParams = append(insertParams, river.InsertManyParams{Args: args})
			index++
		}
	}
	if len(insertParams) > 0 {
		if _, err := client.InsertMany(ctx, insertParams); err != nil {
			return fmt.Errorf("insert upload source jobs: %w", err)
		}
	}
	return nil
}

// resolveDestination returns the folder ID that the per-file jobs use as their
// parent, creating the folders of a path destination on the way.
//
// An empty Destination falls back to the legacy ParentID field and returns an
// empty string for the drive root. A destination that parses as a UUID must name an
// active folder. Any other destination must be an absolute drive path, which
// catalog.EnsureFolderPath resolves, creating the missing folders and tolerating a
// concurrent creation that wins the race. A UUID that catalog.Get cannot resolve for
// the user, whether it is unknown or owned by somebody else, surfaces that lookup's
// ErrNotFound wrapped with a "resolve upload destination" context;
// errInvalidUploadSource reports a file that exists but is not an active folder, or a
// relative path. The root is reported as a nil ID.
func (w *UploadBatchWorker) resolveDestination(ctx context.Context, args UploadBatchArgs) (string, error) {
	// ParentID is retained for jobs queued before destination paths were resolved by the worker.
	if args.Destination == "" {
		if args.ParentID == "" {
			return "", nil
		}
		if _, err := uuid.Parse(args.ParentID); err != nil {
			return "", fmt.Errorf("%w: invalid parent id", errInvalidUploadSource)
		}
		return args.ParentID, nil
	}
	if w.catalog == nil {
		return "", errors.New("upload catalog is not configured")
	}
	destination := strings.TrimSpace(args.Destination)
	if id, err := uuid.Parse(destination); err == nil {
		file, err := w.catalog.Get(ctx, args.UserID, id)
		if err != nil {
			return "", fmt.Errorf("resolve upload destination: %w", err)
		}
		if file.Kind != sqlcgen.FileKindFolder || file.Status != sqlcgen.FileStatusActive {
			return "", fmt.Errorf("%w: destination is not an active folder", errInvalidUploadSource)
		}
		return id.String(), nil
	}
	if !strings.HasPrefix(destination, "/") {
		return "", fmt.Errorf("%w: destination path must be absolute", errInvalidUploadSource)
	}
	id, err := w.catalog.EnsureFolderPath(ctx, args.UserID, nil, destination)
	if err != nil {
		return "", fmt.Errorf("create upload destination: %w", err)
	}
	if id == nil {
		return "", nil
	}
	return id.String(), nil
}

// expand converts one batch source into the concrete files to enqueue.
//
// A "local" source is first checked against the import roots and then walked; an
// "http" source is probed with a HEAD request, falling back to a ranged GET, so its
// size, name and validator are known before any byte is transferred. Both branches
// apply the per-source Exclude patterns, the local one against each file's path
// relative to the source and the HTTP one against the resolved destination path,
// on top of the batch filter; files rejected by either are dropped silently. An
// unsupported source type fails with errInvalidUploadSource. The returned entries
// become the arguments of the per-file jobs; a source that expands to nothing
// yields no jobs.
func (w *UploadBatchWorker) expand(ctx context.Context, source UploadSource, defaults map[string]string, batchFilter uploadFilter) ([]UploadFileSource, error) {
	switch source.Type {
	case "local":
		if err := validateLocalImportSource(source.Path, w.localImportRoots); err != nil {
			return nil, err
		}
		return expandLocalSource(source, batchFilter)
	case "http":
		file, err := inspectHTTPSource(ctx, w.httpClient, source, defaults)
		if err != nil {
			return nil, err
		}
		sourceFilter, err := newUploadFilter(source.Exclude, "", "")
		if err != nil {
			return nil, err
		}
		if uploadSkipReason(batchFilter, sourceFilter, file.DestinationPath, file.Size) != "" {
			return nil, nil
		}
		return []UploadFileSource{file}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported source type %q", errInvalidUploadSource, source.Type)
	}
}

// UploadSourceWorker streams one resolved file into TelDrive storage as a series
// of parts. It resumes an upload that an earlier attempt left open when the stored
// parts still match the file, and it skips the transfer entirely when the
// destination already holds an identical file.
type UploadSourceWorker struct {
	// WorkerDefaults supplies River's no-op defaults for the hooks this worker
	// does not override.
	river.WorkerDefaults[UploadSourceArgs]
	// pool is checked for nil so a worker without a database fails with a sentinel
	// error instead of panicking inside a query.
	pool *pgxpool.Pool
	// queries resolves existing destination files and resumable sessions.
	queries *sqlcgen.Queries
	// catalog creates the intermediate folders of the destination path.
	catalog *catalog.Service
	// uploads creates the upload session and publishes the finished file.
	uploads *uploads.Service
	// pipeline stores the individual parts.
	pipeline *transfer.Pipeline
	// httpClient reads remote sources; it must be the SSRF-guarded client, since
	// this worker follows URLs supplied by the user.
	httpClient *http.Client
	// activeKeyVersion is the encryption key version used for encrypted uploads; a
	// non-positive value makes an encrypted job fail with transfer.ErrEncryptionKey
	// rather than storing plaintext under a promise of encryption.
	activeKeyVersion int32
}

// NewUploadSourceWorker returns a per-file upload worker. pool, catalogService,
// uploadService and pipeline are required: Work fails with
// ErrRuntimeNotConfigured when one of them is missing. A nil httpClient falls back
// to NewUploadHTTPClient, so remote reads are always SSRF-guarded.
func NewUploadSourceWorker(pool *pgxpool.Pool, catalogService *catalog.Service, uploadService *uploads.Service, pipeline *transfer.Pipeline, httpClient *http.Client, activeKeyVersion int32) *UploadSourceWorker {
	if httpClient == nil {
		httpClient = NewUploadHTTPClient()
	}
	return &UploadSourceWorker{pool: pool, queries: sqlcgen.New(pool), catalog: catalogService, uploads: uploadService, pipeline: pipeline, httpClient: httpClient, activeKeyVersion: activeKeyVersion}
}

// Timeout allows up to 24 hours for one file, because the transfer is bounded by
// the throughput of the remote source and of the storage backend rather than by
// this worker, and a resumed job reuses the parts that are already stored.
func (w *UploadSourceWorker) Timeout(*river.Job[UploadSourceArgs]) time.Duration {
	return 24 * time.Hour
}

// Work uploads one file: it resolves the destination folders and file name,
// creates or resumes an upload session, transfers the parts that are still missing
// with at most PartConcurrency requests in flight, and completes the session.
//
// The job is idempotent. When the destination already holds an active file with the
// same size, modification time and MIME type, the upload is skipped and recorded as
// "destination_matches". Otherwise a compatible open session is resumed and only
// the parts that are not stored yet are transferred. After the transfer a local
// source is stat'ed again, and a file that changed underneath the upload fails the
// job instead of storing a mixture of two versions.
//
// Progress is published to the River job row about once per second and after every
// finished part. A failure during the transfer records the "failed" stage and returns
// the error, so River retries the job with the parts that are already stored; a
// failure before the session exists simply returns the error.
func (w *UploadSourceWorker) Work(ctx context.Context, job *river.Job[UploadSourceArgs]) error {
	if w == nil || w.pool == nil || w.catalog == nil || w.uploads == nil || w.pipeline == nil || job.Args.UserID <= 0 {
		return ErrRuntimeNotConfigured
	}
	if job.Args.PartConcurrency <= 0 {
		job.Args.PartConcurrency = 4
	}
	if job.Args.PartConcurrency > 16 {
		return fmt.Errorf("%w: part concurrency exceeds 16", errInvalidUploadSource)
	}
	chunkSize, err := normalizeUploadChunkSize(job.Args.ChunkSize)
	if err != nil {
		return err
	}
	client, _ := river.ClientFromContextSafely[pgx.Tx](ctx)
	parentID, err := parseOptionalUUID(job.Args.ParentID)
	if err != nil {
		return err
	}
	directory, name := path.Split(path.Clean(strings.TrimPrefix(job.Args.Source.DestinationPath, "/")))
	if name == "" || name == "." {
		return errInvalidUploadSource
	}
	parentID, err = w.ensureFolders(ctx, job.Args.UserID, parentID, directory)
	if err != nil {
		return err
	}
	job.Args.Source.MIMEType, err = w.detectSourceMIME(ctx, job.Args.Source)
	if err != nil {
		return err
	}
	existing, err := w.queries.ResolveActiveChild(ctx, sqlcgen.ResolveActiveChildParams{UserID: job.Args.UserID, ParentID: dbtypes.OptionalUUID(parentID), Name: name})
	if err == nil {
		if existing.Kind != sqlcgen.FileKindFile {
			return uploads.ErrNameConflict
		}
		if existing.Size.Valid && existing.Size.Int64 == job.Args.Source.Size && job.Args.Source.HasModTime && modTimesEqual(existing.ModTime.Time, job.Args.Source.ModTime) && existing.MimeType.Valid && existing.MimeType.String == job.Args.Source.MIMEType {
			output := UploadSourceOutput{Path: job.Args.Source.DestinationPath, SourceType: job.Args.Source.Type, Stage: "skipped", Reason: "destination_matches", Progress: 100, TotalBytes: job.Args.Source.Size, UpdatedAt: time.Now().UTC()}
			if client != nil && job.JobRow != nil {
				return river.RecordOutput(ctx, output)
			}
			return nil
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("resolve upload destination: %w", err)
	}

	session, storedParts, err := w.findResumableUpload(ctx, job.Args.UserID, parentID, name, job.Args.Source, job.Args.Encryption)
	if err != nil {
		return err
	}
	if session == nil {
		input := uploads.CreateInput{UserID: job.Args.UserID, ParentID: parentID, Name: name, ExpectedSize: job.Args.Source.Size, MIMEType: optionalString(job.Args.Source.MIMEType), ModTime: job.Args.Source.ModTime, ConflictPolicy: sqlcgen.NameConflictPolicyReplace, Encryption: job.Args.Encryption, PartSize: chunkSize}
		if input.Encryption {
			if w.activeKeyVersion <= 0 {
				return transfer.ErrEncryptionKey
			}
			input.EncryptionKeyVersion = &w.activeKeyVersion
		}
		session, err = w.uploads.Create(ctx, input)
	}
	if err != nil {
		return err
	}
	if session.State == sqlcgen.UploadStateCompleted {
		// Defensive only: findResumableUpload returns open sessions and Create
		// inserts an open one, so a completed session has nothing left to transfer.
		return nil
	}
	uploadID, ok := dbtypes.GoogleUUID(session.ID)
	if !ok {
		return errInvalidUploadSource
	}
	partCount := int((session.ExpectedSize + session.PartSize - 1) / session.PartSize)
	var jobID int64
	if job.JobRow != nil {
		jobID = job.ID
	}
	tracker := newUploadProgressTracker(client, jobID, uploadID, job.Args.Source, session.PartSize, job.Args.PartConcurrency, partCount, storedParts)
	if err := tracker.update(ctx, "uploading", false); err != nil {
		return err
	}
	if partCount == 0 {
		file, completeErr := w.uploads.Complete(ctx, job.Args.UserID, uploadID)
		if completeErr != nil {
			_ = tracker.finish(ctx, "failed", nil)
			return completeErr
		}
		fileID, _ := dbtypes.GoogleUUID(file.ID)
		return tracker.finish(ctx, "completed", &fileID)
	}
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(job.Args.PartConcurrency)
	progressCtx, stopProgress := context.WithCancel(ctx)
	var progressWG sync.WaitGroup
	progressWG.Go(func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-progressCtx.Done():
				return
			case <-ticker.C:
				_ = tracker.update(progressCtx, "uploading", false)
			}
		}
	})
	for part := range partCount {
		if _, ok := storedParts[int32(part+1)]; ok {
			continue
		}
		group.Go(func() error {
			offset := int64(part) * session.PartSize
			size := min(session.PartSize, session.ExpectedSize-offset)
			reader, closeReader, err := w.openPart(groupCtx, job.Args.Source, offset, size)
			if err != nil {
				return err
			}
			defer closeReader()
			reader = io.TeeReader(reader, tracker)
			_, err = w.pipeline.UploadPart(groupCtx, transfer.UploadPartRequest{UserID: job.Args.UserID, UploadID: uploadID, PartNo: int32(part + 1), PlainSize: size, Body: reader})
			if err == nil {
				tracker.partCompleted(groupCtx, size)
			}
			return err
		})
	}
	groupErr := group.Wait()
	stopProgress()
	progressWG.Wait()
	if groupErr != nil {
		_ = tracker.finish(ctx, "failed", nil)
		return groupErr
	}
	if job.Args.Source.Type == "local" {
		info, statErr := os.Stat(job.Args.Source.Location)
		if statErr != nil || info.Size() != job.Args.Source.Size || !info.ModTime().Equal(job.Args.Source.ModTime) {
			_ = tracker.finish(ctx, "failed", nil)
			return fmt.Errorf("%w: local source changed during upload", errInvalidUploadSource)
		}
	}
	if err := tracker.update(ctx, "completing", false); err != nil {
		return err
	}
	file, err := w.uploads.Complete(ctx, job.Args.UserID, uploadID)
	if err != nil {
		_ = tracker.finish(ctx, "failed", nil)
		return err
	}
	fileID, _ := dbtypes.GoogleUUID(file.ID)
	return tracker.finish(ctx, "completed", &fileID)
}

// UploadSourceOutput is the River job output of one file upload. The tracker writes
// the last snapshot of a run here, so an operator can read progress, byte counts
// and the skip reason from the job row without access to the worker's logs. The
// JSON field names are part of the job API and must stay stable.
type UploadSourceOutput struct {
	// UploadID is the upload session UUID; empty when the file was skipped before a
	// session existed.
	UploadID string `json:"uploadId,omitempty"`
	// FileID is the UUID of the stored file, set once the upload completed.
	FileID string `json:"fileId,omitempty"`
	// Path is the destination path of the file inside the drive.
	Path string `json:"path"`
	// SourceType is the source kind, either "local" or "http".
	SourceType string `json:"sourceType"`
	// Stage is the last reported phase: "uploading", "completing", "completed",
	// "skipped" or "failed".
	Stage string `json:"stage"`
	// Reason explains a non-transfer outcome; the value set today is
	// "destination_matches", which marks a file whose identical copy already exists.
	Reason string `json:"reason,omitempty"`
	// Progress is the completion percentage from 0 to 100.
	Progress float64 `json:"progress"`
	// UploadedBytes counts the stored plus the in-flight bytes, capped at
	// TotalBytes, so it may be larger than StoredBytes while parts are in flight.
	UploadedBytes int64 `json:"uploadedBytes"`
	// StoredBytes counts the plaintext bytes of the parts that are durably stored.
	StoredBytes int64 `json:"storedBytes"`
	// TotalBytes is the expected size of the file in plaintext bytes.
	TotalBytes int64 `json:"totalBytes"`
	// SpeedBytesPerSecond is the average transfer rate since the job started, in
	// bytes per second, including bytes that had to be read again after a failure.
	SpeedBytesPerSecond int64 `json:"speedBytesPerSecond"`
	// CompletedParts is the number of parts that are durably stored.
	CompletedParts int32 `json:"completedParts"`
	// TotalParts is the number of parts the file is split into.
	TotalParts int `json:"totalParts"`
	// ChunkSize is the part size in bytes this upload uses.
	ChunkSize int64 `json:"chunkSize"`
	// PartConcurrency is how many parts may transfer at once.
	PartConcurrency int `json:"partConcurrency"`
	// StartedAt is the UTC time the job started tracking progress.
	StartedAt time.Time `json:"startedAt"`
	// UpdatedAt is the UTC time of the last progress update.
	UpdatedAt time.Time `json:"updatedAt"`
}

// uploadProgressTracker accumulates the counters of one upload and publishes them
// as River job output. The counters are atomics because the part readers write to
// them from several goroutines at once, while mu only serializes the snapshots.
type uploadProgressTracker struct {
	// client publishes the output; nil disables publishing entirely.
	client *river.Client[pgx.Tx]
	// jobID is the River job whose output is updated; zero disables the periodic
	// updates.
	jobID int64
	// output is the snapshot written to the job row, protected by mu.
	output UploadSourceOutput
	// started is the UTC time the tracker was created and the baseline for the
	// average speed.
	started time.Time
	// transferred counts bytes that were read from the source but do not belong to
	// a stored part yet; partCompleted moves them over to stored.
	transferred atomic.Int64
	// attempted counts every byte ever read from the source and is never
	// decremented, so the reported speed includes bytes that were read again after
	// a failure.
	attempted atomic.Int64
	// stored counts the plaintext bytes of the parts that are durably stored.
	stored atomic.Int64
	// completed counts the parts that are durably stored.
	completed atomic.Int32
	// mu serializes update so that two progress reports cannot interleave their
	// snapshot writes.
	mu sync.Mutex
}

// newUploadProgressTracker returns a tracker for one upload session. The stored map
// holds the part numbers and plain sizes that an earlier attempt left behind, so a
// resumed job starts with their bytes and count already accounted for. totalParts
// and chunkSize describe the target layout, and client may be nil when the job has
// no River row to update.
func newUploadProgressTracker(client *river.Client[pgx.Tx], jobID int64, uploadID uuid.UUID, source UploadFileSource, chunkSize int64, concurrency, totalParts int, stored map[int32]int64) *uploadProgressTracker {
	now := time.Now().UTC()
	tracker := &uploadProgressTracker{client: client, jobID: jobID, started: now, output: UploadSourceOutput{UploadID: uploadID.String(), Path: source.DestinationPath, SourceType: source.Type, TotalBytes: source.Size, TotalParts: totalParts, ChunkSize: chunkSize, PartConcurrency: concurrency, StartedAt: now}}
	for _, size := range stored {
		tracker.stored.Add(size)
		tracker.completed.Add(1)
	}
	return tracker
}

// Write implements io.Writer so that each part reader accounts for the bytes it
// pulls from the source. Every byte is added to the transferred and the attempted
// counters, and the call never fails: the returned error is always nil.
func (t *uploadProgressTracker) Write(p []byte) (int, error) {
	t.transferred.Add(int64(len(p)))
	t.attempted.Add(int64(len(p)))
	return len(p), nil
}

// partCompleted records that one part was durably stored: it moves the part's plain
// size from the transferred counter to the stored counter, counts the part and
// publishes a progress update at once. The update error is dropped on purpose,
// because progress reporting must never fail an upload that is otherwise fine.
func (t *uploadProgressTracker) partCompleted(ctx context.Context, size int64) {
	t.transferred.Add(-size)
	t.stored.Add(size)
	t.completed.Add(1)
	_ = t.update(ctx, "uploading", false)
}

// update recomputes the output snapshot from the counters and publishes it, as the
// terminal job output when terminal is set and as an in-progress update otherwise.
//
// UploadedBytes counts stored and in-flight bytes as disjoint and is capped at
// TotalBytes, so a part that had to be read twice cannot push progress above 100%.
// An update without a River client or job ID is skipped; the error of a failed
// update is returned, and the callers decide whether it matters, since the periodic
// refresh ignores it while the initial update fails the upload.
func (t *uploadProgressTracker) update(ctx context.Context, stage string, terminal bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now().UTC()
	stored, transferred := t.stored.Load(), t.transferred.Load()
	uploaded := min(t.output.TotalBytes, stored+transferred)
	t.output.Stage, t.output.StoredBytes, t.output.UploadedBytes = stage, stored, uploaded
	t.output.CompletedParts, t.output.UpdatedAt = t.completed.Load(), now
	if t.output.TotalBytes == 0 || stage == "completed" || stage == "skipped" {
		t.output.Progress = 100
	} else {
		t.output.Progress = float64(uploaded) * 100 / float64(t.output.TotalBytes)
	}
	if elapsed := now.Sub(t.started).Seconds(); elapsed > 0 {
		t.output.SpeedBytesPerSecond = int64(float64(t.attempted.Load()) / elapsed)
	}
	if terminal {
		if t.client == nil {
			return nil
		}
		return river.RecordOutput(ctx, t.output)
	}
	if t.client == nil || t.jobID <= 0 {
		return nil
	}
	_, err := t.client.JobUpdate(ctx, t.jobID, &river.JobUpdateParams{Output: t.output})
	return err
}

// normalizeUploadChunkSize validates and aligns a requested part size. A
// non-positive value selects the 512 MiB default; any other value must be between
// 64 MiB and 2000 MiB and is then rounded to the nearest 16 MiB block and capped at
// the maximum. Out-of-range values fail with errInvalidUploadSource. Rounding keeps
// part boundaries aligned with the 16 MiB storage block, which is what lets a part
// be stored as whole blocks.
func normalizeUploadChunkSize(value int64) (int64, error) {
	if value <= 0 {
		return defaultUploadChunk, nil
	}
	if value < minUploadChunk || value > maxUploadChunk {
		return 0, fmt.Errorf("%w: chunk size must be between 64 MiB and 2000 MiB", errInvalidUploadSource)
	}
	aligned := ((value + uploadChunkBlock/2) / uploadChunkBlock) * uploadChunkBlock
	return min(aligned, maxUploadChunk), nil
}

// findResumableUpload looks for an open upload session that an earlier attempt left
// behind and returns it together with the parts that can be reused.
//
// A session is only reusable when it matches the user, destination folder, file
// name, expected size, encryption flag, MIME type and, within one second, the
// modification time; it must still be unexpired and use the replace conflict
// policy, and the newest match wins. A session's parts are only accepted when every
// stored part sits exactly at the offset its number implies and covers its whole
// slice of the file, because a session whose layout or size changed cannot be
// resumed safely. When nothing qualifies it returns a nil session and an empty map,
// which makes the caller create a fresh upload.
func (w *UploadSourceWorker) findResumableUpload(ctx context.Context, userID int64, parentID *uuid.UUID, name string, source UploadFileSource, encryption bool) (*sqlcgen.UploadSession, map[int32]int64, error) {
	modTime := pgtype.Timestamptz{}
	if source.HasModTime {
		modTime = dbtypes.Time(source.ModTime)
	}
	sessions, err := w.queries.FindResumableUploadSessions(ctx, sqlcgen.FindResumableUploadSessionsParams{
		UserID: userID, ParentID: dbtypes.OptionalUUID(parentID), Name: name,
		ExpectedSize: source.Size, Encryption: encryption, MimeType: dbtypes.OptionalText(optionalString(source.MIMEType)),
		HasModTime: source.HasModTime, ModTime: modTime,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("find resumable uploads: %w", err)
	}
	if len(sessions) == 0 {
		return nil, map[int32]int64{}, nil
	}
	uploadIDs := make([]pgtype.UUID, 0, len(sessions))
	for _, session := range sessions {
		uploadIDs = append(uploadIDs, session.ID)
	}
	parts, err := w.queries.ListUploadPartsByUploadIDs(ctx, uploadIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("list resumable upload parts: %w", err)
	}
	partsByUpload := make(map[uuid.UUID][]*sqlcgen.UploadPart, len(sessions))
	for _, part := range parts {
		uploadID, ok := dbtypes.GoogleUUID(part.UploadID)
		if ok {
			partsByUpload[uploadID] = append(partsByUpload[uploadID], part)
		}
	}
	for _, session := range sessions {
		uploadID, ok := dbtypes.GoogleUUID(session.ID)
		if !ok {
			continue
		}
		stored, compatible := resumableStoredParts(partsByUpload[uploadID], session.ExpectedSize, session.PartSize)
		if compatible {
			return session, stored, nil
		}
	}
	return nil, map[int32]int64{}, nil
}

// resumableStoredParts maps part number to plain size for the parts of a session
// that are already stored. It reports false as soon as one stored part does not
// match the layout implied by totalSize and partSize, which tells the caller that
// the session must not be resumed and a new one has to be created instead.
func resumableStoredParts(parts []*sqlcgen.UploadPart, totalSize, partSize int64) (map[int32]int64, bool) {
	stored := make(map[int32]int64)
	for _, part := range parts {
		if part.State != sqlcgen.UploadPartStateStored {
			continue
		}
		offset := int64(part.PartNo-1) * partSize
		if offset < 0 || offset >= totalSize || part.PlainSize != min(partSize, totalSize-offset) {
			return nil, false
		}
		stored[part.PartNo] = part.PlainSize
	}
	return stored, true
}

// finish publishes the terminal job output. For a completed upload it also forces
// the counters to the final layout, so the recorded output shows 100% even when the
// last periodic update had not caught up with the transfer. A nil fileID leaves the
// FileID field empty, which is what skipped and failed uploads want.
func (t *uploadProgressTracker) finish(ctx context.Context, stage string, fileID *uuid.UUID) error {
	if fileID != nil {
		t.output.FileID = fileID.String()
	}
	if stage == "completed" {
		t.stored.Store(t.output.TotalBytes)
		t.transferred.Store(0)
		t.completed.Store(int32(t.output.TotalParts))
	}
	return t.update(ctx, stage, true)
}

// ensureFolders walks the slash-separated directory part of a destination path and
// returns the UUID of its deepest folder, creating the folders that do not exist
// yet. A nil parentID starts at the drive root, and an empty directory returns
// parentID unchanged. The lookup and the create are separate statements, so a
// concurrent upload of the same path can win the race; the resulting
// catalog.ErrConflict is resolved by re-reading the folder. It reports
// errInvalidUploadSource when a resolved folder ID cannot be converted.
func (w *UploadSourceWorker) ensureFolders(ctx context.Context, userID int64, parentID *uuid.UUID, directory string) (*uuid.UUID, error) {
	for name := range strings.SplitSeq(strings.Trim(directory, "/"), "/") {
		if name == "" || name == "." {
			continue
		}
		id, err := w.queries.ResolveActiveChildFolder(ctx, sqlcgen.ResolveActiveChildFolderParams{UserID: userID, ParentID: dbtypes.OptionalUUID(parentID), Name: name})
		if errors.Is(err, pgx.ErrNoRows) {
			folder, createErr := w.catalog.CreateFolder(ctx, catalog.CreateFolderInput{UserID: userID, ParentID: parentID, Name: name})
			if errors.Is(createErr, catalog.ErrConflict) {
				id, err = w.queries.ResolveActiveChildFolder(ctx, sqlcgen.ResolveActiveChildFolderParams{UserID: userID, ParentID: dbtypes.OptionalUUID(parentID), Name: name})
			} else if createErr != nil {
				return nil, createErr
			} else {
				id = folder.ID
				err = nil
			}
		}
		if err != nil {
			return nil, fmt.Errorf("resolve destination folder: %w", err)
		}
		value, ok := dbtypes.GoogleUUID(id)
		if !ok {
			return nil, errInvalidUploadSource
		}
		parentID = &value
	}
	return parentID, nil
}

// detectSourceMIME returns the media type to store for a file. A concrete declared
// type wins; otherwise the first 512 bytes are sniffed, which costs one extra ranged
// request for a remote source, and when sniffing stays inconclusive the extension of
// the destination path decides. It finally falls back to
// "application/octet-stream" rather than guessing from the URL. A declared type that
// does not parse is returned as it was given.
func (w *UploadSourceWorker) detectSourceMIME(ctx context.Context, source UploadFileSource) (string, error) {
	declared := strings.TrimSpace(source.MIMEType)
	if parsed, _, err := mime.ParseMediaType(declared); err == nil {
		declared = parsed
	}
	if declared != "" && declared != "application/octet-stream" {
		return declared, nil
	}

	var reader io.ReadCloser
	switch source.Type {
	case "local":
		file, err := os.Open(source.Location)
		if err != nil {
			return "", fmt.Errorf("open source for MIME detection: %w", err)
		}
		reader = file
	case "http":
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, source.Location, nil)
		if err != nil {
			return "", fmt.Errorf("create MIME detection request: %w", err)
		}
		applyUploadHeaders(request.Header, source.Headers)
		request.Header.Set("Range", "bytes=0-511")
		response, err := w.httpClient.Do(request)
		if err != nil {
			return "", errors.New("read HTTP source for MIME detection failed")
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			_ = response.Body.Close()
			return "", fmt.Errorf("read HTTP source for MIME detection: %s", response.Status)
		}
		reader = response.Body
	default:
		return "", fmt.Errorf("%w: unsupported source type %q", errInvalidUploadSource, source.Type)
	}
	defer reader.Close()

	buffer := make([]byte, 512)
	read, err := io.ReadFull(reader, buffer)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("read source for MIME detection: %w", err)
	}
	if read > 0 {
		detected := http.DetectContentType(buffer[:read])
		if detected != "application/octet-stream" {
			if parsed, _, err := mime.ParseMediaType(detected); err == nil {
				return parsed, nil
			}
			return detected, nil
		}
	}
	if inferred := mime.TypeByExtension(path.Ext(source.DestinationPath)); inferred != "" {
		if parsed, _, err := mime.ParseMediaType(inferred); err == nil {
			return parsed, nil
		}
		return inferred, nil
	}
	return "application/octet-stream", nil
}

// openPart returns a reader for exactly one part of a source plus the function that
// releases it; the caller must always call that function, including on error paths
// where it is a no-op.
//
// A local file is read through a SectionReader, so no copy is made and several parts
// of the same file can be read concurrently. A remote source is requested with a
// Range header and the stored If-Range validator: the response must either be a 206
// whose Content-Range covers exactly this part, or a 200 when the whole file was
// requested at once, which is how a server that ignores ranges is still supported.
// Any other status fails the part, so a server that returns a different range can
// never silently shift the bytes of a part.
func (w *UploadSourceWorker) openPart(ctx context.Context, source UploadFileSource, offset, size int64) (io.Reader, func(), error) {
	if source.Type == "local" {
		file, err := os.Open(source.Location)
		if err != nil {
			return nil, func() {}, fmt.Errorf("open local upload source: %w", err)
		}
		return io.NewSectionReader(file, offset, size), func() { _ = file.Close() }, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source.Location, nil)
	if err != nil {
		return nil, func() {}, err
	}
	applyUploadHeaders(request.Header, source.Headers)
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+size-1))
	if source.Validator != "" {
		request.Header.Set("If-Range", source.Validator)
	}
	response, err := w.httpClient.Do(request)
	if err != nil {
		return nil, func() {}, errors.New("open HTTP upload part request failed")
	}
	if response.StatusCode != http.StatusPartialContent && !(offset == 0 && size == source.Size && response.StatusCode == http.StatusOK) {
		_ = response.Body.Close()
		return nil, func() {}, fmt.Errorf("HTTP upload source does not support byte ranges: %s", response.Status)
	}
	if response.StatusCode == http.StatusPartialContent && !validContentRange(response.Header.Get("Content-Range"), offset, size, source.Size) {
		_ = response.Body.Close()
		return nil, func() {}, fmt.Errorf("HTTP upload source returned an invalid content range")
	}
	return response.Body, func() { _ = response.Body.Close() }, nil
}

// uploadFilter decides which expanded files are imported. It holds the compiled
// exclusion globs and the optional size bounds in bytes; a nil bound means the batch
// did not restrict that side.
type uploadFilter struct {
	// exclude holds the compiled exclusion globs, matched against the
	// slash-separated path of a file relative to its source.
	exclude []*regexp.Regexp
	// minSize is the smallest accepted size in bytes, or nil for no lower bound.
	minSize *int64
	// maxSize is the largest accepted size in bytes, or nil for no upper bound.
	maxSize *int64
}

// newUploadFilter compiles the exclusion patterns and parses the optional minimum
// and maximum sizes. A blank size string leaves that side unbounded; a pattern that
// cannot be compiled, an unparsable size and a minimum above the maximum all fail
// with errInvalidUploadSource, so a malformed request is rejected before any source
// is read.
func newUploadFilter(patterns []string, minSize, maxSize string) (uploadFilter, error) {
	filter := uploadFilter{}
	for _, pattern := range patterns {
		compiled, err := compileUploadGlob(pattern)
		if err != nil {
			return uploadFilter{}, err
		}
		filter.exclude = append(filter.exclude, compiled)
	}
	for _, item := range []struct {
		value  string
		target **int64
	}{{minSize, &filter.minSize}, {maxSize, &filter.maxSize}} {
		value, target := item.value, item.target
		if strings.TrimSpace(value) == "" {
			continue
		}
		size, err := parseUploadSize(value)
		if err != nil {
			return uploadFilter{}, err
		}
		*target = &size
	}
	if filter.minSize != nil && filter.maxSize != nil && *filter.minSize > *filter.maxSize {
		return uploadFilter{}, fmt.Errorf("%w: minimum size exceeds maximum size", errInvalidUploadSource)
	}
	return filter, nil
}

// skipReason returns why a file is not imported, or an empty string when it is. The
// exclusions are tested first, against the path normalized to slash separators with
// any leading "./" removed; the size bounds are inclusive, so a file exactly at a
// bound is still imported. The result is one of skipExcluded, skipBelowMinSize or
// skipAboveMaxSize.
func (f uploadFilter) skipReason(name string, size int64) string {
	normalized := strings.TrimPrefix(path.Clean(strings.ReplaceAll(name, "\\", "/")), "./")
	for _, pattern := range f.exclude {
		if pattern.MatchString(normalized) {
			return skipExcluded
		}
	}
	if f.minSize != nil && size < *f.minSize {
		return skipBelowMinSize
	}
	if f.maxSize != nil && size > *f.maxSize {
		return skipAboveMaxSize
	}
	return ""
}

// parseUploadSize parses a human-readable size such as "10MiB", "1.5 GB" or "500"
// into bytes. A unit is decimal by default and binary when it carries an "i", so
// "KB" is 1000 bytes and "KiB" is 1024, and a missing unit means plain bytes.
// Fractional values are accepted only when they work out to a whole number of bytes,
// and anything unparsable, negative or beyond MaxInt64 fails with
// errInvalidUploadSource.
func parseUploadSize(value string) (int64, error) {
	normalized := strings.TrimSpace(value)
	match := regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?)\s*([kmgtpe]?i?b?)?$`).FindStringSubmatch(normalized)
	if match == nil {
		return 0, fmt.Errorf("%w: invalid size %q", errInvalidUploadSource, value)
	}
	number, err := strconv.ParseFloat(match[1], 64)
	if err != nil || number < 0 {
		return 0, fmt.Errorf("%w: invalid size %q", errInvalidUploadSource, value)
	}
	unit := strings.ToUpper(match[2])
	unit = strings.TrimSuffix(unit, "B")
	base := float64(1000)
	if strings.HasSuffix(unit, "I") {
		base = 1024
		unit = strings.TrimSuffix(unit, "I")
	}
	power := strings.Index(" KMGTPE", unit)
	if power < 0 {
		return 0, fmt.Errorf("%w: invalid size %q", errInvalidUploadSource, value)
	}
	result := number * math.Pow(base, float64(power))
	if result > math.MaxInt64 || math.Trunc(result) != result {
		return 0, fmt.Errorf("%w: invalid size %q", errInvalidUploadSource, value)
	}
	return int64(result), nil
}

// compileUploadGlob turns one exclusion glob into an anchored regular expression
// that is matched against slash-separated paths relative to the source. "**" crosses
// directory separators, a single "*" and "?" stay inside one path segment, and the
// leading "(?:.*/)?" makes a pattern match at any depth, so "*.tmp" excludes the file
// wherever it appears. All other characters are quoted literally. An empty pattern or
// one that cleans away to "." fails with errInvalidUploadSource.
func compileUploadGlob(pattern string) (*regexp.Regexp, error) {
	pattern = strings.TrimPrefix(path.Clean(strings.ReplaceAll(strings.TrimSpace(pattern), "\\", "/")), "./")
	if pattern == "" || pattern == "." {
		return nil, fmt.Errorf("%w: empty exclusion", errInvalidUploadSource)
	}
	var expression strings.Builder
	expression.WriteString("^(?:.*/)?")
	for index := 0; index < len(pattern); index++ {
		switch pattern[index] {
		case '*':
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				if index+2 < len(pattern) && pattern[index+2] == '/' {
					expression.WriteString("(?:.*/)?")
					index += 2
				} else {
					expression.WriteString(".*")
					index++
				}
			} else {
				expression.WriteString("[^/]*")
			}
		case '?':
			expression.WriteString("[^/]")
		default:
			expression.WriteString(regexp.QuoteMeta(string(pattern[index])))
		}
	}
	expression.WriteString("$")
	compiled, err := regexp.Compile(expression.String())
	if err != nil {
		return nil, fmt.Errorf("%w: exclusion %q: %v", errInvalidUploadSource, pattern, err)
	}
	return compiled, nil
}

// validateLocalImportSource reports whether a local path may be read. The path must
// be absolute and local imports must be enabled, which is the case only when roots
// is not empty. The path and every configured root are resolved through their
// symlinks before they are compared, so a link placed inside an allowed root cannot
// be used to read a file outside of it; roots that do not resolve are ignored. A
// path outside of every root fails with errInvalidUploadSource.
func validateLocalImportSource(location string, roots []string) error {
	location = filepath.Clean(strings.TrimSpace(location))
	if !filepath.IsAbs(location) {
		return fmt.Errorf("%w: local path must be absolute", errInvalidUploadSource)
	}
	if len(roots) == 0 {
		return fmt.Errorf("%w: local imports are disabled", errInvalidUploadSource)
	}
	resolvedLocation, err := filepath.EvalSymlinks(location)
	if err != nil {
		return fmt.Errorf("inspect local upload source: %w", err)
	}
	for _, configuredRoot := range roots {
		root := filepath.Clean(strings.TrimSpace(configuredRoot))
		if !filepath.IsAbs(root) {
			continue
		}
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(resolvedRoot, resolvedLocation)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("%w: local path is outside configured import roots", errInvalidUploadSource)
}

// expandLocalSource turns a local file or directory into the list of files to
// import. It assumes the path was already checked against the import roots.
//
// The top-level path must exist and must not be a symlink; symlinks found during a
// walk are skipped and symlinked directories are not descended into, so a source
// cannot escape the tree it names. Excluded directories are pruned before their
// contents are listed, which keeps a large excluded subtree cheap, and only regular
// files are returned. Each file gets a destination built from the base destination
// and its path relative to the source, plus its size and modification time, so a
// later upload can tell that the file changed. Files rejected by the filters are
// dropped without an error.
func expandLocalSource(source UploadSource, batchFilter uploadFilter) ([]UploadFileSource, error) {
	location := filepath.Clean(strings.TrimSpace(source.Path))
	if !filepath.IsAbs(location) {
		return nil, fmt.Errorf("%w: local path must be absolute", errInvalidUploadSource)
	}
	info, err := os.Lstat(location)
	if err != nil {
		return nil, fmt.Errorf("inspect local upload source: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: symlink sources are not supported", errInvalidUploadSource)
	}
	filter, err := newUploadFilter(source.Exclude, "", "")
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: local source is not a regular file", errInvalidUploadSource)
		}
		name := source.DestinationPath
		if name == "" {
			name = info.Name()
		}
		destination, err := validateDestinationPath(name)
		if err != nil {
			return nil, err
		}
		if uploadSkipReason(batchFilter, filter, info.Name(), info.Size()) != "" {
			return nil, nil
		}
		return []UploadFileSource{{Type: "local", Location: location, DestinationPath: destination, Size: info.Size(), ModTime: info.ModTime().UTC(), HasModTime: true, MIMEType: mime.TypeByExtension(filepath.Ext(info.Name()))}}, nil
	}
	baseDestination, err := validateDestinationPath(source.DestinationPath)
	if err != nil {
		return nil, err
	}
	files := make([]UploadFileSource, 0)
	err = filepath.WalkDir(location, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(location, filePath)
		if err != nil || relative == "." {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			// WalkDir does not follow symlinks, so a symlinked directory is only
			// reported as this entry and skipped here without being descended into.
			return nil
		}
		if entry.IsDir() {
			if filter.skipReason(relative, 0) == skipExcluded || batchFilter.skipReason(relative, 0) == skipExcluded {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || uploadSkipReason(batchFilter, filter, relative, info.Size()) != "" {
			return nil
		}
		files = append(files, UploadFileSource{Type: "local", Location: filePath, DestinationPath: cleanDestinationPath(path.Join(baseDestination, relative)), Size: info.Size(), ModTime: info.ModTime().UTC(), HasModTime: true, MIMEType: mime.TypeByExtension(filepath.Ext(info.Name()))})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk local upload source: %w", err)
	}
	return files, nil
}

// inspectHTTPSource probes a remote source and returns the resolved file entry that
// the per-file job will read.
//
// The URL must use http or https and carry a host. The probe is a HEAD request; when
// the server rejects HEAD with 405 or 501 it retries with a one-byte ranged GET. The
// size comes from the total of a Content-Range that parses, and from Content-Length
// otherwise, so an unparsable or absent total never downgrades a length that was
// already known. A partial response whose total is missing, as in the "bytes 0-0/*"
// a server may send for a ranged request, has no usable size at all: its
// Content-Length measures only the returned range, so the source is rejected as
// errInvalidUploadSource instead of being uploaded truncated. The file name comes
// from the destination path, then from Content-Disposition, then from the last URL
// segment. The ETag, or otherwise Last-Modified, becomes the If-Range validator that
// stops a resumed upload from splicing two versions of the file together. The merged
// headers are stored on the entry so the part requests reuse them.
func inspectHTTPSource(ctx context.Context, client *http.Client, source UploadSource, defaults map[string]string) (UploadFileSource, error) {
	if client == nil {
		client = http.DefaultClient
	}
	parsed, err := url.Parse(strings.TrimSpace(source.URL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return UploadFileSource{}, fmt.Errorf("%w: invalid HTTP URL", errInvalidUploadSource)
	}
	headers := mergeUploadHeaders(defaults, source.Headers)
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, parsed.String(), nil)
	if err != nil {
		return UploadFileSource{}, err
	}
	applyUploadHeaders(request.Header, headers)
	response, err := client.Do(request)
	if err != nil {
		return UploadFileSource{}, errors.New("inspect HTTP upload source request failed")
	}
	_ = response.Body.Close()
	if response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusNotImplemented {
		request, err = http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
		if err != nil {
			return UploadFileSource{}, err
		}
		applyUploadHeaders(request.Header, headers)
		request.Header.Set("Range", "bytes=0-0")
		response, err = client.Do(request)
		if err != nil {
			return UploadFileSource{}, errors.New("inspect HTTP upload source request failed")
		}
		_ = response.Body.Close()
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return UploadFileSource{}, fmt.Errorf("inspect HTTP upload source: %s", response.Status)
	}
	size := response.ContentLength
	if contentRange := response.Header.Get("Content-Range"); contentRange != "" {
		if slash := strings.LastIndex(contentRange, "/"); slash >= 0 {
			total, parseErr := strconv.ParseInt(strings.TrimSpace(contentRange[slash+1:]), 10, 64)
			switch {
			case parseErr == nil:
				size = total
			case response.StatusCode == http.StatusPartialContent:
				// A partial response covers one range only, so its Content-Length
				// measures that range and the missing ("bytes 0-0/*") or unparsable
				// total leaves the file size unknown.
				size = -1
			}
			// For a full response the Content-Length read above is the whole entity
			// length, so it is kept instead of being replaced by zero.
		}
	}
	if size < 0 {
		return UploadFileSource{}, fmt.Errorf("%w: HTTP source has no content length", errInvalidUploadSource)
	}
	name := source.DestinationPath
	if name == "" {
		if _, params, parseErr := mime.ParseMediaType(response.Header.Get("Content-Disposition")); parseErr == nil {
			name = params["filename"]
		}
	}
	if name == "" {
		name = path.Base(parsed.Path)
	}
	if name == "" || name == "." || name == "/" {
		return UploadFileSource{}, fmt.Errorf("%w: HTTP destination name is required", errInvalidUploadSource)
	}
	modTime, timeErr := http.ParseTime(response.Header.Get("Last-Modified"))
	validator := strings.TrimSpace(response.Header.Get("ETag"))
	if validator == "" && timeErr == nil {
		validator = modTime.Format(http.TimeFormat)
	}
	destination, err := validateDestinationPath(name)
	if err != nil {
		return UploadFileSource{}, err
	}
	return UploadFileSource{Type: "http", Location: parsed.String(), Headers: headers, DestinationPath: destination, Size: size, ModTime: modTime.UTC(), HasModTime: timeErr == nil, MIMEType: strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]), Validator: validator}, nil
}

// mergeUploadHeaders returns the batch-level defaults with the per-source overrides
// applied on top of them. Header names are canonicalized, and the returned map is
// always newly allocated, so callers may mutate it without affecting either input.
func mergeUploadHeaders(defaults, overrides map[string]string) map[string]string {
	result := make(map[string]string, len(defaults)+len(overrides))
	for key, value := range defaults {
		result[http.CanonicalHeaderKey(key)] = value
	}
	for key, value := range overrides {
		result[http.CanonicalHeaderKey(key)] = value
	}
	return result
}

// applyUploadHeaders copies header values onto target, canonicalizing the names and
// dropping the ones that must stay under the transport's control: Host,
// Content-Length, Range, Connection, Transfer-Encoding and Proxy-Connection. Without
// that filter a user-supplied Range or Content-Length would corrupt the part layout,
// so a caller can always set its own Range afterwards.
func applyUploadHeaders(target http.Header, values map[string]string) {
	for key, value := range values {
		switch http.CanonicalHeaderKey(key) {
		case "Host", "Content-Length", "Range", "Connection", "Transfer-Encoding", "Proxy-Connection":
			continue
		}
		target.Set(key, value)
	}
}

// cleanDestinationPath normalizes a destination path to a slash-separated relative
// path: backslashes become slashes, "." and ".." components are resolved lexically
// and a leading slash is removed. It returns an empty string when the result would be
// empty or would climb out of the destination, which callers read as "no usable path"
// rather than as an error.
func cleanDestinationPath(value string) string {
	cleaned := path.Clean(strings.TrimSpace(strings.ReplaceAll(value, "\\", "/")))
	if cleaned == "." || cleaned == "/" {
		return ""
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return ""
	}
	return strings.TrimPrefix(cleaned, "/")
}

// validateDestinationPath normalizes and validates a destination path for a single
// file or for the base directory of a source. An empty value is valid and returns an
// empty path without an error, because the caller derives the name elsewhere.
// An absolute path, or a value that cleans away to nothing, fails with
// errInvalidUploadSource.
func validateDestinationPath(value string) (string, error) {
	normalized := strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if normalized == "" {
		return "", nil
	}
	if strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("%w: destination path must be relative", errInvalidUploadSource)
	}
	cleaned := cleanDestinationPath(normalized)
	if cleaned == "" {
		return "", fmt.Errorf("%w: invalid destination path", errInvalidUploadSource)
	}
	return cleaned, nil
}

// parseOptionalUUID parses an optional UUID field such as a destination parent. A
// blank value returns a nil UUID, which the queries read as the drive root, and
// anything unparsable fails with errInvalidUploadSource.
func parseOptionalUUID(value string) (*uuid.UUID, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid parent id", errInvalidUploadSource)
	}
	return &parsed, nil
}

// optionalString returns a pointer to value when it holds anything other than
// whitespace, and nil otherwise, so that an absent text column is stored as NULL
// instead of an empty string. The value itself is not trimmed.
func optionalString(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

// modTimesEqual reports whether two modification times are within one second of each
// other, in either direction. The tolerance absorbs the second-resolution timestamps
// of HTTP headers and some filesystems, and it matches the window the resumable
// session query uses, so both agree on whether a file is the same version.
func modTimesEqual(left, right time.Time) bool {
	difference := left.Sub(right)
	return difference >= -time.Second && difference <= time.Second
}

// uploadSkipReason applies the batch-wide filter first and then the per-source
// filter, and returns the first non-empty reason, so a file rejected by both is
// attributed to the batch-level rule.
func uploadSkipReason(batch, source uploadFilter, name string, size int64) string {
	if reason := batch.skipReason(name, size); reason != "" {
		return reason
	}
	return source.skipReason(name, size)
}

// validContentRange reports whether a Content-Range header describes exactly the
// requested part: the start offset, the end offset and the total size must all match
// what was asked for. It guards against a server that answers a range request with a
// different range, which would silently shift every following byte of the part.
func validContentRange(value string, offset, size, total int64) bool {
	var start, end, reportedTotal int64
	if _, err := fmt.Sscanf(strings.TrimSpace(value), "bytes %d-%d/%d", &start, &end, &reportedTotal); err != nil {
		return false
	}
	return start == offset && end == offset+size-1 && reportedTotal == total
}

// NewUploadHTTPClient returns the HTTP client used to read remote upload sources. It
// is deliberately hardened, because the URLs come from users:
//
//   - Proxy support is switched off, so an environment proxy cannot be used to reach
//     an address the dialer would refuse.
//   - DialContext resolves the host itself and dials the first address that
//     safeUploadAddress accepts, which admits global unicast addresses only: loopback,
//     private, link-local, multicast, unspecified and the shared or translated ranges
//     listed in nonPublicUploadPrefixes are all refused. Resolving and dialing in one
//     step also closes the DNS-rebinding window, because the address that was checked
//     is the address that is dialed, and every redirect is subject to the same check.
//   - Redirects are limited to ten hops and must stay on http or https, and the
//     Authorization, Cookie and Proxy-Authorization headers are dropped when the host
//     changes, so credentials for one source are never replayed to another.
//
// A target that has no usable public address fails with errInvalidUploadSource, and a
// request that is rejected by one of these rules fails instead of falling back to an
// unrestricted connection.
func NewUploadHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			if !safeUploadAddress(address) {
				continue
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
		}
		return nil, fmt.Errorf("%w: HTTP target has no public address", errInvalidUploadSource)
	}
	client := &http.Client{Transport: transport}
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if request.URL.Scheme != "http" && request.URL.Scheme != "https" {
			return fmt.Errorf("%w: unsupported redirect scheme", errInvalidUploadSource)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if len(via) > 0 && !strings.EqualFold(request.URL.Host, via[len(via)-1].URL.Host) {
			request.Header.Del("Authorization")
			request.Header.Del("Cookie")
			request.Header.Del("Proxy-Authorization")
		}
		return nil
	}
	return client
}

// nonPublicUploadPrefixes are address blocks that are neither loopback nor
// private, so netip's predicates accept them, but that still do not belong to a
// public peer. They matter because a user-supplied URL is allowed to reach public
// addresses only: shared carrier-grade NAT space is what overlay networks such as
// Tailscale hand out, and the translation and benchmarking ranges can carry a
// private peer's traffic.
var nonPublicUploadPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // RFC 6598 shared address space
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved for future use
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64
	netip.MustParsePrefix("2002::/16"),     // 6to4
}

// safeUploadAddress reports whether a resolved address may be dialed for a
// user-supplied URL. It refuses anything that is not a global unicast address and
// everything in nonPublicUploadPrefixes, so a source can only reach a public peer.
// An IPv4-mapped IPv6 address is first reduced to the IPv4 address it carries,
// because the predicates do not agree on unmapping and the dialer does.
func safeUploadAddress(address netip.Addr) bool {
	// Judge an IPv4-mapped address as the IPv4 address it carries. netip only
	// unmaps inside some of the predicates below, so ::ffff:0.0.0.0 would
	// otherwise pass the unspecified check while the dialer turns it back into
	// 0.0.0.0 and reaches the loopback interface.
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range nonPublicUploadPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
