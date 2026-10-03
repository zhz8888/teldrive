package telegramstore

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/contrib/middleware/ratelimit"
	"github.com/gotd/log"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/net/proxy"
	"golang.org/x/time/rate"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
	"github.com/tgdrive/teldrive/v2/internal/principal"
	"github.com/tgdrive/teldrive/v2/internal/secureblob"
	"github.com/tgdrive/teldrive/v2/internal/telethonsession"
)

// ErrTelegramConfiguration reports missing or contradictory gotd client
// configuration, such as an absent application ID or hash, a negative retry
// count, an incomplete rate limit, or a proxy combination that cannot be used.
// Callers must test it with errors.Is.
var ErrTelegramConfiguration = errors.New("Telegram client factory is not configured")

const (
	// maxFloodWait bounds how long one request may sleep inside the gotd
	// flood-wait waiter. Telegram can answer FLOOD_WAIT with hours for a login
	// attempt, and the waiter sleeps inline while the caller's request, its
	// database connection and its goroutine stay occupied, so an unbounded wait
	// turns a throttled account into a stalled server. A longer wait is reported
	// to the caller as an error instead.
	maxFloodWait = 2 * time.Minute
	// maxFloodWaitRetries is how many FLOOD_WAIT answers one request follows
	// before giving up, which keeps the total sleep below a few minutes.
	maxFloodWaitRetries = 2
)

// FactoryConfig is the gotd client configuration taken from the application
// config. AppID and AppHash are the only mandatory fields: NewFactory applies
// the defaults described below and then validates the result, so a zero valued
// timing or device field is not an error by itself.
type FactoryConfig struct {
	// AppID is the Telegram application ID and must be positive.
	AppID int
	// AppHash is the Telegram application hash and must not be blank.
	AppHash string
	// Device is the device identity the client announces to Telegram.
	// NewFactory substitutes "TelDrive Backend v2", "2", and "Server" for a
	// blank SystemVersion, AppVersion, and DeviceModel; the language fields are
	// passed through as configured.
	Device telegram.DeviceConfig
	// DialTimeout bounds one Telegram connection attempt and each HTTP proxy
	// tunnel setup. Values at or below zero become 15 seconds.
	DialTimeout time.Duration
	// ReconnectTimeout caps the total exponential reconnect backoff, which grows
	// by a factor of 1.1 with at most 10 seconds between attempts. Values at or
	// below zero become 5 minutes.
	ReconnectTimeout time.Duration
	// MaxRetries is both the transport retry count of the gotd client and the
	// number of immediate retries of a transient RPC error per invocation. Zero
	// disables retrying and a negative value is rejected.
	MaxRetries int
	// RateLimit enables the client side rate limiter, which spaces requests by
	// RateInterval and allows bursts of RateBurst.
	RateLimit bool
	// RateInterval is the minimum interval between requests while rate limiting
	// is enabled and must then be positive; it is ignored otherwise.
	RateInterval time.Duration
	// RateBurst is the burst allowance of the rate limiter and must be at least
	// one while rate limiting is enabled; it is ignored otherwise.
	RateBurst int
	// Proxy is an HTTP, HTTPS, or SOCKS5 proxy URL. A blank value connects
	// directly, an http or https URL tunnels through CONNECT, and any other
	// supported scheme is dialed by golang.org/x/net/proxy.
	Proxy string
	// MTProxyAddress is the MTProto proxy address in host:port form. It must be
	// set together with MTProxySecret and cannot be combined with Proxy.
	MTProxyAddress string
	// MTProxySecret is the MTProto proxy secret in hexadecimal form; it must
	// decode as hex.
	MTProxySecret string
	// Logger receives gotd client logs. A nil logger disables client logging.
	Logger log.Logger
}

// Factory creates unstarted gotd clients. Each caller must execute the client
// with Client.Run for exactly one request-scoped operation.
type Factory struct {
	// config is the configuration with defaults applied; newClient reads the
	// client options from it and AppCredentials the application credentials.
	config FactoryConfig
	// resolver decides which Telegram data centers the client dials and carries
	// the configured proxy.
	resolver dcs.Resolver
	// middleware is the shared client middleware chain, outermost first: flood
	// wait, transient retry, and optionally rate limiting. Every client and every
	// connection pool built by the factory wraps its own copy of this slice.
	middleware []telegram.Middleware
}

