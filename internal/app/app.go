// Package app is the composition root of the TelDrive backend and the owner of
// the process lifecycle.
//
// New builds the whole object graph in the one order the dependencies allow:
// configuration is validated, a legacy database is migrated when asked for, the
// SQL schema is configured, River and application migrations run, the long-lived
// connection pool opens, and only then are the cipher, the shared cache, the
// Telegram gateways, the domain services, the job runtime and the HTTP router
// created. Each stage consumes the previous one — the Telegram session provider
// and every service need the open pool, the upload pipeline needs channels and
// storage, and the job runtime needs the file operations service — so the order
// is a correctness requirement, not a style choice. Migrations likewise run on
// their own short-lived connection before the pool exists, which keeps a schema
// upgrade from serving traffic against a half-migrated database.
//
// Run and Serve own the running process. They start the event service first and
// the River workers second, so nothing can subscribe to a service or enqueue work
// against a runtime that is not up, and then serve HTTP on a listener that is
// never silently re-bound: a taken port is a startup error rather than a server
// that quietly moved elsewhere.
//
// Shutdown unwinds in the opposite direction — SSE handlers, then HTTP while
// in-flight requests drain, then warm Telegram download clients, then River
// workers, then the shared cache, and the connection pool last — so a request
// still being served keeps its database access for as long as it is allowed to
// run. Close is the bounded-timeout form for callers that treat an App as a
// resource rather than running Serve directly.
//
// The package also installs the browser-facing security chain, namely trusted
// proxy detection, session renewal and CSRF checks, and serves the UI bundle
// embedded in the binary.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	api "github.com/tgdrive/teldrive/v2/internal/api"
	"github.com/tgdrive/teldrive/v2/internal/authn"
	"github.com/tgdrive/teldrive/v2/internal/bots"
	"github.com/tgdrive/teldrive/v2/internal/cache"
	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/channels"
	"github.com/tgdrive/teldrive/v2/internal/config"
	"github.com/tgdrive/teldrive/v2/internal/database"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	userevents "github.com/tgdrive/teldrive/v2/internal/events"
	"github.com/tgdrive/teldrive/v2/internal/fileops"
	"github.com/tgdrive/teldrive/v2/internal/health"
	"github.com/tgdrive/teldrive/v2/internal/jobs"
	"github.com/tgdrive/teldrive/v2/internal/legacymigrate"
	"github.com/tgdrive/teldrive/v2/internal/secureblob"
	"github.com/tgdrive/teldrive/v2/internal/shares"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
	"github.com/tgdrive/teldrive/v2/internal/transfer"
	"github.com/tgdrive/teldrive/v2/internal/uploads"
)

var (
	// ErrInvalidDependencies reports that Run or Serve was called on an App whose
	// required collaborators are missing, which happens when the value did not
	// come from New or was left zero. It is detected before any listener is bound
	// or touched, so a rejected Serve leaves the caller's listener open and owned
	// by the caller.
	ErrInvalidDependencies = errors.New("invalid application dependencies")
	// ErrAlreadyRunning reports that Serve was called while another Serve call on
	// the same App is still active. The rejected call leaves the supplied listener
	// untouched, so the caller still owns closing it.
	ErrAlreadyRunning = errors.New("application is already running")
)

// Dependencies carries the collaborators and metadata that New cannot derive from
// configuration. Every field is an override, so the zero value is valid: a nil
// Storage makes New build the storage for the configured Telegram backend, a nil
// Authenticator makes New wire its own authentication service into the generated
// server, and a nil Logger falls back to slog.Default.
type Dependencies struct {
	// Storage replaces the Telegram storage the configured backend would build,
	// which lets tests and alternate deployments inject a fake. When nil, New
	// constructs the real one; when set, the caller owns it and App never closes
	// it.
	Storage telegramstore.Storage
	// Authenticator replaces the credential resolver used by the HTTP security
	// layer. When nil, New passes its own authn service, which is what production
	// uses.
	Authenticator api.Authenticator
	// Logger receives startup, migration and request logging. A nil value falls
	// back to slog.Default.
	Logger *slog.Logger
	// Version is the build version reported by the health endpoints and stamped
	// into every health status. It may be empty when the binary carries no version
	// information.
	Version string
}

