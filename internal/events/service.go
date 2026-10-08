package events

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
)

var (
	// ErrInvalidCursor is returned when the client's cursor is ahead of the
	// newest event id, which means it was never valid for this user. Callers must
	// test it with errors.Is; the HTTP layer maps it to 422 Unprocessable Entity.
	ErrInvalidCursor = errors.New("invalid event stream cursor")
	// ErrInvalidTicket is returned by AuthenticateTicket for a blank, unknown,
	// expired or already deleted ticket. The security layer turns it into an
	// unauthenticated request.
	ErrInvalidTicket = errors.New("invalid or expired event stream ticket")
)

// Config holds the tunables of Service. The zero value of every field means
// "use the built-in default" and is replaced by withDefaults, so a caller only
// has to avoid negative and out-of-range values.
type Config struct {
	// BatchSize is the maximum number of events one ListAfter call returns; it
	// must be between 1 and 1000.
	BatchSize int32
	// MaxConnectionsPerUser caps the concurrent SSE streams a single user may
	// hold; it must be between 1 and 1000.
	MaxConnectionsPerUser int
	// Heartbeat is how long the SSE handler may stay silent before it polls for
	// new events and writes a keep-alive comment.
	Heartbeat time.Duration
	// WriteTimeout bounds a single write to a slow SSE client.
	WriteTimeout time.Duration
	// TicketTTL is how long an issued event stream ticket keeps authenticating.
	TicketTTL time.Duration
	// CleanupInterval is how often expired event stream tickets are deleted.
	CleanupInterval time.Duration
	// ConnectTimeout bounds one listener connect attempt and each ticket cleanup
	// query.
	ConnectTimeout time.Duration
	// PingInterval is how long the listener may idle on a blocking read before it
	// pings PostgreSQL to prove the connection is still alive.
	PingInterval time.Duration
	// ReconnectMin is the delay before the listener's first reconnect attempt.
	ReconnectMin time.Duration
	// ReconnectMax caps the listener's exponential reconnect backoff and must not
	// be smaller than ReconnectMin.
	ReconnectMax time.Duration
}

// Event is one persisted row of the user event log, as returned by ListAfter and
// rendered by the SSE handler as a single event frame. The durable row, not the
// wake-up notification, is what clients are guaranteed to see.
type Event struct {
	// ID is the monotonically increasing event id assigned by the database and
	// doubles as the stream cursor clients send back.
	ID int64
	// UserID is the owner of the event; a stream only ever reads its own user's
	// rows.
	UserID int64
	// Type is the event name written to the SSE event field, for example
	// file.created.
	Type string
	// ResourceType names the kind of resource the event is about, for example
	// file or upload.
	ResourceType string
	// ResourceID identifies that resource and is empty for events that do not
	// target a single resource.
	ResourceID string
	// Generation is the resource generation after the change when the event
	// tracks one; nil means the event carries no generation.
	Generation *int64
	// Payload is the event-specific JSON object copied from the jsonb column. It
	// is always a JSON object and never aliases the driver's row buffer.
	Payload []byte
	// OccurredAt is when the event was stored, normalized to UTC.
	OccurredAt time.Time
}

// Ticket is a short-lived bearer credential that lets a browser EventSource,
// which cannot set an Authorization header, authenticate the event stream
// through a query parameter. Only the SHA-256 hash of Value is stored, so the
// database never holds anything usable as a credential.
type Ticket struct {
	// Value is the base64url random secret handed to the client; it cannot be
	// recovered from the stored hash.
	Value string
	// ExpiresAt is when the ticket stops authenticating, in UTC.
	ExpiresAt time.Time
}

