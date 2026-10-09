package telegramstore

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestPlanTelegramReadsAlignsShortTail(t *testing.T) {
	plans := planTelegramReads(557056, 1234, defaultTelegramReadParallel)
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}
	plan := plans[0]
	if plan.offset != 557056 || plan.limit != telegramReadAlign || plan.skip != 0 || plan.length != 1234 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestGotdDownloadSessionRunsOneClientLifecycle(t *testing.T) {
	runner := &sessionCountingRunner{}
	storage := &GotdStorage{runner: runner}
	session, err := storage.OpenDownloadSession(context.Background(), 7)
	if err != nil {
		t.Fatalf("OpenDownloadSession() error = %v", err)
	}
	if got := runner.runs.Load(); got != 1 {
		t.Fatalf("runner calls = %d, want 1", got)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := runner.runs.Load(); got != 1 {
		t.Fatalf("runner calls after close = %d, want 1", got)
	}
}

func TestGotdDownloadSessionStopsWhenRequestIsCanceled(t *testing.T) {
	runner := &sessionCountingRunner{}
	ctx, cancel := context.WithCancel(context.Background())
	session, err := (&GotdStorage{runner: runner}).OpenDownloadSession(ctx, 7)
	if err != nil {
		t.Fatalf("OpenDownloadSession() error = %v", err)
	}
	cancel()
	if err := session.Close(); err != nil {
		t.Fatalf("Close() after cancellation error = %v", err)
	}
	if got := runner.runs.Load(); got != 1 {
		t.Fatalf("runner calls = %d, want 1", got)
	}
}

// sessionCountingRunner serves downloads and counts Run invocations, so a test can
// tell how many background sessions a storage handle actually opened.
type sessionCountingRunner struct {
	// runs counts Run invocations that reached the callback.
	runs atomic.Int32
}

// Run counts every invocation before validating it, rejects anything but a
// download and runs fn against a fresh client.
func (r *sessionCountingRunner) Run(ctx context.Context, _ int64, operation Operation, fn func(context.Context, *tg.Client) error) error {
	r.runs.Add(1)
	if operation != OperationDownload {
		return ErrInvalidRequest
	}
	return fn(ctx, new(tg.Client))
}

func TestPlanTelegramReadsAlignsArbitraryRange(t *testing.T) {
	plans := planTelegramReads(101, telegramReadChunk+5000, defaultTelegramReadParallel)
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want 2", len(plans))
	}
	first := plans[0]
	if first.offset != 0 || first.limit != telegramReadChunk || first.skip != 101 || first.length != telegramReadChunk-101 {
		t.Fatalf("first plan = %+v", first)
	}
	second := plans[1]
	if second.offset != telegramReadChunk || second.limit != 8192 || second.skip != 0 || second.length != 5101 {
		t.Fatalf("second plan = %+v", second)
	}
	for _, plan := range plans {
		if plan.offset%telegramReadAlign != 0 || plan.limit%telegramReadAlign != 0 || plan.limit > telegramReadChunk {
			t.Fatalf("unaligned plan = %+v", plan)
		}
	}
}

func TestPlanTelegramReadsStaysWithinTelegramBoundaries(t *testing.T) {
	plans := planTelegramReads(1474560, 13926400, defaultTelegramReadParallel)
	if len(plans) != defaultTelegramReadParallel {
		t.Fatalf("plans = %d, want %d", len(plans), defaultTelegramReadParallel)
	}
	for _, plan := range plans {
		if plan.offset%telegramReadAlign != 0 {
			t.Fatalf("unaligned offset: %+v", plan)
		}
		if plan.limit < telegramReadAlign || telegramReadChunk%plan.limit != 0 {
			t.Fatalf("invalid limit: %+v", plan)
		}
		end := plan.offset + int64(plan.limit) - 1
		if plan.offset/telegramReadChunk != end/telegramReadChunk {
			t.Fatalf("plan crosses 1 MiB boundary: %+v", plan)
		}
	}
}
func TestTelegramRangeReaderBoundsReadAheadBehindSlowChunk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	releaseFirst := make(chan struct{})
	invoker := &pipelinedDownloadInvoker{
		started: make(chan int64, 3),
		releases: map[int64]<-chan struct{}{
			0: releaseFirst,
		},
	}
	api := tg.NewClient(invoker)
	reader := newTelegramRangeReader(ctx, cancel, 4, 2)
	errCh := make(chan error, 1)
	go func() {
		errCh <- reader.fill(ctx, api, &tg.InputDocumentFileLocation{}, 0, 3*telegramReadChunk, nil)
	}()
	// The chunk budget covers the prefetched chunks as well, so fill can only
	// reach the end of the range while a consumer drains it.
	drained := make(chan error, 1)
	go func() {
		_, err := io.CopyN(io.Discard, reader, 3*telegramReadChunk)
		drained <- err
	}()

	started := map[int64]bool{}
	for len(started) < 2 {
		select {
		case offset := <-invoker.started:
			started[offset] = true
		case <-time.After(time.Second):
			t.Fatal("initial Telegram reads did not start")
		}
	}
	if !started[0] || !started[telegramReadChunk] {
		t.Fatalf("initial offsets = %#v", started)
	}
	select {
	case offset := <-invoker.started:
		t.Fatalf("read-ahead escaped ordered window while first chunk was stalled: offset %d", offset)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseFirst)
	select {
	case offset := <-invoker.started:
		if offset != 2*telegramReadChunk {
			t.Fatalf("next offset = %d, want %d", offset, 2*telegramReadChunk)
		}
	case <-time.After(time.Second):
		t.Fatal("next Telegram read did not start after stalled head completed")
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("fill() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("fill() did not complete")
	}
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("drain error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the range was not delivered completely")
	}
}

