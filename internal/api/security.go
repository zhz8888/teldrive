package api

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/tgdrive/teldrive/v2/internal/api/gen"
	"github.com/tgdrive/teldrive/v2/internal/principal"
)

var (
	// ErrUnauthenticated is returned when a request carries no usable credential.
	// The security handlers join it to the underlying authenticator error, so
	// callers must test it with errors.Is; ErrorHandler turns it into a 401.
	ErrUnauthenticated = errors.New("authentication failed")
	// ErrInvalidIdentity is returned when a credential authenticated but resolved
	// to a non-positive user ID, which means it cannot identify anyone.
	ErrInvalidIdentity = errors.New("authenticated identity is invalid")
)

// Identity is the caller identity carried by the request context. It is an alias
// for principal.Identity rather than a distinct type, so the value resolved here
// can be handed to principal.WithIdentity and read back by domain services.
type Identity = principal.Identity

// Authenticator resolves a credential into the identity of the caller.
//
// Implementations receive the raw credential, must treat it as opaque, and must
// report an error rather than an empty identity when it is unknown, expired or
// revoked. One instance is shared by every request, so implementations must be
// safe for concurrent use.
type Authenticator interface {
	// AuthenticateBearer validates an access token issued by the login flow and
	// returns the identity it belongs to. Browser cookie sessions are validated
	// through the same method because their cookie value is an access token.
	AuthenticateBearer(context.Context, string) (Identity, error)
	// AuthenticateAPIKey validates a long-lived external API key and returns the
	// identity it belongs to.
	AuthenticateAPIKey(context.Context, string) (Identity, error)
}

// EventTicketAuthenticator resolves a short-lived event stream ticket into the
// user ID it was issued for. It is optional: NewSecurity only enables ticket
// authentication when an implementation is supplied.
type EventTicketAuthenticator interface {
	// AuthenticateTicket validates an event stream ticket and returns the user ID
	// it belongs to. It returns an error for values that were never issued or are
	// no longer valid.
	AuthenticateTicket(context.Context, string) (int64, error)
}

// Security implements the generated security handler for the credential types in
// the contract: bearer access tokens, external API keys, browser cookies and
// event stream tickets. It only reads its dependencies after construction and is
// safe for concurrent use.
type Security struct {
	// authenticator resolves bearer tokens, cookie values and API keys; nil
	// disables all three of those schemes.
	authenticator Authenticator
	// eventTickets resolves event stream tickets; nil makes HandleEventTicketAuth
	// reject every ticket.
	eventTickets EventTicketAuthenticator
}

// NewSecurity returns a Security backed by authenticator. The event ticket
// authenticator is optional and only the first value is used when several are
// supplied, so callers that do not stream events can omit it.
func NewSecurity(authenticator Authenticator, eventTickets ...EventTicketAuthenticator) *Security {
	security := &Security{authenticator: authenticator}
	if len(eventTickets) > 0 {
		security.eventTickets = eventTickets[0]
	}
	return security
}

// IdentityFromContext returns the identity stored in ctx and reports whether a
// usable one is present. A missing identity is not an error, so each caller
// decides whether the operation requires authentication.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	return principal.FromContext(ctx)
}

// UserIDFromContext returns the Telegram user ID of the authenticated caller. It
// returns ErrUnauthenticated when the context carries no usable identity, which
// mapServiceError maps to a 401 response.
func UserIDFromContext(ctx context.Context) (int64, error) {
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		return 0, ErrUnauthenticated
	}
	return identity.UserID, nil
}

// HasRole reports whether the authenticated caller holds the named role. It
// returns false for unauthenticated callers and for identities that carry no
// roles, such as event ticket identities.
func HasRole(ctx context.Context, role string) bool {
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		return false
	}
	return slices.Contains(identity.Roles, role)
}

// HasAdminRole reports whether the authenticated caller holds the "admin" or
// "owner" role.
func HasAdminRole(ctx context.Context) bool {
	return HasRole(ctx, "admin") || HasRole(ctx, "owner")
}