// globalCacher is the contract of the process-local cache the App owns: the cache
// operations the services use plus Close. Naming both in one interface is what makes
// the release path in Shutdown compile only for a cache implementation that can
// actually be closed, instead of silently skipping one that cannot.
type globalCacher interface {
	cache.Cacher
	// Close releases the resources the cache holds. Shutdown calls it once the
	// workers have stopped and before the connection pool is closed.
	Close()
}

// App owns a fully constructed backend together with the resources it opened. New
// returns it unstarted; Run or Serve starts serving, and Shutdown or Close
// releases the resources exactly once. The lifecycle flags make the value safe for
// concurrent use, but only one Serve call may be active at a time.
type App struct {
	// config is the validated configuration New received. It is read-only after
	// construction and supplies the HTTP address and shutdown timeout.
	config config.Config
	// pool is the long-lived PostgreSQL pool shared by every service in the
	// graph. Shutdown closes it last, so it must outlive all of them.
	pool *pgxpool.Pool
	// http is the configured HTTP server whose Handler is the router New built.
	// Run binds its address; Serve uses the listener it is given instead.
	http *http.Server
	// jobs is the River runtime for background work. Serve starts it only when
	// Jobs.RunWorkers is enabled, so it may legitimately stay idle.
	jobs *jobs.Runtime
	// events is the SSE event service. It is started before HTTP starts serving so
	// no request can subscribe to a service that is not running yet.
	events *userevents.Service
	// telegramDownloads is the warm Telegram client pool, or nil when the backend
	// does not pool clients or the storage was injected. Shutdown closes it after
	// HTTP has drained but before the job workers stop.
	telegramDownloads *telegramstore.DownloadClientPool
	// globalCache is the process-local cache shared by the catalog and the Telegram
	// storage layer. It is closed after the workers stop; the interface also
	// guarantees Close, and Cache exposes it as a plain cache.Cacher.
	globalCache globalCacher

	// mu guards the lifecycle flags below so Serve and Shutdown cannot interleave.
	mu sync.Mutex
	// running reports whether a Serve call currently owns the HTTP server.
	running bool
	// closed reports whether Shutdown has already claimed the resource release. It is
	// never reset, so a closed App cannot be restarted.
	closed bool
	// shutdownDone is closed once the single resource release has finished, and is
	// created under mu when closed flips to true. Every later or concurrent Shutdown
	// waits on it instead of returning before the resources are gone.
	shutdownDone chan struct{}
	// shutdownErr is the result of that release. It is written before shutdownDone is
	// closed and read only after it, so the two are never observed out of order.
	shutdownErr error
	// shutdownOnce runs the release exactly once even when several callers arrive at
	// the same moment; they all observe the same shutdownErr.
	shutdownOnce sync.Once
}

