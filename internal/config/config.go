// Package config defines Teldrive's configuration tree and the loader that
// assembles it from defaults, a TOML or YAML file, TELDRIVE_ environment
// variables, and command-line flags.
//
// Every leaf is addressable by the same name in all four sources: http.address
// in a config file, TELDRIVE_HTTP_ADDRESS in the environment, and --http-address
// on the command line. Sources are applied in increasing precedence, defaults <
// config file < environment variables < explicitly changed flags, and the merged
// result is checked by Config.Validate before it is returned.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhz8888/teldrive/v2/internal/database"
	"github.com/zhz8888/teldrive/v2/internal/size"
)

// envPrefix is prepended to every generated environment variable name, so the
// config key http.address is read from TELDRIVE_HTTP_ADDRESS.
const envPrefix = "TELDRIVE_"

// ErrInvalid reports configuration that is missing, malformed, or mutually
// inconsistent. Validate wraps it, as do the decoding and validation steps of
// Load and LoadFrom, but their config-file discovery and parsing failures are
// returned unwrapped, so callers must test for it with errors.Is instead of
// comparing error values.
var ErrInvalid = errors.New("invalid configuration")

// HTTP holds the listen address and timeouts of the public HTTP server, plus
// the proxies whose forwarding headers the server trusts.
type HTTP struct {
	// Address is copied into net/http's Server.Addr, so ":8080" binds every
	// interface; the required tag rejects an empty value at load time.
	Address string `koanf:"address" default:"127.0.0.1:8080" validate:"required" description:"HTTP listen address"`
	// ReadHeaderTimeout bounds reading the headers of one request, which is the
	// slow-client defence; zero removes the bound.
	ReadHeaderTimeout time.Duration `koanf:"read-header-timeout" default:"10s" validate:"gte=0" description:"Maximum time to read request headers"`
	// ReadTimeout bounds one whole request including its body, so an upload
	// streamed for longer than the value is cut off; zero removes the bound.
	ReadTimeout time.Duration `koanf:"read-timeout" default:"1h" validate:"gte=0" description:"Maximum time to read an entire request; zero disables it for streaming uploads"`
	// WriteTimeout bounds writing one response; zero removes the bound, which
	// long-lived responses such as the SSE stream and ranged downloads need.
	WriteTimeout time.Duration `koanf:"write-timeout" default:"1h" validate:"gte=0" description:"Maximum response write duration; zero disables it for streaming"`
	// IdleTimeout bounds an idle keep-alive connection; at zero net/http falls
	// back to ReadTimeout, and when that is zero too a connection never idles out.
	IdleTimeout time.Duration `koanf:"idle-timeout" default:"2m" validate:"gte=0" description:"HTTP keep-alive idle timeout"`
	// ShutdownTimeout bounds how long shutdown waits for in-flight requests and
	// for the download client pool to close; it must be positive.
	ShutdownTimeout time.Duration `koanf:"shutdown-timeout" default:"10s" validate:"gt=0" description:"Graceful shutdown timeout"`
	// TrustedProxies lists the proxies whose forwarding headers the request
	// security layer believes. Every entry must parse as an IP address or CIDR,
	// and an empty list trusts none, so the peer address is used as the client.
	TrustedProxies []string `koanf:"trusted-proxies" default:"" description:"Proxy IP addresses or CIDRs trusted to set forwarding headers"`
}

// TelegramMTProxy configures an MTProto proxy for the Telegram client. Address
// and Secret must be set together, and the proxy cannot be combined with the
// HTTP/SOCKS5 proxy in Telegram.Proxy.
type TelegramMTProxy struct {
	// Address is the proxy's host:port. Validate requires it to be set together
	// with Secret and rejects it alongside Telegram.Proxy, which it replaces.
	Address string `koanf:"address" default:"" description:"MTProto proxy address in host:port form"`
	// Secret is the proxy's hexadecimal secret, passed to gotd unchanged; a
	// value that is not valid hex fails when the client factory is built.
	Secret string `koanf:"secret" default:"" description:"MTProto proxy secret in hexadecimal form"`
}