// HandleBearerAuth authenticates the bearer token of the request and stores the
// resulting identity in the returned context for downstream handlers.
//
// It returns ErrUnauthenticated when the token is blank or rejected, and
// ErrInvalidIdentity when the credential resolves to no usable user. As a side
// effect it copies the resolved roles back into auth, which is how the generated
// layer enforces any per-operation scopes.
func (s *Security) HandleBearerAuth(ctx context.Context, _ gen.OperationName, auth gen.BearerAuth) (context.Context, error) {
	if s == nil || s.authenticator == nil || strings.TrimSpace(auth.Token) == "" {
		return ctx, ErrUnauthenticated
	}
	identity, err := s.authenticator.AuthenticateBearer(ctx, auth.Token)
	if err != nil {
		return ctx, errors.Join(ErrUnauthenticated, err)
	}
	if identity.UserID <= 0 {
		return ctx, ErrInvalidIdentity
	}
	auth.Roles = append([]string(nil), identity.Roles...)
	return principal.WithIdentity(ctx, identity), nil
}

// HandleExternalApiKeyAuth authenticates the caller's external API key and stores
// the resulting identity in the returned context. Failures follow
// HandleBearerAuth: ErrUnauthenticated for a blank or rejected key, and
// ErrInvalidIdentity when the key resolves to no usable user.
func (s *Security) HandleExternalApiKeyAuth(ctx context.Context, _ gen.OperationName, auth gen.ExternalApiKeyAuth) (context.Context, error) {
	if s == nil || s.authenticator == nil || strings.TrimSpace(auth.APIKey) == "" {
		return ctx, ErrUnauthenticated
	}
	identity, err := s.authenticator.AuthenticateAPIKey(ctx, auth.APIKey)
	if err != nil {
		return ctx, errors.Join(ErrUnauthenticated, err)
	}
	if identity.UserID <= 0 {
		return ctx, ErrInvalidIdentity
	}
	auth.Roles = append([]string(nil), identity.Roles...)
	return principal.WithIdentity(ctx, identity), nil
}

// HandleCookieAuth authenticates the browser session cookie and stores the
// resulting identity in the returned context. The cookie value is validated
// through the bearer-token path, so the two credential forms share one session
// lookup and one set of failure modes.
func (s *Security) HandleCookieAuth(ctx context.Context, _ gen.OperationName, auth gen.CookieAuth) (context.Context, error) {
	if s == nil || s.authenticator == nil || strings.TrimSpace(auth.APIKey) == "" {
		return ctx, ErrUnauthenticated
	}
	identity, err := s.authenticator.AuthenticateBearer(ctx, auth.APIKey)
	if err != nil {
		return ctx, errors.Join(ErrUnauthenticated, err)
	}
	if identity.UserID <= 0 {
		return ctx, ErrInvalidIdentity
	}
	auth.Roles = append([]string(nil), identity.Roles...)
	return principal.WithIdentity(ctx, identity), nil
}

// HandleEventTicketAuth authenticates a short-lived event stream ticket. The
// identity it stores carries the user ID and the source "event_ticket" but no
// session or roles, so a ticket-authenticated request can only reach operations
// that do not check roles.
func (s *Security) HandleEventTicketAuth(ctx context.Context, _ gen.OperationName, auth gen.EventTicketAuth) (context.Context, error) {
	if s == nil || s.eventTickets == nil || strings.TrimSpace(auth.APIKey) == "" {
		return ctx, ErrUnauthenticated
	}
	userID, err := s.eventTickets.AuthenticateTicket(ctx, auth.APIKey)
	if err != nil {
		return ctx, errors.Join(ErrUnauthenticated, err)
	}
	if userID <= 0 {
		return ctx, ErrInvalidIdentity
	}
	return principal.WithIdentity(ctx, principal.Identity{UserID: userID, Source: "event_ticket"}), nil
}

var _ gen.SecurityHandler = (*Security)(nil)
