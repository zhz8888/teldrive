// Package api adapts the generated ogen HTTP contract to TelDrive's domain
// services.
//
// Handlers only decode the request, resolve the caller's access, call one
// service method and map domain errors onto HTTP responses with
// mapServiceError; storage and Telegram work stays in the domain packages.
// Operations whose response body is streamed rather than serialized (file
// downloads and the server-sent event feed) live on RawHandler, and the
// credential handling shared by every route lives in security.go.
package api

import (
	"errors"

	"github.com/zhz8888/teldrive/v2/internal/api/gen"
	"github.com/zhz8888/teldrive/v2/internal/authn"
	"github.com/zhz8888/teldrive/v2/internal/bots"
	"github.com/zhz8888/teldrive/v2/internal/catalog"
	"github.com/zhz8888/teldrive/v2/internal/channels"
	"github.com/zhz8888/teldrive/v2/internal/events"
	"github.com/zhz8888/teldrive/v2/internal/fileops"
	"github.com/zhz8888/teldrive/v2/internal/health"
	"github.com/zhz8888/teldrive/v2/internal/jobs"
	"github.com/zhz8888/teldrive/v2/internal/shares"
	"github.com/zhz8888/teldrive/v2/internal/telegramstore"
	"github.com/zhz8888/teldrive/v2/internal/transfer"
	"github.com/zhz8888/teldrive/v2/internal/uploads"
)

// ErrOperationUnavailable reports that the service backing an operation was not
// wired into the Handler. Handlers return it when a route exists in this build
// but its dependency was skipped, and mapServiceError turns it into a 503
// service_unavailable response.
var ErrOperationUnavailable = errors.New("operation is not implemented")

// Handler implements the generated ogen handler interfaces by delegating every
// operation to a domain service.
//
// The composition root assembles one Handler during startup and shares it across
// all requests; the fields are written only before the server starts serving and
// are read concurrently afterwards. Handlers that check a dependency before use
// answer 503 with ErrOperationUnavailable when the service is nil; the
// remaining handlers dereference their service directly and assume it is wired.
type Handler struct {
	// Catalog serves file and folder metadata operations: lookup, listing,
	// create, update, move, trash and restore.
	Catalog *catalog.Service
	// Uploads owns the upload session state machine: create, list, complete and
	// abort.
	Uploads *uploads.Service
	// UploadPipeline writes uploaded parts to storage and is required by
	// PutUploadPart.
	UploadPipeline *transfer.Pipeline
	// Downloader opens readers over stored file content for the streaming
	// download handlers.
	Downloader *transfer.Downloader
	// Events backs the server-sent event feed, including its replay cursor and
	// per-user wakeup channel.
	Events *events.Service
	// Health answers the liveness and readiness probes.
	Health *health.Service
	// Jobs exposes the background job runtime: queues, on-demand jobs and
	// periodic schedules.
	Jobs *jobs.Runtime
	// Auth implements Telegram login flows, token refresh, sessions and API keys.
	Auth *authn.Service
	// Bots manages the Telegram bot accounts users register.
	Bots *bots.Service
	// Channels manages the Telegram channels used as file storage.
	Channels *channels.Service
	// FileOps implements file operations that span several files, such as copy
	// and trash cleanup.
	FileOps *fileops.Service
	// Shares manages public share links together with share-scoped uploads and
	// downloads.
	Shares *shares.Service
	// TelegramAccount performs Telegram account-level calls that are not bound to
	// a user session.
	TelegramAccount telegramstore.Account
	// ActiveEncryptionKeyVersion is the key version given to newly written upload
	// parts; parts already in storage keep decrypting with the version recorded
	// with them.
	ActiveEncryptionKeyVersion int32
}

// NewHandler returns a Handler wired with the services needed by the file,
// upload, download, health and event routes. The account-scoped services (auth,
// bots, channels, file operations and shares) are attached separately with
// ConfigureDomains, and the background job runtime with ConfigureJobs.
func NewHandler(catalogService *catalog.Service, uploadService *uploads.Service, uploadPipeline *transfer.Pipeline, downloader *transfer.Downloader, healthService *health.Service, activeEncryptionKeyVersion int32, eventService *events.Service) *Handler {
	return &Handler{
		Catalog: catalogService, Uploads: uploadService, UploadPipeline: uploadPipeline,
		Downloader: downloader, Events: eventService, Health: healthService,
		ActiveEncryptionKeyVersion: activeEncryptionKeyVersion,
	}
}

// ConfigureDomains attaches the account-scoped services to an already created
// Handler and returns the same value so calls can be chained. It must run before
// the HTTP server starts serving requests, and it tolerates a nil receiver so a
// partially built composition root does not panic. Services left nil keep their
// routes unavailable, which the handlers report as 503.
func (h *Handler) ConfigureDomains(authService *authn.Service, botService *bots.Service, channelService *channels.Service, fileService *fileops.Service, shareService *shares.Service, account telegramstore.Account) *Handler {
	if h != nil {
		h.Auth, h.Bots, h.Channels, h.FileOps, h.Shares = authService, botService, channelService, fileService, shareService
		h.TelegramAccount = account
	}
	return h
}

// ConfigureJobs attaches the background job runtime used by the job and queue
// endpoints and returns the same Handler. It is meant to run once during
// startup and tolerates a nil receiver.
func (h *Handler) ConfigureJobs(runtime *jobs.Runtime) *Handler {
	if h != nil {
		h.Jobs = runtime
	}
	return h
}

var _ gen.Handler = (*Handler)(nil)