// NewFactory validates config, applies the documented defaults, and builds the
// middleware chain that every client created afterwards will share. It returns
// ErrTelegramConfiguration (wrapped) when the credentials are missing, the
// retry count is negative, the rate limit is enabled without a positive
// interval and burst, or the proxy settings are incomplete or unusable.
func NewFactory(config FactoryConfig) (*Factory, error) {
	if config.AppID <= 0 || strings.TrimSpace(config.AppHash) == "" {
		return nil, ErrTelegramConfiguration
	}
	if config.DialTimeout <= 0 {
		config.DialTimeout = 15 * time.Second
	}
	if config.ReconnectTimeout <= 0 {
		config.ReconnectTimeout = 5 * time.Minute
	}
	if config.MaxRetries < 0 {
		return nil, fmt.Errorf("%w: max retries cannot be negative", ErrTelegramConfiguration)
	}
	if config.Device.SystemVersion == "" {
		config.Device.SystemVersion = "TelDrive Backend v2"
	}
	if config.Device.AppVersion == "" {
		config.Device.AppVersion = "2"
	}
	if config.Device.DeviceModel == "" {
		config.Device.DeviceModel = "Server"
	}
	resolver, err := resolverFromConfig(config)
	if err != nil {
		return nil, err
	}
	retry, err := newRetryMiddleware(config.MaxRetries)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTelegramConfiguration, err)
	}
	middlewares := []telegram.Middleware{
		floodwait.NewSimpleWaiter().WithMaxRetries(maxFloodWaitRetries).WithMaxWait(maxFloodWait),
		retry,
	}
	if config.RateLimit {
		if config.RateInterval <= 0 || config.RateBurst < 1 {
			return nil, fmt.Errorf("%w: rate interval and burst must be positive", ErrTelegramConfiguration)
		}
		middlewares = append(middlewares, ratelimit.New(rate.Every(config.RateInterval), config.RateBurst))
	}
	return &Factory{config: config, resolver: resolver, middleware: middlewares}, nil
}

// resolverFromConfig turns the proxy settings into a data center resolver: an
// MTProto resolver when an MTProxy is configured, otherwise a plain resolver
// whose dialer tunnels through the HTTP(S) proxy, dials through the SOCKS5
// proxy, or connects directly. Address and secret must be configured together
// and an MTProxy cannot be combined with Proxy; every rejection is reported as
// ErrTelegramConfiguration (wrapped).
func resolverFromConfig(config FactoryConfig) (dcs.Resolver, error) {
	proxyURL := strings.TrimSpace(config.Proxy)
	mtAddress := strings.TrimSpace(config.MTProxyAddress)
	mtSecret := strings.TrimSpace(config.MTProxySecret)
	if (mtAddress == "") != (mtSecret == "") {
		return nil, fmt.Errorf("%w: MTProxy address and secret must be configured together", ErrTelegramConfiguration)
	}
	if proxyURL != "" && mtAddress != "" {
		return nil, fmt.Errorf("%w: proxy and MTProxy cannot be used together", ErrTelegramConfiguration)
	}
	if mtAddress != "" {
		secret, err := hex.DecodeString(mtSecret)
		if err != nil {
			return nil, fmt.Errorf("%w: decode MTProxy secret: %v", ErrTelegramConfiguration, err)
		}
		resolver, err := dcs.MTProxy(mtAddress, secret, dcs.MTProxyOptions{})
		if err != nil {
			return nil, fmt.Errorf("%w: create MTProxy resolver: %v", ErrTelegramConfiguration, err)
		}
		return resolver, nil
	}

	var dialer proxy.ContextDialer = proxy.Direct
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("%w: parse proxy URL: %v", ErrTelegramConfiguration, err)
		}
		if parsed.Scheme == "http" || parsed.Scheme == "https" {
			dialer, err = newHTTPConnectDialer(parsed, config.DialTimeout)
			if err != nil {
				return nil, fmt.Errorf("%w: create HTTP proxy dialer: %v", ErrTelegramConfiguration, err)
			}
		} else {
			created, err := proxy.FromURL(parsed, proxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("%w: create proxy dialer: %v", ErrTelegramConfiguration, err)
			}
			contextDialer, ok := created.(proxy.ContextDialer)
			if !ok {
				return nil, fmt.Errorf("%w: proxy dialer does not support context cancellation", ErrTelegramConfiguration)
			}
			dialer = contextDialer
		}
	}
	return dcs.Plain(dcs.PlainOptions{Dial: dialer.DialContext}), nil
}

// New returns an unstarted gotd client that keeps its session in storage and
// ignores Telegram updates. A nil factory, missing credentials, or a nil
// storage returns ErrTelegramConfiguration; the caller owns Client.Run and
// must run the client exactly once, because Client.Run is not reentrant.
func (f *Factory) New(storage telegram.SessionStorage) (*telegram.Client, error) {
	return f.newClient(storage, nil)
}