// New builds the complete v2 backend. It upgrades legacy databases and runs all
// TelDrive, River, and RiverPro migrations before opening the long-lived pool.
func New(ctx context.Context, cfg config.Config, dependencies Dependencies) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Database.AutoMigrateLegacy {
		migrationVerifier, err := buildLegacyBotVerifier(cfg, dependencies.Logger)
		if err != nil {
			return nil, fmt.Errorf("create legacy bot verifier: %w", err)
		}
		report, migrated, err := legacymigrate.MigrateIfNeeded(ctx, cfg.Database, cfg.Security.DataKey, migrationVerifier)
		if err != nil {
			return nil, fmt.Errorf("migrate legacy database: %w", err)
		}
		if migrated {
			logger := dependencies.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Info("database.legacy_migration.completed",
				"users", report.Users,
				"channels", report.Channels,
				"bots", report.Bots,
				"folders", report.Folders,
				"files", report.Files,
				"file_parts", report.FileParts,
				"backup_schema", report.BackupSchema,
			)
		}
	}
	if err := sqlcgen.ConfigureSchema(cfg.Database.Schema); err != nil {
		return nil, fmt.Errorf("configure database schema: %w", err)
	}
	if err := database.Migrate(ctx, cfg.Database); err != nil {
		return nil, fmt.Errorf("migrate application database: %w", err)
	}
	pool, err := database.Open(ctx, cfg.Database)
	if err != nil {
		return nil, err
	}
	cleanupPool := true
	defer func() {
		if cleanupPool {
			pool.Close()
		}
	}()

	secureCipher, err := secureblob.New(cfg.Security.DataKey)
	if err != nil {
		return nil, fmt.Errorf("create secure data cipher: %w", err)
	}
	// One bounded process-local cache is shared by domains, while each domain
	// owns its key policy and explicit post-commit invalidation rules.
	globalCache := cache.NewMemoryCache(int(cfg.Cache.Memory.Size))
	cleanupGlobalCache := true
	defer func() {
		if cleanupGlobalCache {
			globalCache.Close()
		}
	}()
	telegram, err := buildTelegramComponents(cfg, pool, secureCipher, dependencies.Logger, dependencies.Storage, globalCache)
	if err != nil {
		return nil, err
	}
	cleanupTelegramDownloads := telegram.downloadClients != nil
	defer func() {
		if cleanupTelegramDownloads {
			closeCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
			defer cancel()
			_ = telegram.downloadClients.Close(closeCtx)
		}
	}()
	authService, err := authn.NewService(pool, secureCipher, telegram.login, authn.Config{
		SigningKey: cfg.Security.SigningKey, Issuer: cfg.Security.Issuer,
		AllowedUsers:   cfg.Security.AllowedUsers,
		AccessTokenTTL: cfg.Security.AccessTokenTTL, RefreshTokenTTL: cfg.Security.RefreshTokenTTL,
		LoginFlowTTL: cfg.Security.LoginFlowTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("create authentication service: %w", err)
	}
	storage := telegram.storage
	telegramAccount := telegram.account
	authenticator := dependencies.Authenticator
	if authenticator == nil {
		authenticator = authService
	}

	eventService, err := userevents.NewService(pool, dependencies.Logger, userevents.Config{
		BatchSize:             int32(cfg.Events.BatchSize),
		MaxConnectionsPerUser: cfg.Events.MaxConnectionsPerUser,
		Heartbeat:             cfg.Events.Heartbeat,
		WriteTimeout:          cfg.Events.WriteTimeout,
		TicketTTL:             cfg.Events.TicketTTL,
		CleanupInterval:       cfg.Events.CleanupInterval,
		ConnectTimeout:        cfg.Events.ConnectTimeout,
		PingInterval:          cfg.Events.PingInterval,
		ReconnectMin:          cfg.Events.ReconnectMin,
		ReconnectMax:          cfg.Events.ReconnectMax,
	})
	if err != nil {
		return nil, fmt.Errorf("create event service: %w", err)
	}
	botService, err := bots.NewService(pool, secureCipher, telegram.verifier)
	if err != nil {
		return nil, fmt.Errorf("create bot service: %w", err)
	}
	catalogService := catalog.NewService(pool, globalCache)
	uploadService := uploads.NewService(pool, cfg.Uploads.SessionTTL)
	uploadService.SetCacheInvalidator(catalogService)
	channelService := channels.NewService(pool, channels.TelegramCreator{Storage: storage}, channels.Config{
		PartLimit: cfg.Telegram.ChannelPartLimit, AutoCreate: cfg.Telegram.AutoChannelCreate,
		NamePrefix: cfg.Telegram.ChannelNamePrefix,
	})
	keys := make(transfer.StaticKeyProvider, len(cfg.Encryption.Keys))
	maps.Copy(keys, cfg.Encryption.Keys)
	uploadPipeline := transfer.NewPipeline(uploadService, channelService, storage, keys, transfer.Config{
		UploadThreads: cfg.Telegram.UploadThreads, RandomizePartNames: cfg.Telegram.RandomizePartNames,
		DisableHashing: !cfg.Uploads.HashingEnabled,
	})
	downloader := transfer.NewDownloader(catalogService, storage, keys)
	fileService, err := fileops.NewService(pool, catalogService, channelService, storage)
	if err != nil {
		return nil, fmt.Errorf("create file operations service: %w", err)
	}
	shareService, err := shares.NewService(pool, catalogService)
	if err != nil {
		return nil, fmt.Errorf("create share service: %w", err)
	}
	healthService := health.NewService(dependencies.Version, pool)
	jobRuntime, err := jobs.NewRuntimeWithServices(
		pool, storage, cfg.Database.Schema, botService, secureCipher, cfg.Uploads.SessionTTL,
		jobs.UploaderServices{Catalog: catalogService, Uploads: uploadService, Pipeline: uploadPipeline, ActiveKeyVersion: cfg.Encryption.ActiveKeyVersion, LocalImportRoots: cfg.Uploads.LocalImportRoots},
		fileService,
	)
	if err != nil {
		return nil, fmt.Errorf("create job runtime: %w", err)
	}
	handler := api.NewHandler(
		catalogService, uploadService, uploadPipeline, downloader, healthService,
		cfg.Encryption.ActiveKeyVersion, eventService,
	).ConfigureDomains(authService, botService, channelService, fileService, shareService, telegramAccount).
		ConfigureJobs(jobRuntime)
	httpServer, err := api.NewServer(handler, api.NewSecurity(authenticator, eventService))
	if err != nil {
		return nil, fmt.Errorf("create generated HTTP server: %w", err)
	}
	webUI, err := newWebUIHandler()
	if err != nil {
		return nil, fmt.Errorf("configure web UI: %w", err)
	}
	mux := chi.NewRouter()
	mux.Use(requestIDMiddleware)
	requestSecurity, err := newRequestSecurity(cfg.HTTP.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("configure trusted proxies: %w", err)
	}
	// The logger needs the trusted proxy list to decide whether the forwarding
	// headers of a request may be believed when it records the client address.
	mux.Use(httpRequestLogger(dependencies.Logger, requestSecurity))
	routeApplication(mux, requestSecurity.middleware(browserCSRFMiddleware(sessionRenewalMiddleware(authService, httpServer))), webUI)

	application := &App{
		config:            cfg,
		pool:              pool,
		jobs:              jobRuntime,
		events:            eventService,
		telegramDownloads: telegram.downloadClients,
		globalCache:       globalCache,
		http: &http.Server{
			Addr:              cfg.HTTP.Address,
			Handler:           mux,
			ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
			ReadTimeout:       cfg.HTTP.ReadTimeout,
			WriteTimeout:      cfg.HTTP.WriteTimeout,
			IdleTimeout:       cfg.HTTP.IdleTimeout,
		},
	}
	cleanupPool = false
	cleanupTelegramDownloads = false
	cleanupGlobalCache = false
	return application, nil
}