// Telegram controls the gotd client that stores file data in Telegram:
// credentials and device identity, request throttling, transport retries, and
// the concurrency of upload and download workers. Backend selects between the
// real Telegram API and a local filesystem emulator.
type Telegram struct {
	// Backend picks the storage implementation; the loader trims and lowercases it
	// before validation, so "Remote" works, and the tag rejects anything but remote
	// or filesystem.
	Backend string `koanf:"backend" default:"remote" validate:"oneofci=remote filesystem" description:"Telegram backend: remote or filesystem"`
	// LocalRoot is the directory the filesystem emulator stores content under; a
	// leading "~" is expanded by the app, and a blank value is only rejected
	// while Backend is filesystem.
	LocalRoot string `koanf:"local-root" default:"./var/local-telegram" validate:"required_if=Backend filesystem" description:"Filesystem root used by the local Telegram emulator"`
	// AppID is the api_id of the Telegram application that owns the client
	// session; it must be positive and is only required for the remote backend.
	AppID int `koanf:"app-id" default:"2496" validate:"required_if=Backend remote,omitempty,gt=0" description:"Telegram application ID"`
	// AppHash is the api_hash paired with AppID and is only required for the
	// remote backend.
	AppHash string `koanf:"app-hash" default:"8da85b0d5bfe62527e5b244c209159c3" validate:"required_if=Backend remote" description:"Telegram application hash"`
	// DeviceModel is the device model reported in the MTProto initConnection
	// call; it identifies this client to Telegram, not the host.
	DeviceModel string `koanf:"device-model" default:"Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:109.0) Gecko/20100101 Firefox/116.0" validate:"required" description:"Telegram client device model"`
	// SystemVersion is the operating-system string reported in initConnection.
	SystemVersion string `koanf:"system-version" default:"Win32" validate:"required" description:"Telegram client system version"`
	// AppVersion is the application version string reported in initConnection.
	AppVersion string `koanf:"app-version" default:"6.1.4 K" validate:"required" description:"Telegram client application version"`
	// LanguageCode is the interface language reported in initConnection.
	LanguageCode string `koanf:"language-code" default:"en" validate:"required" description:"Telegram client language code"`
	// SystemLanguageCode is the system language reported in initConnection.
	SystemLanguageCode string `koanf:"system-language-code" default:"en-US" validate:"required" description:"Telegram client system language code"`
	// LanguagePack is the MTProto lang_pack parameter the client requests; the
	// app leaves it at the upstream default of the webk pack.
	LanguagePack string `koanf:"language-pack" default:"webk" description:"Telegram client language pack"`
	// DialTimeout bounds a single connection attempt to a Telegram endpoint and
	// must be positive.
	DialTimeout time.Duration `koanf:"dial-timeout" default:"10s" validate:"gt=0" description:"Telegram connection timeout"`
	// ReconnectTimeout caps how long the client backs off before rebuilding a
	// dropped connection; it is not a per-attempt timeout and must be positive.
	ReconnectTimeout time.Duration `koanf:"reconnect-timeout" default:"5m" validate:"gt=0" description:"Maximum Telegram reconnect backoff duration"`
	// MaxRetries is the number of extra transport attempts after a first
	// failure; zero disables retrying and the value may be zero.
	MaxRetries int `koanf:"max-retries" default:"10" validate:"gte=0" description:"Maximum Telegram transport retry attempts"`
	// RateLimit enables the client-side request limiter; when false the other
	// three rate settings are ignored.
	RateLimit bool `koanf:"rate-limit" default:"true" description:"Enable Telegram API request rate limiting"`
	// RateInterval is the minimum spacing between Telegram requests while
	// RateLimit is on; it must be positive.
	RateInterval time.Duration `koanf:"rate-interval" default:"50ms" validate:"gt=0" description:"Minimum interval between Telegram API requests"`
	// RateBurst is how many requests may be issued back to back before
	// RateInterval is enforced; it must be at least one.
	RateBurst int `koanf:"rate-burst" default:"10" validate:"gt=0" description:"Telegram API request burst allowance"`
	// Proxy is the HTTP, HTTPS, or SOCKS5 proxy URL used for Telegram traffic; a
	// blank value connects directly and the value is mutually exclusive with
	// MTProxy.
	Proxy string `koanf:"proxy" default:"" description:"HTTP, HTTPS, or SOCKS5 proxy URL"`
	// MTProxy is an alternative transport-level proxy; it is used instead of
	// Proxy and the two are rejected together by Config.Validate.
	MTProxy TelegramMTProxy `koanf:"mtproxy"`
	// ClientLogging routes gotd's own protocol logs through the application
	// logger; it is off by default because the traces are verbose.
	ClientLogging bool `koanf:"client-logging" default:"false" description:"Enable verbose gotd Telegram client logs through the application logger"`
	// UploadThreads is the number of parallel connections the storage layer uses
	// to send one part; the tag keeps it in 1..32.
	UploadThreads int `koanf:"upload-threads" default:"8" validate:"min=1,max=32" description:"Concurrent Telegram upload workers"`
	// DownloadBots caps how many enabled bots are considered for download
	// rotation; zero falls back to the authenticated user's session and the tag
	// keeps the value in 0..32.
	DownloadBots int `koanf:"download-bots" default:"0" validate:"min=0,max=32" description:"Maximum enabled bots used for download rotation; zero uses the authenticated user"`
	// DownloadClientPool keeps authenticated Telegram download clients warm
	// between HTTP requests, so a request does not pay for a fresh client run.
	// It is off by default because every pooled client keeps its own Telegram
	// connections open.
	DownloadClientPool bool `koanf:"download-client-pool" default:"false" description:"Keep authenticated Telegram download clients warm between HTTP requests"`
	// DownloadReadBuffers is the number of prefetched chunks held in memory per
	// open download stream, so it multiplies uploads.max-concurrent-downloads
	// into the process memory ceiling; the tag keeps it in 1..256.
	DownloadReadBuffers int `koanf:"download-read-buffers" default:"32" validate:"min=1,max=256" description:"Number of prefetched Telegram download chunks buffered in memory per stream"`
	// DownloadReadParallel is the number of chunk fetches in flight per stream,
	// which is also the connection count a pooled download session requests.
	DownloadReadParallel int `koanf:"download-read-parallel" default:"4" validate:"min=1,max=32" description:"Concurrent Telegram chunk fetches per download stream"`
	// RandomizePartNames stores each part under a random hex name; when false
	// the part is named "<fileName>.<part number>", which keeps the original
	// name visible in Telegram.
	RandomizePartNames bool `koanf:"randomize-part-names" default:"true" description:"Randomize Telegram document names"`
	// AutoChannelCreate lets channel allocation roll over to a newly created
	// channel when the selected one is full or unavailable; when false that
	// situation is reported as an error, and the admin methods never create
	// channels implicitly either way.
	AutoChannelCreate bool `koanf:"auto-channel-create" default:"true" description:"Create storage channels automatically"`
	// BotRotationBackend selects where download rotation state lives: "memory"
	// is per instance, "database" shares it across instances, and the tag
	// rejects any other value.
	BotRotationBackend string `koanf:"bot-rotation-backend" default:"memory" validate:"oneof=memory database" description:"Bot rotation backend: memory for single-instance speed or database for cluster-wide coordination"`
	// ChannelPartLimit is the number of stored messages at which a channel
	// counts as full and rollover starts; it must be positive.
	ChannelPartLimit int64 `koanf:"channel-part-limit" default:"500000" validate:"gt=0" description:"Maximum parts stored in one Telegram channel"`
	// ChannelNamePrefix starts the name of every generated channel, which ends
	// with the UTC creation timestamp; the tag rejects an empty prefix.
	ChannelNamePrefix string `koanf:"channel-name-prefix" default:"teldrive" validate:"required" description:"Prefix for automatically created channels"`
}