// Service is the entry point of the event subsystem. It owns the hub, the
// LISTEN listener and the ticket store, and exposes the reads the SSE handler
// needs. Build it with NewService, start it with Start and stop it with Close;
// the read methods are safe for concurrent use.
type Service struct {
	// queries reads persisted events and manages stream tickets.
	queries *sqlcgen.Queries
	// hub fans wake-ups out to the local subscribers of a user.
	hub *Hub
	// listener owns the dedicated LISTEN connection.
	listener *Listener
	// logger reports listener trouble and cleanup failures; never nil after
	// NewService.
	logger *slog.Logger
	// config is the validated configuration with defaults already applied.
	config Config

	// mu guards cancel, done, running and closed.
	mu sync.Mutex
	// startMu serializes Start so that at most one listener connect is ever in
	// flight and a caller that races another Start either observes the running
	// service or performs the connect itself. It is held across the connect;
	// Close never takes it, so shutdown can never queue behind a slow connect.
	// It is never acquired while mu is held.
	startMu sync.Mutex
	// cancel stops the ticket cleanup goroutine; nil while the service is
	// stopped.
	cancel context.CancelFunc
	// done is closed when the cleanup goroutine returns; NewService and Done keep
	// it closed until the first Start.
	done chan struct{}
	// running reports whether the cleanup goroutine is active.
	running bool
	// listenerActive reports whether the listener was started and not yet closed
	// from Close. It is separate from running because the cleanup goroutine can
	// stop, when the context Start received is cancelled, while the listener is
	// still up; Close has to stop the listener in that case instead of returning
	// with the dedicated connection open.
	listenerActive bool
	// closed records that Close ran; it is final, so Start and IssueTicket refuse
	// further work.
	closed bool
}

// NewService builds an event service around pool. It applies the defaults to
// cfg, validates the result and creates the hub and the LISTEN listener, but
// connects nothing yet: the service stays idle until Start. It returns an error
// for a nil pool or an invalid configuration.
func NewService(pool *pgxpool.Pool, logger *slog.Logger, cfg Config) (*Service, error) {
	if pool == nil {
		return nil, errors.New("event service requires a database pool")
	}
	if logger == nil {
		logger = slog.Default()
	}
	cfg = withDefaults(cfg)
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	hub := NewHub(cfg.MaxConnectionsPerUser)
	done := make(chan struct{})
	close(done)
	return &Service{
		queries: sqlcgen.New(pool),
		hub:     hub,
		listener: newListener(pool, hub, logger, listenerConfig{
			ConnectTimeout: cfg.ConnectTimeout,
			PingInterval:   cfg.PingInterval,
			ReconnectMin:   cfg.ReconnectMin,
			ReconnectMax:   cfg.ReconnectMax,
		}),
		logger: logger,
		config: cfg,
		done:   done,
	}, nil
}

// withDefaults returns a copy of cfg in which every zero field is replaced by
// the built-in default. Callers therefore cannot request a zero value, which is
// why validateConfig only has to reject negative and out-of-range settings.
func withDefaults(cfg Config) Config {
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 100
	}
	if cfg.MaxConnectionsPerUser == 0 {
		cfg.MaxConnectionsPerUser = 5
	}
	if cfg.Heartbeat == 0 {
		cfg.Heartbeat = 20 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 10 * time.Second
	}
	if cfg.TicketTTL == 0 {
		cfg.TicketTTL = 2 * time.Minute
	}
	if cfg.CleanupInterval == 0 {
		cfg.CleanupInterval = time.Hour
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	if cfg.PingInterval == 0 {
		cfg.PingInterval = 5 * time.Second
	}
	if cfg.ReconnectMin == 0 {
		cfg.ReconnectMin = 100 * time.Millisecond
	}
	if cfg.ReconnectMax == 0 {
		cfg.ReconnectMax = 30 * time.Second
	}
	return cfg
}