// NewWithUpdates is New with an update handler installed, for clients that must
// receive updates. A nil handler is rejected with ErrTelegramConfiguration
// instead of silently producing a client that drops updates.
func (f *Factory) NewWithUpdates(storage telegram.SessionStorage, handler telegram.UpdateHandler) (*telegram.Client, error) {
	if handler == nil {
		return nil, ErrTelegramConfiguration
	}
	return f.newClient(storage, handler)
}

// PooledAPI spreads the client over size extra connections to the current data
// center and returns an API that load balances calls over them, plus the
// function that closes those connections. It must be called while the client is
// running, which the runner does inside Client.Run. The pooled API wraps its own
// copy of the factory middleware chain, so it keeps the flood wait, retry, and
// rate limit behavior of the single connection API. A nil factory or client, or
// a size below one, returns ErrTelegramConfiguration, and the caller must call
// the returned close function when the work is done.
func (f *Factory) PooledAPI(client *telegram.Client, size int) (*tg.Client, func() error, error) {
	if f == nil || client == nil || size < 1 {
		return nil, nil, ErrTelegramConfiguration
	}
	invoker, err := client.Pool(int64(size))
	if err != nil {
		return nil, nil, fmt.Errorf("create Telegram connection pool: %w", err)
	}
	var wrapped tg.Invoker = invoker
	for i := len(f.middleware) - 1; i >= 0; i-- {
		wrapped = f.middleware[i].Handle(wrapped)
	}
	return tg.NewClient(wrapped), invoker.Close, nil
}

// AppCredentials reports the application ID and hash used by this factory. The
// boolean is false for a nil factory or for credentials that NewFactory would
// have rejected, so callers can decide whether Telegram login is available.
func (f *Factory) AppCredentials() (int, string, bool) {
	if f == nil || f.config.AppID <= 0 || strings.TrimSpace(f.config.AppHash) == "" {
		return 0, "", false
	}
	return f.config.AppID, f.config.AppHash, true
}

// newClient assembles the gotd options shared by New and NewWithUpdates: the
// session storage, the device identity, the resolver, a copy of the middleware
// chain, and the reconnect backoff derived from ReconnectTimeout. A nil
// factory, missing credentials, or a nil storage returns
// ErrTelegramConfiguration.
func (f *Factory) newClient(storage telegram.SessionStorage, handler telegram.UpdateHandler) (*telegram.Client, error) {
	if f == nil || f.config.AppID <= 0 || f.config.AppHash == "" || storage == nil {
		return nil, ErrTelegramConfiguration
	}
	options := telegram.Options{
		SessionStorage: storage,
		UpdateHandler:  handler,
		NoUpdates:      handler == nil,
		DialTimeout:    f.config.DialTimeout,
		MaxRetries:     f.config.MaxRetries,
		Device:         f.config.Device,
		Resolver:       f.resolver,
		Middlewares:    append([]telegram.Middleware(nil), f.middleware...),
		Logger:         f.config.Logger,
		RetryInterval:  2 * time.Second,
		ReconnectionBackoff: func() backoff.BackOff {
			value := backoff.NewExponentialBackOff()
			value.Multiplier = 1.1
			value.MaxElapsedTime = f.config.ReconnectTimeout
			value.MaxInterval = 10 * time.Second
			return value
		},
	}
	return telegram.NewClient(f.config.AppID, f.config.AppHash, options), nil
}

// DatabaseClientProvider loads one encrypted Telegram session for the user,
// constructs a fresh gotd client, and returns it unstarted. ClientRunner owns
// Client.Run and closes all network state when the request ends.
type DatabaseClientProvider struct {
	// queries reads and writes the TelDrive session rows. It is built from the
	// pool passed to NewDatabaseClientProvider and is never nil.
	queries *sqlcgen.Queries
	// cipher seals and opens the stored Telegram session blob.
	cipher *secureblob.Cipher
	// factory builds the gotd client for the loaded session.
	factory *Factory
}

// NewDatabaseClientProvider returns a ClientProvider that loads the caller's
// active Telegram session from the database and builds a fresh gotd client for
// it. A nil pool, cipher, or factory is rejected with
// ErrTelegramConfiguration, so a non-nil provider always has all three.
func NewDatabaseClientProvider(pool *pgxpool.Pool, cipher *secureblob.Cipher, factory *Factory) (*DatabaseClientProvider, error) {
	if pool == nil || cipher == nil || factory == nil {
		return nil, ErrTelegramConfiguration
	}
	return &DatabaseClientProvider{queries: sqlcgen.New(pool), cipher: cipher, factory: factory}, nil
}