// expandHomePath resolves a configured path that may begin with a tilde. Exactly
// "~" becomes the current user's home directory and "~/sub/path" is joined onto it;
// any other "~user" form is rejected, because resolving another account's home
// directory is not portable. A path without a tilde is returned trimmed of
// surrounding whitespace, so the result is empty only when the input was blank.
func expandHomePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, path[2:]), nil
	}
	if strings.HasPrefix(path, "~") {
		return "", fmt.Errorf("unsupported home-relative path %q", path)
	}
	return path, nil
}

// requestIDMiddleware makes a request ID available to everything downstream. It
// reuses a non-blank X-Request-ID supplied by the caller, which keeps a trace
// intact across a reverse proxy, and mints a UUID otherwise, so the ID is always
// present. The value is stored under middleware.RequestIDKey for the logger and
// echoed back in the X-Request-ID response header.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if requestID == "" {
			requestID = uuid.NewString()
		}
		ctx := context.WithValue(r.Context(), middleware.RequestIDKey, requestID)
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Handler returns the router to serve. A nil receiver or an App without an HTTP
// server yields a handler that answers 503 instead of panicking, so a partially
// wired App cannot crash the process at request time.
func (a *App) Handler() http.Handler {
	if a == nil || a.http == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "application is not configured", http.StatusServiceUnavailable)
		})
	}
	return a.http.Handler
}

// Pool returns the long-lived database pool for callers that need it outside the
// HTTP stack, such as health probes and tests. Ownership stays with the App: the
// pool is closed by Shutdown and must not be closed by the caller. A nil receiver
// returns nil.
func (a *App) Pool() *pgxpool.Pool {
	if a == nil {
		return nil
	}
	return a.pool
}

// Cache returns the process-local cache shared by the services in the graph, so
// callers can invalidate entries those services wrote. As with Pool, the App owns
// the instance and closes it during Shutdown. A nil receiver returns nil, which
// callers must treat as "no cache configured" rather than creating one.
func (a *App) Cache() cache.Cacher {
	if a == nil {
		return nil
	}
	return a.globalCache
}