// Encryption configures Teldrive-managed encryption of the file data stored in
// Telegram. Keys maps a positive key version to its secret, and ActiveKeyVersion
// selects the version used for new uploads, which must be one of the configured
// keys.
type Encryption struct {
	// ActiveKeyVersion is stamped on the parts of an upload that requests
	// encryption; a request that asks for encryption while the value is zero is
	// rejected, and a positive value must name an entry in Keys.
	ActiveKeyVersion int32 `koanf:"active-key-version" default:"0" validate:"gte=0" description:"Active server-managed encryption key version for new uploads"`
	// Keys maps a key version to its secret. The loader accepts either a table
	// or the comma-separated "version:key" form; a version must be positive, a
	// secret must not be blank, and the decoded map is never nil.
	Keys map[int32]string `koanf:"keys" default:"" description:"Encryption keys as comma-separated version:key entries"`
}

// Security holds the JWT signing material and the authorization policy: the key
// that encrypts stored Telegram credentials, the token and login-flow lifetimes,
// and the allow-list of Telegram usernames permitted to sign in.
type Security struct {
	// SigningKey signs and verifies access tokens with HS256; both the tag and
	// authn.NewService reject a value shorter than 32 characters.
	SigningKey string `koanf:"signing-key" default:"" validate:"required,min=32" description:"JWT signing key"`
	// DataKey is the base64 secret with which the XChaCha20-Poly1305 cipher
	// seals stored Telegram credentials and migrated bot tokens. It must decode
	// to exactly 32 bytes, and Validate refuses to start without it.
	DataKey string `koanf:"data-key" default:"" validate:"required" description:"Key used to encrypt stored Telegram credentials"`
	// Issuer is embedded in each access token and required again on every bearer
	// authentication, so tokens minted by another deployment are rejected.
	Issuer string `koanf:"issuer" default:"teldrive-v2" validate:"required" description:"JWT issuer"`
	// AllowedUsers is the login allow-list, matched against Telegram usernames
	// with an optional leading "@"; entries must not be blank and an empty list
	// permits every account.
	AllowedUsers []string `koanf:"allowed-users" default:"" description:"Allowed Telegram usernames; empty permits every user"`
	// AccessTokenTTL is the access-token lifetime, reported to clients in
	// seconds; it must be positive.
	AccessTokenTTL time.Duration `koanf:"access-token-ttl" default:"15m" validate:"gt=0" description:"Access-token lifetime"`
	// RefreshTokenTTL is the session lifetime: the sessions row and its refresh
	// token expire together, and it must be positive.
	RefreshTokenTTL time.Duration `koanf:"refresh-token-ttl" default:"720h" validate:"gt=0" description:"Refresh-token lifetime"`
	// LoginFlowTTL is how long an unfinished Telegram login flow may be polled
	// before it is reported as not found; it must be positive.
	LoginFlowTTL time.Duration `koanf:"login-flow-ttl" default:"10m" validate:"gt=0" description:"Telegram login-flow lifetime"`
}