// Client loads the user's active Telegram session and returns an unstarted
// gotd client that writes session updates back to the same row. When ctx
// carries the identity of the requested user, that login's session is loaded;
// otherwise the user's most recently active session is used. It returns
// ErrTelegramConfiguration for an unusable provider or a non-positive user ID
// and a wrapped error when the session cannot be loaded. The operation is
// ignored because session lookup does not depend on it.
func (p *DatabaseClientProvider) Client(ctx context.Context, userID int64, _ Operation) (*telegram.Client, error) {
	if p == nil || p.queries == nil || p.cipher == nil || p.factory == nil || userID <= 0 {
		return nil, ErrTelegramConfiguration
	}
	var stored *sqlcgen.Session
	var err error
	identity, hasIdentity := principal.FromContext(ctx)
	if hasIdentity && identity.UserID == userID && identity.SessionID != uuid.Nil {
		stored, err = p.queries.GetActiveSession(ctx, sqlcgen.GetActiveSessionParams{
			SessionID: dbtypes.UUID(identity.SessionID), UserID: userID,
		})
	} else {
		stored, err = p.queries.GetLatestActiveSessionForUser(ctx, userID)
	}
	if err != nil {
		return nil, fmt.Errorf("load active Telegram session: %w", err)
	}
	storage := &databaseSessionStorage{
		queries: p.queries, cipher: p.cipher, sessionID: stored.ID, userID: userID,
	}
	return p.factory.New(storage)
}

// databaseSessionStorage persists gotd session updates into one stored session
// row, translating between the binary gotd format and the Telethon string
// format TelDrive keeps in the database and encrypting the result. It is bound
// to a single session row, so every client needs its own instance.
type databaseSessionStorage struct {
	// queries accesses the session row.
	queries *sqlcgen.Queries
	// cipher seals and opens the blob under the "telegram-session" purpose.
	cipher *secureblob.Cipher
	// sessionID is the row that receives updates. An invalid UUID makes both
	// methods fail with ErrTelegramConfiguration.
	sessionID pgtype.UUID
	// userID is the owner of the row. Both paths address the session by
	// (session_id, user_id): the read filter scopes the load, and StoreSession's
	// update carries the same owner predicate, so a session ID alone can neither
	// read nor overwrite another user's session.
	userID int64
}

// LoadSession reads the row, decrypts the stored blob, and decodes the Telethon
// session string into the gotd format telegram.Client expects. It returns
// ErrTelegramConfiguration for a nil or incomplete storage and a wrapped error
// when the row cannot be read, decrypted, or decoded.
func (s *databaseSessionStorage) LoadSession(ctx context.Context) ([]byte, error) {
	if s == nil || s.queries == nil || s.cipher == nil || s.userID <= 0 || !s.sessionID.Valid {
		return nil, ErrTelegramConfiguration
	}
	row, err := s.queries.GetActiveSession(ctx, sqlcgen.GetActiveSessionParams{
		SessionID: s.sessionID, UserID: s.userID,
	})
	if err != nil {
		return nil, fmt.Errorf("load encrypted Telegram session: %w", err)
	}
	plain, err := s.cipher.Open("telegram-session", row.TelegramSession)
	if err != nil {
		return nil, fmt.Errorf("decrypt Telegram session: %w", err)
	}
	raw, err := telethonsession.DecodeToGotd(ctx, string(plain))
	if err != nil {
		return nil, fmt.Errorf("load Telethon Telegram session: %w", err)
	}
	return raw, nil
}

// StoreSession encodes a gotd session update as a Telethon string, encrypts it,
// and writes it back to the row. It returns ErrTelegramConfiguration for a nil
// or incomplete storage or empty data, ErrClientUnavailable when the update
// matched no row because the session was revoked while the client ran, and a
// wrapped error for an encoding, encryption, or database failure.
func (s *databaseSessionStorage) StoreSession(ctx context.Context, data []byte) error {
	if s == nil || s.queries == nil || s.cipher == nil || len(data) == 0 || !s.sessionID.Valid {
		return ErrTelegramConfiguration
	}
	encoded, err := telethonsession.EncodeGotd(ctx, data)
	if err != nil {
		return fmt.Errorf("encode Telegram session update as Telethon: %w", err)
	}
	ciphertext, err := s.cipher.Seal("telegram-session", []byte(encoded))
	if err != nil {
		return fmt.Errorf("encrypt Telegram session update: %w", err)
	}
	count, err := s.queries.UpdateSessionTelegramSession(ctx, sqlcgen.UpdateSessionTelegramSessionParams{
		TelegramSession: ciphertext, SessionID: s.sessionID, UserID: s.userID,
	})
	if err != nil {
		return fmt.Errorf("store Telegram session update: %w", err)
	}
	if count == 0 {
		return ErrClientUnavailable
	}
	return nil
}

var (
	_ ClientProvider          = (*DatabaseClientProvider)(nil)
	_ telegram.SessionStorage = (*databaseSessionStorage)(nil)
)