// validateConfig reports the first setting the service cannot honour: a batch
// size or per-user connection cap outside 1..1000, a non-positive duration, or
// reconnect bounds whose maximum is below their minimum. Every message names the
// offending setting, and a nil result means cfg is usable.
func validateConfig(cfg Config) error {
	switch {
	case cfg.BatchSize < 1 || cfg.BatchSize > 1000:
		return errors.New("event batch size must be between 1 and 1000")
	case cfg.MaxConnectionsPerUser < 1 || cfg.MaxConnectionsPerUser > 1000:
		return errors.New("event connections per user must be between 1 and 1000")
	case cfg.Heartbeat <= 0:
		return errors.New("event heartbeat must be positive")
	case cfg.WriteTimeout <= 0:
		return errors.New("event write timeout must be positive")
	case cfg.TicketTTL <= 0:
		return errors.New("event ticket TTL must be positive")
	case cfg.CleanupInterval <= 0:
		return errors.New("event cleanup interval must be positive")
	case cfg.ConnectTimeout <= 0:
		return errors.New("event listener connect timeout must be positive")
	case cfg.PingInterval <= 0:
		return errors.New("event listener ping interval must be positive")
	case cfg.ReconnectMin <= 0 || cfg.ReconnectMax < cfg.ReconnectMin:
		return errors.New("event listener reconnect bounds are invalid")
	default:
		return nil
	}
}

// Start launches the listener and the periodic ticket cleanup under ctx. It is
// idempotent while running and returns ErrServiceClosed after Close. The
// listener connects synchronously, so a connection failure is reported here and
// the service stays stopped; cancelling ctx stops both goroutines and Close
// waits for them. The connect runs outside the state lock, so Done and Close are
// never blocked by it: concurrent Start calls are serialized instead, and a
// Close that lands while this call is connecting is noticed afterwards, in which
// case Start stops the fresh listener again and reports ErrServiceClosed rather
// than leaking its dedicated connection.
func (s *Service) Start(ctx context.Context) error {
	if s == nil || s.listener == nil {
		return errors.New("event service is not configured")
	}

	s.startMu.Lock()
	defer s.startMu.Unlock()

	s.mu.Lock()
	closed, running := s.closed, s.running
	s.mu.Unlock()
	if closed {
		return ErrServiceClosed
	}
	if running {
		return nil
	}

	serviceCtx, cancel := context.WithCancel(ctx)
	if err := s.listener.Start(serviceCtx); err != nil {
		cancel()
		return err
	}

	s.mu.Lock()
	if s.closed {
		// Close ran while this call was connecting and saw nothing running, so
		// it left the listener alone; stop it here instead of leaking the
		// dedicated connection. The run goroutine terminates on its own once
		// serviceCtx is cancelled even if ctx is already done and Close returns
		// early.
		s.mu.Unlock()
		cancel()
		_ = s.listener.Close(ctx)
		return ErrServiceClosed
	}
	s.cancel = cancel
	s.running = true
	s.listenerActive = true
	s.done = make(chan struct{})
	go s.runTicketCleanup(serviceCtx, s.done)
	s.mu.Unlock()
	return nil
}

// Close stops the listener and the ticket cleanup, closes the hub and waits for
// the cleanup goroutine to exit. It is idempotent and safe on a nil service. A
// Close that lands while a concurrent Start is still connecting leaves that
// listener to Start, which stops it and reports ErrServiceClosed. Closing the hub
// ends every in-flight SSE stream, and if ctx expires before the goroutine stops,
// the listener error is joined with ctx.Err() and returned.
func (s *Service) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if !s.running {
		listenerActive := s.listenerActive
		s.listenerActive = false
		s.mu.Unlock()
		s.hub.Close()
		if listenerActive {
			// The cleanup goroutine is already gone, which happens when the
			// context Start received was cancelled, but the listener was never
			// stopped from here. Closing it keeps shutdown from returning while
			// the dedicated LISTEN connection is still open.
			return s.listener.Close(ctx)
		}
		return nil
	}
	cancel, done := s.cancel, s.done
	s.listenerActive = false
	s.mu.Unlock()

	cancel()
	s.hub.Close()
	listenerErr := s.listener.Close(ctx)
	select {
	case <-done:
		return listenerErr
	case <-ctx.Done():
		return errors.Join(listenerErr, ctx.Err())
	}
}