func TestTelegramRangeReaderRetriesTimedOutChunk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	invoker := &timeoutThenSuccessDownloadInvoker{}
	api := tg.NewClient(invoker)
	reader := newTelegramRangeReader(ctx, cancel, 2, 1)
	reader.timeout = 10 * time.Millisecond
	reader.attempts = 2

	if err := reader.fill(ctx, api, &tg.InputDocumentFileLocation{}, 0, telegramReadAlign, nil); err != nil {
		t.Fatalf("fill() error = %v", err)
	}
	if got := invoker.calls.Load(); got != 2 {
		t.Fatalf("download calls = %d, want 2", got)
	}
}

func TestTelegramRangeReaderRetriesRPCTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	invoker := &rpcTimeoutThenSuccessDownloadInvoker{}
	api := tg.NewClient(invoker)
	reader := newTelegramRangeReader(ctx, cancel, 2, 1)
	reader.attempts = 2

	if err := reader.fill(ctx, api, &tg.InputDocumentFileLocation{}, 0, telegramReadAlign, nil); err != nil {
		t.Fatalf("fill() error = %v", err)
	}
	if got := invoker.calls.Load(); got != 2 {
		t.Fatalf("download calls = %d, want 2", got)
	}
}

func TestTelegramRangeReaderRefreshesExpiredFileReference(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	invoker := &expiringDownloadInvoker{}
	api := tg.NewClient(invoker)
	reader := newTelegramRangeReader(ctx, cancel, 2, 1)
	fresh := &tg.InputDocumentFileLocation{ID: 2}
	var refreshes atomic.Int32
	err := reader.fill(ctx, api, &tg.InputDocumentFileLocation{ID: 1}, 0, telegramReadAlign, func(context.Context) (*tg.InputDocumentFileLocation, error) {
		refreshes.Add(1)
		return fresh, nil
	})
	if err != nil {
		t.Fatalf("fill() error = %v", err)
	}
	if got := invoker.calls.Load(); got != 2 {
		t.Fatalf("download calls = %d, want 2", got)
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

// expiringDownloadInvoker impersonates Telegram answering a stale location: the
// first UploadGetFile call fails with FILE_REFERENCE_EXPIRED and every later call
// succeeds, so a test can pin that the reader refreshes the reference exactly once.
type expiringDownloadInvoker struct {
	// calls counts Invoke invocations; the first one is the expired attempt.
	calls atomic.Int32
}

// Invoke fails the first call with FILE_REFERENCE_EXPIRED and serves the rest with
// a full-size file box.
func (i *expiringDownloadInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	request := input.(*tg.UploadGetFileRequest)
	if i.calls.Add(1) == 1 {
		return tgerr.New(400, "FILE_REFERENCE_EXPIRED")
	}
	if request.Location.(*tg.InputDocumentFileLocation).ID != 2 {
		return tgerr.New(400, "FILE_REFERENCE_EXPIRED")
	}
	box := output.(*tg.UploadFileBox)
	box.File = &tg.UploadFile{Bytes: make([]byte, request.Limit)}
	return nil
}

// pipelinedDownloadInvoker lets a test hold individual chunk reads in flight: each
// request is announced on started and then blocks until the release channel
// registered for its offset is closed, which pins how far ahead the reader reads.
type pipelinedDownloadInvoker struct {
	// started receives the offset of every request as it begins.
	started chan int64
	// releases maps a chunk offset to the channel whose close lets that request
	// finish; an offset without an entry returns immediately.
	releases map[int64]<-chan struct{}
}

// Invoke announces the request offset, waits for its release or ctx and then
// returns a full-size file box.
func (i *pipelinedDownloadInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	request := input.(*tg.UploadGetFileRequest)
	i.started <- request.Offset
	if release := i.releases[request.Offset]; release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	box := output.(*tg.UploadFileBox)
	box.File = &tg.UploadFile{Bytes: make([]byte, request.Limit)}
	return nil
}

// timeoutThenSuccessDownloadInvoker makes the first request outlive its context and
// succeed afterwards, so a test can pin that a chunk which times out is retried
// rather than failing the whole read.
type timeoutThenSuccessDownloadInvoker struct {
	// calls counts Invoke invocations; the first one is the timed-out attempt.
	calls atomic.Int32
}

// Invoke blocks the first call until ctx is done and serves every later call.
func (i *timeoutThenSuccessDownloadInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	request := input.(*tg.UploadGetFileRequest)
	if i.calls.Add(1) == 1 {
		<-ctx.Done()
		return ctx.Err()
	}
	box := output.(*tg.UploadFileBox)
	box.File = &tg.UploadFile{Bytes: make([]byte, request.Limit)}
	return nil
}

// rpcTimeoutThenSuccessDownloadInvoker answers the first request with Telegram's
// RPC-level -503 Timeout and succeeds afterwards, so a test can pin that the retry
// path covers an error returned by the API rather than by the transport.
type rpcTimeoutThenSuccessDownloadInvoker struct {
	// calls counts Invoke invocations; the first one is the rejected attempt.
	calls atomic.Int32
}

// Invoke returns the -503 Timeout RPC error on the first call and serves the rest.
func (i *rpcTimeoutThenSuccessDownloadInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	request := input.(*tg.UploadGetFileRequest)
	if i.calls.Add(1) == 1 {
		return tgerr.New(-503, "Timeout")
	}
	box := output.(*tg.UploadFileBox)
	box.File = &tg.UploadFile{Bytes: make([]byte, request.Limit)}
	return nil
}

func TestTelegramRangeReaderCloseWaitsForFill(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reader := newTelegramRangeReader(ctx, cancel, 2, 1)
	closed := make(chan struct{})
	go func() {
		_ = reader.Close()
		close(closed)
	}()

	// Close cancels the stream, but that cancellation is not a substitute for the
	// fill goroutine ending: until finish reports the outcome, the fetches and
	// their payloads are still alive.
	select {
	case <-closed:
		t.Fatal("Close() returned before the fill goroutine finished")
	case <-time.After(50 * time.Millisecond):
	}

	reader.finish(nil)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close() did not return after the fill goroutine finished")
	}
}

func TestTelegramRangeReaderKeepsChunkBudgetAcrossPrefetchAndFetches(t *testing.T) {
	for _, test := range []struct {
		name     string
		buffers  int
		parallel int
		want     int
	}{
		{name: "defaults", buffers: 0, parallel: 0, want: defaultTelegramReadBuffers},
		{name: "budget above parallelism", buffers: 32, parallel: 4, want: 32},
		{name: "budget equals parallelism", buffers: 4, parallel: 4, want: 4},
		{name: "budget below parallelism", buffers: 2, parallel: 8, want: 2},
		{name: "single chunk budget", buffers: 1, parallel: 4, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			reader := newTelegramRangeReader(ctx, cancel, test.buffers, test.parallel)
			// The chunks waiting in the prefetch channel and the fetches that hold
			// a payload before it reaches that channel both count against the
			// budget, which is what callers read as the per stream memory bound.
			if held := cap(reader.buffers) + reader.parallel; held != test.want {
				t.Fatalf("chunks held = %d (%d prefetched + %d in flight), want %d",
					held, cap(reader.buffers), reader.parallel, test.want)
			}
		})
	}
}