// Logging selects the level and the encoding used by the process-wide
// application logger.
type Logging struct {
	// LogLevel is parsed by slog's level parser after trimming, so the four
	// names are matched case-insensitively; the loader lowercases the value before
	// validation so the tag accepts the same spellings, and an unknown value stops
	// startup.
	LogLevel string `koanf:"log-level" default:"info" validate:"oneofci=debug info warn error" description:"Log level: debug, info, warn, or error"`
	// LogFormat selects the handler: json (also the empty value) or text, which
	// colors its output only when stdout is a terminal.
	LogFormat string `koanf:"log-format" default:"text" validate:"oneof=json text" description:"Log format: json or text"`
}

// Events tunes the PostgreSQL-backed server-sent event stream: per-user
// connection limits, batch size, heartbeat and write deadlines, ticket
// lifetimes, and the listener's ping and reconnect policy.
type Events struct {
	// BatchSize caps the rows one event read returns; the tag keeps it in
	// 1..1000 and the app narrows it to int32.
	BatchSize int `koanf:"batch-size" default:"100" validate:"min=1,max=1000" description:"Maximum events read from PostgreSQL per SSE batch"`
	// MaxConnectionsPerUser caps the concurrent event streams one user may hold
	// on this instance, so a user behind several instances gets the limit per
	// instance.
	MaxConnectionsPerUser int `koanf:"max-connections-per-user" default:"5" validate:"min=1,max=1000" description:"Maximum concurrent SSE connections per user on one API instance"`
	// Heartbeat is how long a stream may stay silent before the handler polls
	// for new events and writes a keep-alive comment.
	Heartbeat time.Duration `koanf:"heartbeat" default:"20s" validate:"gt=0" description:"SSE heartbeat interval"`
	// WriteTimeout bounds a single write to a slow stream client.
	WriteTimeout time.Duration `koanf:"write-timeout" default:"10s" validate:"gt=0" description:"Maximum duration for one SSE write and flush"`
	// TicketTTL is how long an issued event stream ticket keeps authenticating,
	// which is deliberately much shorter than an access token.
	TicketTTL time.Duration `koanf:"ticket-ttl" default:"2m" validate:"gt=0" description:"Lifetime of browser event stream tickets"`
	// CleanupInterval is how often expired event stream tickets are deleted.
	CleanupInterval time.Duration `koanf:"cleanup-interval" default:"1h" validate:"gt=0" description:"Expired event stream ticket cleanup interval"`
	// ConnectTimeout bounds one event listener connect attempt and each ticket
	// cleanup query.
	ConnectTimeout time.Duration `koanf:"connect-timeout" default:"10s" validate:"gt=0" description:"PostgreSQL event listener connection timeout"`
	// PingInterval is how long the event listener may idle before it pings
	// PostgreSQL to prove the connection is still alive.
	PingInterval time.Duration `koanf:"ping-interval" default:"5s" validate:"gt=0" description:"PostgreSQL event listener health-check interval"`
	// ReconnectMin is the delay before the listener's first reconnect attempt;
	// Validate rejects a ReconnectMax below it.
	ReconnectMin time.Duration `koanf:"reconnect-min" default:"100ms" validate:"gt=0" description:"Minimum PostgreSQL listener reconnect delay"`
	// ReconnectMax caps the listener's exponential reconnect backoff.
	ReconnectMax time.Duration `koanf:"reconnect-max" default:"30s" validate:"gt=0" description:"Maximum PostgreSQL listener reconnect delay"`
}