// Done returns the channel that is closed once the ticket cleanup goroutine has
// stopped. It is already closed before the first Start and closes as the service
// shuts down, so a select on it works as a shutdown signal without extra
// bookkeeping; a nil service returns a closed channel too.
func (s *Service) Done() <-chan struct{} {
	if s == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// Subscribe returns the wake-up channel for userID together with the function
// that unregisters it; see Hub.Subscribe for the buffering, closing and
// unsubscribe semantics. It returns ErrServiceClosed for a nil service, so a
// caller can treat any subscription failure as "the stream cannot start".
func (s *Service) Subscribe(userID int64) (<-chan struct{}, func(), error) {
	if s == nil || s.hub == nil {
		return nil, nil, ErrServiceClosed
	}
	return s.hub.Subscribe(userID)
}

// BatchSize returns the configured number of events one ListAfter call may
// return, falling back to the package default for a nil service.
func (s *Service) BatchSize() int32 {
	if s == nil {
		return 100
	}
	return s.config.BatchSize
}

// Heartbeat returns the configured SSE keep-alive interval, falling back to the
// package default for a nil service.
func (s *Service) Heartbeat() time.Duration {
	if s == nil {
		return 20 * time.Second
	}
	return s.config.Heartbeat
}

// WriteTimeout returns the configured deadline for a single SSE write, falling
// back to the package default for a nil service.
func (s *Service) WriteTimeout() time.Duration {
	if s == nil {
		return 10 * time.Second
	}
	return s.config.WriteTimeout
}

// ListAfter returns up to BatchSize events of userID with an id greater than
// afterID, in ascending id order, so the caller advances its cursor to the id of
// the last event it received. An empty eventTypes slice selects every type and a
// non-empty one restricts the result to those exact names. An empty result with
// a nil error means the user has nothing new. afterID zero starts at the beginning
// of the retained log, and events already deleted by retention are skipped
// silently, which CursorExpired detects for the caller. A stored row that fails
// validation fails the whole call instead of being dropped.
func (s *Service) ListAfter(ctx context.Context, userID, afterID int64, eventTypes []string) ([]Event, error) {
	if s == nil || s.queries == nil || userID <= 0 || afterID < 0 {
		return nil, errors.New("invalid event list request")
	}
	rows, err := s.queries.ListUserEventsAfter(ctx, sqlcgen.ListUserEventsAfterParams{
		UserID: userID, AfterID: afterID, EventTypes: eventTypes, EventLimit: s.config.BatchSize,
	})
	if err != nil {
		return nil, fmt.Errorf("list user events: %w", err)
	}
	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		event, err := eventFromRow(row)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

// CurrentCursor returns the newest event ID known for the user.
func (s *Service) CurrentCursor(ctx context.Context, userID int64) (int64, error) {
	if s == nil || s.queries == nil || userID <= 0 {
		return 0, errors.New("invalid event cursor request")
	}
	state, err := s.queries.GetUserEventCursorState(ctx, sqlcgen.GetUserEventCursorStateParams{
		CursorUserID: userID,
		AfterID:      0,
	})
	if err != nil {
		return 0, fmt.Errorf("get current event cursor: %w", err)
	}
	return state.LastEventID, nil
}

// CursorExpired reports whether the client's cursor has fallen out of the
// retained window: the event with that id no longer exists while newer events
// are available, so the gap can never be replayed. A non-positive user id is
// rejected as an invalid request, exactly as ListAfter and CurrentCursor reject
// it. A non-positive cursor is treated as not expired, a cursor ahead of the
// newest event yields ErrInvalidCursor, and a failed lookup returns a wrapped
// error. The SSE handler turns a true result into a sync.required control frame.
func (s *Service) CursorExpired(ctx context.Context, userID, afterID int64) (bool, error) {
	if s == nil || s.queries == nil || userID <= 0 {
		return false, errors.New("invalid event cursor request")
	}
	if afterID <= 0 {
		return false, nil
	}
	state, err := s.queries.GetUserEventCursorState(ctx, sqlcgen.GetUserEventCursorStateParams{
		CursorUserID: userID,
		AfterID:      afterID,
	})
	if err != nil {
		return false, fmt.Errorf("get event cursor state: %w", err)
	}
	if afterID > state.LastEventID {
		return false, ErrInvalidCursor
	}
	return !state.CursorExists && state.LastEventID > afterID, nil
}

// IssueTicket creates a stream ticket for userID and returns its plaintext value
// and expiry; only the SHA-256 hash is persisted, so a value that is lost cannot
// be recovered. The ticket keeps authenticating any number of requests until it
// expires and is only removed by the periodic cleanup, so callers must treat the
// value as a bearer secret. It returns ErrServiceClosed after shutdown, and the
// random source or the insert can fail the call too.
func (s *Service) IssueTicket(ctx context.Context, userID int64) (Ticket, error) {
	if s == nil || s.queries == nil || userID <= 0 {
		return Ticket{}, errors.New("invalid event ticket request")
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return Ticket{}, ErrServiceClosed
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Ticket{}, fmt.Errorf("generate event stream ticket: %w", err)
	}
	value := base64.RawURLEncoding.EncodeToString(raw)
	expiresAt := time.Now().UTC().Add(s.config.TicketTTL)
	hash := sha256.Sum256([]byte(value))
	if err := s.queries.CreateEventStreamTicket(ctx, sqlcgen.CreateEventStreamTicketParams{
		TokenHash: hash[:],
		UserID:    userID,
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); err != nil {
		return Ticket{}, fmt.Errorf("store event stream ticket: %w", err)
	}
	return Ticket{Value: value, ExpiresAt: expiresAt}, nil
}

// AuthenticateTicket resolves a ticket value to the user it was issued for. It
// returns ErrInvalidTicket for a blank or unknown value, for a ticket past its
// expiry and for a stored row whose user id is not positive, and a wrapped error
// only when the lookup itself fails.
func (s *Service) AuthenticateTicket(ctx context.Context, value string) (int64, error) {
	if s == nil || s.queries == nil || strings.TrimSpace(value) == "" {
		return 0, ErrInvalidTicket
	}
	hash := sha256.Sum256([]byte(value))
	userID, err := s.queries.GetEventStreamTicketUser(ctx, hash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrInvalidTicket
	}
	if err != nil {
		return 0, fmt.Errorf("authenticate event stream ticket: %w", err)
	}
	if userID <= 0 {
		return 0, ErrInvalidTicket
	}
	return userID, nil
}

// runTicketCleanup deletes expired event stream tickets every CleanupInterval
// until ctx is cancelled. Each sweep runs under its own ConnectTimeout, and a
// failure is logged instead of stopping the loop, so the goroutine only exits
// through ctx; on the way out it clears the running flag and closes the done
// channel Start handed out, which is what Close waits for.
func (s *Service) runTicketCleanup(ctx context.Context, done chan struct{}) {
	defer func() {
		s.mu.Lock()
		s.running = false
		s.cancel = nil
		close(done)
		s.mu.Unlock()
	}()

	ticker := time.NewTicker(s.config.CleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanupCtx, cancel := context.WithTimeout(ctx, s.config.ConnectTimeout)
			if _, err := s.queries.DeleteExpiredEventStreamTickets(cleanupCtx); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.ErrorContext(ctx, "delete expired event stream tickets", "error", err)
			}
			cancel()
		}
	}
}

// eventFromRow converts one stored user_events row into an Event, copying the
// payload so the result does not alias the driver's row buffer. It rejects rows
// without a positive id or user, or without a timestamp, and maps a null
// resource_id to an empty string and a null generation to a nil pointer.
func eventFromRow(row *sqlcgen.UserEvent) (Event, error) {
	if row == nil || row.ID <= 0 || row.UserID <= 0 || !row.OccurredAt.Valid {
		return Event{}, errors.New("invalid stored user event")
	}
	event := Event{
		ID:           row.ID,
		UserID:       row.UserID,
		Type:         row.EventType,
		ResourceType: row.ResourceType,
		Payload:      append([]byte(nil), row.Payload...),
		OccurredAt:   row.OccurredAt.Time.UTC(),
	}
	if row.ResourceID.Valid {
		event.ResourceID = row.ResourceID.String
	}
	if row.Generation.Valid {
		generation := row.Generation.Int64
		event.Generation = &generation
	}
	return event, nil
}
