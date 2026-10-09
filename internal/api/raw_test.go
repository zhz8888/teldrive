package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zhz8888/teldrive/v2/internal/api/gen"
	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/dbtypes"
	"github.com/zhz8888/teldrive/v2/internal/telegramstore"
	"github.com/zhz8888/teldrive/v2/internal/transfer"
)

func TestIsClientDisconnect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "canceled context", err: context.Canceled, want: true},
		{name: "connection reset", err: syscall.ECONNRESET, want: true},
		{name: "broken pipe", err: syscall.EPIPE, want: true},
		{name: "wrapped connection reset", err: fmt.Errorf("write tcp: %w", syscall.ECONNRESET), want: true},
		{name: "stream failure", err: errors.New("upstream read failed"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isClientDisconnect(tt.err); got != tt.want {
				t.Fatalf("isClientDisconnect(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// streamTestCatalog is the in-memory transfer.FileCatalog the streamFile tests
// resolve against: it returns the seeded row and part list for any owner.
type streamTestCatalog struct {
	// file is the row Get returns.
	file *sqlcgen.File
	// parts is the part list Parts returns.
	parts []*sqlcgen.FilePart
}

// Get returns the seeded file row without checking the owner.
func (c *streamTestCatalog) Get(context.Context, int64, uuid.UUID) (*sqlcgen.File, error) {
	return c.file, nil
}

// Parts returns the seeded part list without checking the owner.
func (c *streamTestCatalog) Parts(context.Context, int64, uuid.UUID) ([]*sqlcgen.FilePart, error) {
	return c.parts, nil
}

// streamTestStorage serves ranges out of one in-memory payload so a streamFile
// test can read the exact bytes the handler announced without a Telegram client.
// It deliberately implements only Storage, so the downloader wraps it in its
// plain adapter.
type streamTestStorage struct {
	// payload is the stored content of the single part every test file has.
	payload []byte
}

// OpenRange returns a reader over the requested window of the payload, clamping
// an over-long length the way the storage contract does.
func (s *streamTestStorage) OpenRange(_ context.Context, request telegramstore.RangeRequest) (io.ReadCloser, error) {
	offset := min(max(request.Offset, 0), int64(len(s.payload)))
	end := int64(len(s.payload))
	if request.Length >= 0 && offset+request.Length < end {
		end = offset + request.Length
	}
	return io.NopCloser(bytes.NewReader(s.payload[offset:end])), nil
}

// Upload is unused by the streaming tests and returns an empty part.
func (s *streamTestStorage) Upload(context.Context, telegramstore.UploadRequest) (telegramstore.StoredPart, error) {
	return telegramstore.StoredPart{}, nil
}

// DeleteMessages is a no-op; the streaming tests never delete.
func (s *streamTestStorage) DeleteMessages(context.Context, int64, int64, []int64) error { return nil }

// CopyPart is unused by the streaming tests and returns an empty part.
func (s *streamTestStorage) CopyPart(context.Context, int64, int64, int64, int64) (telegramstore.StoredPart, error) {
	return telegramstore.StoredPart{}, nil
}

// CreateChannel is unused by the streaming tests and returns an empty channel.
func (s *streamTestStorage) CreateChannel(context.Context, int64, string) (telegramstore.Channel, error) {
	return telegramstore.Channel{}, nil
}

// DeleteChannel is a no-op, like DeleteMessages.
func (s *streamTestStorage) DeleteChannel(context.Context, int64, int64) error { return nil }

// newStreamTestHandler returns a RawHandler that streams body as one part of a
// single file whose stored mime type is contentType, together with that file row.
func newStreamTestHandler(t *testing.T, body []byte, contentType string) (*RawHandler, *sqlcgen.File) {
	t.Helper()
	file := &sqlcgen.File{
		ID: dbtypes.UUID(uuid.New()), UserID: 7, Name: "upload.html", Kind: sqlcgen.FileKindFile,
		Status: sqlcgen.FileStatusActive, Size: pgtype.Int8{Int64: int64(len(body)), Valid: true},
		MimeType: pgtype.Text{String: contentType, Valid: true},
		ModTime:  pgtype.Timestamptz{Time: time.Unix(0, 0).UTC(), Valid: true},
	}
	catalog := &streamTestCatalog{file: file, parts: []*sqlcgen.FilePart{{
		PartNo: 1, ChannelID: 1, MessageID: 1,
		PlainSize:  pgtype.Int8{Int64: int64(len(body)), Valid: true},
		StoredSize: pgtype.Int8{Int64: int64(len(body)), Valid: true},
	}}}
	handler := &Handler{Downloader: transfer.NewDownloader(catalog, &streamTestStorage{payload: body}, nil, 0)}
	return NewRawHandler(handler), file
}

// TestStreamFileHardensStoredContent pins the behaviour of the one exit for
// stored content: every response is nosniff and sandboxed, and a stored content
// type a browser could execute is sent as an attachment even though the client
// asked for inline rendering. Removing either guard would let an uploaded HTML
// or SVG file run script in the application's origin.
func TestStreamFileHardensStoredContent(t *testing.T) {
	t.Parallel()

	body := []byte("<script>alert(document.domain)</script>")
	tests := []struct {
		name        string
		contentType string
		attachment  bool
		wantDispose string
	}{
		{name: "html is forced to download", contentType: "text/html", wantDispose: `attachment; filename=upload.html`},
		{name: "parameterised html is forced to download", contentType: "text/html; charset=utf-8", wantDispose: `attachment; filename=upload.html`},
		{name: "svg is forced to download", contentType: "image/svg+xml", wantDispose: `attachment; filename=upload.html`},
		{name: "xhtml is forced to download", contentType: "application/xhtml+xml", wantDispose: `attachment; filename=upload.html`},
		{name: "an xml subtype is forced to download", contentType: "application/atom+xml", wantDispose: `attachment; filename=upload.html`},
		{name: "an unclassifiable type is forced to download", contentType: "not a media type", wantDispose: `attachment; filename=upload.html`},
		{name: "plain text stays inline", contentType: "text/plain", wantDispose: `inline; filename=upload.html`},
		{name: "binary stays inline", contentType: "application/octet-stream", wantDispose: `inline; filename=upload.html`},
		{name: "a requested download stays an attachment", contentType: "text/plain", attachment: true, wantDispose: `attachment; filename=upload.html`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handler, file := newStreamTestHandler(t, body, test.contentType)
			fileID, ok := dbtypes.GoogleUUID(file.ID)
			if !ok {
				t.Fatal("test file has no id")
			}
			recorder := httptest.NewRecorder()
			if err := handler.streamFile(context.Background(), recorder, 7, fileID, file, gen.OptString{}, gen.OptETag{}, test.attachment); err != nil {
				t.Fatalf("streamFile() error = %v", err)
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
			if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := recorder.Header().Get("Content-Security-Policy"); got != "default-src 'none'; sandbox" {
				t.Fatalf("Content-Security-Policy = %q, want the sandbox policy", got)
			}
			if got := recorder.Header().Get("Content-Disposition"); got != test.wantDispose {
				t.Fatalf("Content-Disposition = %q, want %q", got, test.wantDispose)
			}
			if got := recorder.Header().Get("Content-Type"); got != test.contentType {
				t.Fatalf("Content-Type = %q, want %q", got, test.contentType)
			}
			if got := recorder.Body.String(); got != string(body) {
				t.Fatalf("body = %q, want %q", got, body)
			}
		})
	}
}

// TestStreamFileKeepsConditionalAndRangeSemantics checks that the hardening did
// not disturb the two shortcut paths: a matching If-None-Match still answers 304
// without a body, and a Range still answers 206 with the announced slice.
func TestStreamFileKeepsConditionalAndRangeSemantics(t *testing.T) {
	t.Parallel()

	handler, file := newStreamTestHandler(t, []byte("0123456789"), "application/octet-stream")
	fileID, ok := dbtypes.GoogleUUID(file.ID)
	if !ok {
		t.Fatal("test file has no id")
	}
	etag := contentETag(file)

	notModified := httptest.NewRecorder()
	if err := handler.streamFile(context.Background(), notModified, 7, fileID, file, gen.OptString{}, gen.NewOptETag(etag), false); err != nil {
		t.Fatalf("conditional streamFile() error = %v", err)
	}
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
		t.Fatalf("conditional response = %d, body %q, want 304 with no body", notModified.Code, notModified.Body.String())
	}
	if got := notModified.Header().Get("ETag"); got != string(etag) {
		t.Fatalf("conditional ETag = %q, want %q", got, etag)
	}

	partial := httptest.NewRecorder()
	if err := handler.streamFile(context.Background(), partial, 7, fileID, file, gen.NewOptString("bytes=2-4"), gen.OptETag{}, false); err != nil {
		t.Fatalf("range streamFile() error = %v", err)
	}
	if partial.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, want %d", partial.Code, http.StatusPartialContent)
	}
	if got := partial.Body.String(); got != "234" {
		t.Fatalf("range body = %q, want %q", got, "234")
	}
	if got := partial.Header().Get("Content-Range"); got != "bytes 2-4/10" {
		t.Fatalf("Content-Range = %q, want %q", got, "bytes 2-4/10")
	}
	if got := partial.Header().Get("Content-Length"); got != "3" {
		t.Fatalf("Content-Length = %q, want %q", got, "3")
	}
}
