// Package health answers process liveness and dependency readiness probes for
// the unauthenticated health endpoints. It owns no state that outlives a request
// and caches no probe result, so every call reports the state observed at that
// moment rather than a value sampled at startup.
package health

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotConfigured reports that readiness was probed without the dependency the probe
// needs, which is a configuration error rather than a failing dependency. Ready
// returns it for a Service constructed with a nil Pinger, so a caller can use
// errors.Is to tell "the database was never wired up" from "the database did not
// answer" instead of reporting one degraded state for both.
var ErrNotConfigured = errors.New("database is not configured")

// Pinger is the readiness dependency of a Service. *pgxpool.Pool satisfies it,
// but the interface keeps the probe testable without a real database.
type Pinger interface {
	// Ping reports whether the dependency is reachable and usable. It blocks
	// until the dependency answers or ctx is done, and returns the underlying
	// connection error otherwise. Implementations are called from concurrent
	// requests and must be safe for concurrent use.
	Ping(context.Context) error
}

// Status is the probe result returned by Live and Ready. State is "ok" when the
// probe passed and "degraded" when a dependency failed or was never configured;
// Version is always the build version the Service was constructed with, so a caller
// that reaches a misconfigured instance can still tell which process answered.
type Status struct {
	// State is the machine-readable probe state, currently "ok" or "degraded".
	State string
	// Version is the server build version, possibly empty when the binary was
	// built without version information.
	Version string
}

// Service answers liveness and readiness probes for one process. It is immutable
// after construction and therefore safe for concurrent use.
type Service struct {
	// version is stamped into every Status; it is not validated, so the empty
	// string is a legal build version.
	version string
	// db is the readiness dependency. It may be nil, which makes Ready report a
	// degraded status carrying ErrNotConfigured instead of panicking, mirroring an
	// unwired dependency.
	db Pinger
}

// NewService returns a health service that stamps version into every status and
// probes db for readiness. A nil db is accepted and surfaced as a degraded readiness
// result carrying ErrNotConfigured; the caller keeps ownership of db, which is never
// closed here.
func NewService(version string, db Pinger) *Service {
	return &Service{version: version, db: db}
}

// Live reports liveness without touching any dependency, so it keeps answering
// "ok" while the database is unreachable and a restart would not help. It never
// fails: the process that can execute it is by definition alive.
func (s *Service) Live() Status {
	return Status{State: "ok", Version: s.version}
}

// Ready probes the readiness dependency and returns "ok" only when the ping
// succeeds. A missing dependency yields a "degraded" Status with ErrNotConfigured,
// and a failed ping one wrapping the cause, so a caller can tell a configuration gap
// from a database that is down; the Status is still returned on every path so the
// version remains available.
func (s *Service) Ready(ctx context.Context) (Status, error) {
	if s.db == nil {
		return Status{State: "degraded", Version: s.version}, ErrNotConfigured
	}
	if err := s.db.Ping(ctx); err != nil {
		return Status{State: "degraded", Version: s.version}, fmt.Errorf("ping database: %w", err)
	}
	return Status{State: "ok", Version: s.version}, nil
}