func TestTelegramRangeReaderDeliversRangeWithSingleChunkBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// One chunk of budget with more fetches configured is the tightest
	// configuration: the reader narrows the fetch window to the budget and leaves
	// the prefetch channel unbuffered, so the range must still arrive complete and
	// in order instead of stalling.
	api := tg.NewClient(&offsetPatternDownloadInvoker{})
	reader := newTelegramRangeReader(ctx, cancel, 1, 4)
	length := int64(3*telegramReadChunk + 777)
	go func() {
		reader.finish(reader.fill(ctx, api, &tg.InputDocumentFileLocation{}, 0, length, nil))
	}()

	got := make([]byte, 0, length)
	buf := make([]byte, 64*1024)
	for int64(len(got)) < length {
		n, err := reader.Read(buf)
		if err != nil {
			t.Fatalf("Read() error = %v after %d bytes", err, len(got))
		}
		got = append(got, buf[:n]...)
	}
	for index, value := range got {
		if value != byte(index) {
			t.Fatalf("byte at %d = %d, want %d", index, value, byte(index))
		}
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// offsetPatternDownloadInvoker serves every request with a payload whose bytes
// encode the absolute offset they belong to, so a test can verify that a reader
// reassembles a range in order and without gaps.
type offsetPatternDownloadInvoker struct{}

// Invoke fills the response with the absolute offset of each byte.
func (i *offsetPatternDownloadInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	request := input.(*tg.UploadGetFileRequest)
	payload := make([]byte, request.Limit)
	for index := range payload {
		payload[index] = byte(request.Offset + int64(index))
	}
	box := output.(*tg.UploadFileBox)
	box.File = &tg.UploadFile{Bytes: payload}
	return nil
}