// Uploads configures resumable upload sessions, content hashing, and the server
// directories that local background imports may read from.
type Uploads struct {
	// SessionTTL is added to the creation time to get a session's ExpiresAt. The
	// job runtime also uses it as the grace period of the orphaned-document
	// cleanup, so lowering it lets that sweep consider younger documents.
	SessionTTL time.Duration `koanf:"session-ttl" default:"168h" validate:"gt=0" description:"Lifetime of resumable upload sessions"`
	// HashingEnabled computes the BLAKE3 tree hash recorded for each file. When
	// false, parts are stored without a checksum unless the caller supplied one,
	// which skips the hashing pass over the uploaded bytes.
	HashingEnabled bool `koanf:"hashing-enabled" default:"true" description:"Compute and store BLAKE3 hashes for uploaded files"`
	// LocalImportRoots lists the absolute directories a background local import
	// may read from. An empty list disables local imports, and a path outside
	// every root is refused, which is the security boundary for this feature.
	LocalImportRoots []string `koanf:"local-import-roots" default:"" description:"Absolute server directories allowed as local background-upload sources; empty disables local imports"`
	// MaxConcurrentDownloads bounds how many byte ranges may be open at once.
	// Each one buffers telegram.download-read-buffers megabytes in memory, so an
	// unbounded server is driven into the OOM killer by a handful of concurrent
	// downloads on a small host. A request waits for a free slot rather than
	// being refused, and is abandoned only when its own context is cancelled.
	MaxConcurrentDownloads int `koanf:"max-concurrent-downloads" default:"4" validate:"min=1,max=256" description:"Maximum number of downloads open at the same time"`
	// DefaultPartSize is the part size a session receives when it does not request
	// one. A larger part means fewer Telegram documents and less request overhead;
	// a smaller one means a failed part is cheaper to retry and less memory is
	// held per concurrent upload, which is what matters on a small host.
	DefaultPartSize size.Size `koanf:"default-part-size" default:"512MiB" validate:"gt=0" description:"Part size a session gets when it does not request one, in bytes or a size such as 128MiB"`
	// MaxPartSize caps the part size a session may request. It has to stay at or
	// above DefaultPartSize, and it keeps the byte arithmetic below inside int64.
	MaxPartSize size.Size `koanf:"max-part-size" default:"4GiB" validate:"gt=0" description:"Largest part size a session may request, in bytes or a size such as 4GiB"`
}