// Run binds the configured address and owns the full server lifecycle until the
// context is cancelled or the HTTP server fails. It never searches for another
// port, which prevents production deployments from silently becoming unreachable.
func (a *App) Run(ctx context.Context) error {
	if a == nil || a.http == nil || a.jobs == nil || a.events == nil || a.pool == nil {
		return ErrInvalidDependencies
	}
	listener, err := net.Listen("tcp", a.config.HTTP.Address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", a.config.HTTP.Address, err)
	}
	return a.Serve(ctx, listener)
}

// Serve is Run with a caller-provided listener, which makes lifecycle behavior
// deterministic in tests and supports socket activation.
func (a *App) Serve(ctx context.Context, listener net.Listener) error {
	if a == nil || a.http == nil || a.jobs == nil || a.events == nil || a.pool == nil || listener == nil {
		return ErrInvalidDependencies
	}
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return ErrAlreadyRunning
	}
	if a.closed {
		a.mu.Unlock()
		return errors.New("application is closed")
	}
	a.running = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()
	}()

	if err := a.events.Start(ctx); err != nil {
		_ = listener.Close()
		return fmt.Errorf("start event service: %w", err)
	}
	if a.config.Jobs.RunWorkers {
		if err := a.jobs.Start(ctx); err != nil {
			_ = listener.Close()
			closeCtx, cancel := context.WithTimeout(context.Background(), a.config.HTTP.ShutdownTimeout)
			defer cancel()
			return errors.Join(err, a.events.Close(closeCtx))
		}
	}
	a.http.BaseContext = func(net.Listener) context.Context { return ctx }
	serveErrors := make(chan error, 1)
	go func() {
		err := a.http.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErrors <- err
	}()

	select {
	case err := <-serveErrors:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), a.config.HTTP.ShutdownTimeout)
		defer cancel()
		return errors.Join(err, a.Shutdown(shutdownCtx))
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), a.config.HTTP.ShutdownTimeout)
		defer cancel()
		shutdownErr := a.Shutdown(shutdownCtx)
		serveErr := <-serveErrors
		return errors.Join(shutdownErr, serveErr)
	}
}

// Shutdown stops long-lived SSE handlers, closes HTTP and warm Telegram download clients,
// drains RiverPro workers, and finally closes PostgreSQL.
//
// It is safe to call repeatedly and from several goroutines at once. The first call
// performs the release and records its result; every other call waits for that release
// to finish and returns the same error, so a caller never sees nil while the resources
// are still being released. Only the first call's ctx bounds the work: a caller that
// arrives later waits for the release to end rather than reporting a result of its own.
func (a *App) Shutdown(ctx context.Context) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	if !a.closed {
		a.closed = true
		a.shutdownDone = make(chan struct{})
	}
	done := a.shutdownDone
	a.mu.Unlock()

	a.shutdownOnce.Do(func() {
		defer close(done)
		a.shutdownErr = a.releaseResources(ctx)
	})
	<-done
	return a.shutdownErr
}

// releaseResources performs the shutdown sequence in the order Shutdown documents and
// joins every error it collects. Shutdown calls it at most once.
func (a *App) releaseResources(ctx context.Context) error {
	var result error
	if a.events != nil {
		result = errors.Join(result, a.events.Close(ctx))
	}
	if a.http != nil {
		result = errors.Join(result, a.http.Shutdown(ctx))
	}
	if a.telegramDownloads != nil {
		result = errors.Join(result, a.telegramDownloads.Close(ctx))
	}
	if a.jobs != nil {
		if err := a.jobs.Stop(ctx); err != nil && !errors.Is(err, jobs.ErrRuntimeNotConfigured) {
			result = errors.Join(result, err)
		}
	}
	if a.globalCache != nil {
		a.globalCache.Close()
	}
	if a.pool != nil {
		a.pool.Close()
	}
	return result
}

// Close provides a bounded non-request-context shutdown for callers that use
// App as a resource rather than running Serve directly.
func (a *App) Close() error {
	if a == nil {
		return nil
	}
	timeout := a.config.HTTP.ShutdownTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return a.Shutdown(ctx)
}
