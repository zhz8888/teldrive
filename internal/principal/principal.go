// Package principal carries the authenticated caller identity through the
// request context.
//
// Authentication middleware resolves a credential into an Identity and stores
// it with WithIdentity; downstream services read it back with FromContext
// instead of re-parsing tokens or querying the session table again.
package principal

import (
	"context"

	"github.com/google/uuid"
)

// Identity describes who is performing the current request.
//
// It is a value type copied into the context, so it must stay cheap to copy and
// must never hold a database handle or other resource. Credentials themselves
// are deliberately absent: only the resolved identifiers are carried.
type Identity struct {
	// UserID is the Telegram user ID of the caller. Because the zero value is
	// indistinguishable from "absent", FromContext only reports an identity when
	// this field is greater than zero.
	UserID int64

	// SessionID identifies the login session the credential was issued for. It is
	// the zero UUID for credentials that are not tied to a session, such as an
	// event ticket.
	SessionID uuid.UUID

	// Roles lists the role names granted to this caller, for example "admin" or
	// "owner". Callers should test membership through the helpers in internal/api
	// rather than by reading this slice directly.
	Roles []string

	// Source records which credential produced the identity, for example
	// "bearer", "api_key" or "event_ticket". It exists for auditing and
	// diagnostics and must not be used for authorisation decisions.
	Source string
}

// contextKey is the unexported context key type that keeps identity values in a
// namespace no other package can address.
type contextKey struct{}

// WithIdentity returns a copy of ctx carrying identity. A later call shadows an
// earlier one for the same key, so middleware should call it only once the
// request is fully authenticated.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, identity)
}

// FromContext returns the identity stored by WithIdentity and reports whether a
// usable one was present. The boolean is false when the context carries no
// identity or when UserID is not positive, which lets callers treat "missing"
// and "zero user" identically.
func FromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(contextKey{}).(Identity)
	return identity, ok && identity.UserID > 0
}