// Jobs controls whether this process runs the River background job workers or
// leaves that to another instance of the server.
type Jobs struct {
	// RunWorkers starts the River workers in this process during startup; when
	// false the instance only enqueues, which is how a deployment dedicates
	// separate processes to serving and to background work.
	RunWorkers bool `koanf:"run-workers" default:"true" description:"Run River background job workers in this process"`
}

// MemoryCache sizes the single Ristretto cache shared by the whole process.
type MemoryCache struct {
	// Size is the maximum cost Ristretto may hold, in bytes, and it must be
	// positive; the loader also accepts human forms such as "5MB". It bounds the
	// whole process cache, so a larger value trades memory for fewer cache misses.
	Size size.Size `koanf:"size" default:"5MB" validate:"gt=0" description:"Global Ristretto memory cache size (e.g., 5MB, 10MB)"`
}

// Cache groups the process-wide in-memory caches. They are built once by the
// application composition root and shared across requests.
type Cache struct {
	// Memory is the single process-local cache shared by the domain services;
	// its configured size must be greater than zero.
	Memory MemoryCache `koanf:"memory"`
}

// Config is the complete server configuration tree. Each field is a section
// that maps to a same-named TOML/YAML table, to a TELDRIVE_-prefixed family of
// environment variables, and to a group of kebab-case command-line flags.
type Config struct {
	// HTTP configures the public HTTP server: listen address, timeouts, and the
	// proxies trusted to set forwarding headers.
	HTTP HTTP `koanf:"http"`
	// Database configures the PostgreSQL connection pool and schema.
	Database database.Config `koanf:"database"`
	// Telegram configures the Telegram storage backend and its client.
	Telegram Telegram `koanf:"telegram"`
	// Encryption configures Teldrive-managed encryption keys for new uploads.
	Encryption Encryption `koanf:"encryption"`
	// Security configures authentication, token lifetimes, and access control.
	Security Security `koanf:"security"`
	// Logging configures the application log level and format.
	Logging Logging `koanf:"logging"`
	// Events configures the server-sent event stream and its database listener.
	Events Events `koanf:"events"`
	// Uploads configures resumable upload sessions, hashing, and local imports.
	Uploads Uploads `koanf:"uploads"`
	// Jobs configures in-process background job workers.
	Jobs Jobs `koanf:"jobs"`
	// Cache configures the process-wide in-memory caches.
	Cache Cache `koanf:"cache"`
}

// Default returns a Config whose fields are filled from the `default` struct
// tags of the configuration tree. It panics when a tag cannot be parsed or a
// field has an unsupported type, because that is a programming error in the
// declarations rather than bad user input; use Load to read a validated
// configuration from files, the environment, and flags.
func Default() Config {
	var cfg Config
	if err := applyDefaults(&cfg); err != nil {
		panic(err)
	}
	return cfg
}

// normalizeAllowedUsername normalises one allow-list entry exactly the way the
// login path does: surrounding whitespace is trimmed, one leading "@" is dropped,
// the remainder is trimmed again, and the result is lowercased. Validation only
// needs the emptiness decision, but the order matters: trimming before the leading
// "@" is stripped, as an earlier version did, let an entry such as " @" pass here
// while the login path dropped it as blank, and a list whose entries are all
// dropped becomes empty, which the login path reads as "permit every account". The
// login path cannot share this helper because its package imports this one.
func normalizeAllowedUsername(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "@")))
}

// Validate checks the rules that the `validate` struct tags cannot express:
// a non-empty data key, connection-pool ordering, trusted-proxy syntax, the
// exclusivity of MTProxy and Proxy, encryption-key consistency, allowed
// usernames, absolute local import roots, and the event reconnect bounds.
//
// A missing security.data-key fails immediately with a dedicated message; every
// other problem is collected, sorted, and joined with semicolons into a single
// error wrapping ErrInvalid. It returns nil when the configuration is usable.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Security.DataKey) == "" {
		return fmt.Errorf("%w: security.data-key is required; set security.data-key or TELDRIVE_SECURITY_DATA_KEY before starting Teldrive", ErrInvalid)
	}
	problems := validateTaggedFields(c)

	// A default part above the cap would make every session that does not
	// request one fail, so the pair is checked together at startup rather than
	// silently corrected while serving.
	if c.Uploads.DefaultPartSize > c.Uploads.MaxPartSize {
		problems = append(problems, "uploads default-part-size cannot exceed max-part-size")
	}
	if c.Uploads.MaxPartSize > 0 && c.Uploads.DefaultPartSize > 0 && c.Uploads.MaxPartSize%c.Uploads.DefaultPartSize != 0 {
		problems = append(problems, "uploads max-part-size must be a whole multiple of default-part-size")
	}
	if c.Database.MinConnections > c.Database.MaxConnections {
		problems = append(problems, "database min connections cannot exceed max connections")
	}
	for _, proxy := range c.HTTP.TrustedProxies {
		value := strings.TrimSpace(proxy)
		if _, err := netip.ParseAddr(value); err != nil {
			if _, prefixErr := netip.ParsePrefix(value); prefixErr != nil {
				problems = append(problems, fmt.Sprintf("HTTP trusted proxy %q is not an IP address or CIDR", proxy))
			}
		}
	}

	mtAddress := strings.TrimSpace(c.Telegram.MTProxy.Address)
	mtSecret := strings.TrimSpace(c.Telegram.MTProxy.Secret)
	if (mtAddress == "") != (mtSecret == "") {
		problems = append(problems, "Telegram MTProxy address and secret must be configured together")
	}
	if mtAddress != "" && strings.TrimSpace(c.Telegram.Proxy) != "" {
		problems = append(problems, "Telegram proxy and MTProxy cannot be used together")
	}

	if len(c.Encryption.Keys) > 0 && c.Encryption.ActiveKeyVersion == 0 {
		problems = append(problems, "active encryption key version is required when encryption keys are configured")
	}
	if c.Encryption.ActiveKeyVersion > 0 {
		key, ok := c.Encryption.Keys[c.Encryption.ActiveKeyVersion]
		if !ok || strings.TrimSpace(key) == "" {
			problems = append(problems, "active encryption key version has no configured key")
		}
	}
	for version, key := range c.Encryption.Keys {
		if version <= 0 || strings.TrimSpace(key) == "" {
			problems = append(problems, fmt.Sprintf("encryption key version %d is invalid", version))
		}
	}

	for _, username := range c.Security.AllowedUsers {
		// The entry is normalised exactly the way the login path normalises it
		// (trim, strip one leading "@", trim again), so an entry that trims down
		// to nothing is refused here. Accepting it would let the login path drop
		// it silently, and an allow-list that drops every entry becomes empty,
		// which the login path reads as "permit every account".
		if normalizeAllowedUsername(username) == "" {
			problems = append(problems, "security allowed users cannot contain an empty username")
			break
		}
	}
	for _, root := range c.Uploads.LocalImportRoots {
		value := strings.TrimSpace(root)
		if value == "" || !filepath.IsAbs(value) {
			problems = append(problems, fmt.Sprintf("upload local import root %q must be an absolute path", root))
		}
	}
	if c.Events.ReconnectMax < c.Events.ReconnectMin {
		problems = append(problems, "event reconnect maximum must not be less than minimum")
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("%w: %s", ErrInvalid, strings.Join(problems, "; "))
	}
	return nil
}

// parseEncryptionKeys decodes the comma-separated list of version:key entries
// used by the encryption.keys setting. It returns an error wrapping ErrInvalid
// when an entry is malformed, has a non-positive or unparsable version, or
// repeats a version; empty or whitespace-only input yields an empty, non-nil
// map.
func parseEncryptionKeys(raw string) (map[int32]string, error) {
	keys := make(map[int32]string)
	if strings.TrimSpace(raw) == "" {
		return keys, nil
	}
	for entry := range strings.SplitSeq(raw, ",") {
		versionText, key, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok {
			return nil, fmt.Errorf("%w: encryption keys must use version:key entries", ErrInvalid)
		}
		version64, err := strconv.ParseInt(strings.TrimSpace(versionText), 10, 32)
		if err != nil || version64 <= 0 || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("%w: an encryption key entry is invalid", ErrInvalid)
		}
		version := int32(version64)
		if _, exists := keys[version]; exists {
			return nil, fmt.Errorf("%w: duplicate encryption key version %d", ErrInvalid, version)
		}
		keys[version] = key
	}
	return keys, nil
}
