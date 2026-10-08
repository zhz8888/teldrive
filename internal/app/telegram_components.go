package app

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/gotd/log/logslog"
	"github.com/gotd/td/telegram"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zhz8888/teldrive/v2/internal/authn"
	"github.com/zhz8888/teldrive/v2/internal/botgateway"
	"github.com/zhz8888/teldrive/v2/internal/bots"
	"github.com/zhz8888/teldrive/v2/internal/cache"
	"github.com/zhz8888/teldrive/v2/internal/config"
	"github.com/zhz8888/teldrive/v2/internal/localtelegram"
	"github.com/zhz8888/teldrive/v2/internal/logingateway"
	"github.com/zhz8888/teldrive/v2/internal/secureblob"
	"github.com/zhz8888/teldrive/v2/internal/telegramstore"
)

// telegramComponents groups the Telegram-facing gateways that one configured
// backend provides. The fields are always set together by buildTelegramComponents;
// only downloadClients may be nil, because the pool exists just for the remote
// backend with DownloadClientPool enabled and without an injected storage.
type telegramComponents struct {
	// login drives interactive Telegram authorization and is localTelegramLogin
	// when the filesystem backend is selected.
	login authn.TelegramLogin
	// verifier turns Telegram init data into a bot identity; it rejects every
	// credential on the filesystem backend.
	verifier bots.Verifier
	// account exposes the caller's own Telegram account data, namely the channels
	// they may store files in and their profile photo.
	account telegramstore.Account
	// storage is the storage boundary used by uploads, downloads and purge.
	storage telegramstore.Storage
	// downloadClients pools warm Telegram clients for reads. It is nil unless the
	// remote backend built its own storage with pooling enabled, and the owner
	// must close it; injected storage leaves pooling to the injector.
	downloadClients *telegramstore.DownloadClientPool
}

// buildLegacyBotVerifier builds only the bot verifier, without the pool, cipher or
// storage the full component set needs. The legacy database migration runs before
// the connection pool exists yet still has to authenticate bot rows through the
// Telegram API, so it needs this narrow dependency. Unsupported backends are
// rejected with an error naming the configured value.
func buildLegacyBotVerifier(cfg config.Config, logger *slog.Logger) (bots.Verifier, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Telegram.Backend)) {
	case "filesystem":
		return localBotVerifier{}, nil
	case "remote":
		factory, err := newRemoteTelegramFactory(cfg, logger)
		if err != nil {
			return nil, err
		}
		verifier, err := botgateway.NewGotdVerifier(factory)
		if err != nil {
			return nil, fmt.Errorf("create bot verifier: %w", err)
		}
		return verifier, nil
	default:
		return nil, fmt.Errorf("unsupported Telegram backend %q", cfg.Telegram.Backend)
	}
}

// newRemoteTelegramFactory builds the gotd client factory that every remote
// backend component shares, so AppID/AppHash, device identity, timeouts, rate
// limits and proxy settings are applied once. gotd's own request logging is wired
// to logger only when Telegram.ClientLogging is enabled; otherwise the factory
// runs with a nil logger to keep protocol traces out of the application log.
func newRemoteTelegramFactory(cfg config.Config, logger *slog.Logger) (*telegramstore.Factory, error) {
	gotdLogger := logslog.New(logger)
	if !cfg.Telegram.ClientLogging {
		gotdLogger = nil
	}
	factory, err := telegramstore.NewFactory(telegramstore.FactoryConfig{
		AppID: cfg.Telegram.AppID, AppHash: cfg.Telegram.AppHash,
		Device: telegram.DeviceConfig{
			DeviceModel: cfg.Telegram.DeviceModel, SystemVersion: cfg.Telegram.SystemVersion,
			AppVersion: cfg.Telegram.AppVersion, LangCode: cfg.Telegram.LanguageCode,
			SystemLangCode: cfg.Telegram.SystemLanguageCode, LangPack: cfg.Telegram.LanguagePack,
		},
		DialTimeout: cfg.Telegram.DialTimeout, ReconnectTimeout: cfg.Telegram.ReconnectTimeout,
		MaxRetries: cfg.Telegram.MaxRetries, RateLimit: cfg.Telegram.RateLimit,
		RateInterval: cfg.Telegram.RateInterval, RateBurst: cfg.Telegram.RateBurst,
		Proxy: cfg.Telegram.Proxy, MTProxyAddress: cfg.Telegram.MTProxy.Address,
		MTProxySecret: cfg.Telegram.MTProxy.Secret,
		Logger:        gotdLogger,
	})
	if err != nil {
		return nil, fmt.Errorf("create Telegram client factory: %w", err)
	}
	return factory, nil
}

// buildTelegramComponents assembles the Telegram gateways for the configured
// backend. The filesystem backend opens the local emulator under
// Telegram.LocalRoot and never touches pool or cipher; the remote backend builds
// the session provider on pool, which is why the caller must migrate the database
// and open the pool first. An injected storage short-circuits storage
// construction, and with it the download client pool, so tests and alternate
// backends keep ownership of both. Every failure is returned with the component
// that could not be built wrapped into the error.
func buildTelegramComponents(cfg config.Config, pool *pgxpool.Pool, cipher *secureblob.Cipher, logger *slog.Logger, injected telegramstore.Storage, globalCache cache.Cacher) (telegramComponents, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Telegram.Backend)) {
	case "filesystem":
		root, err := expandHomePath(cfg.Telegram.LocalRoot)
		if err != nil {
			return telegramComponents{}, fmt.Errorf("resolve local Telegram root: %w", err)
		}
		server, err := localtelegram.Open(root)
		if err != nil {
			return telegramComponents{}, fmt.Errorf("open local Telegram emulator: %w", err)
		}
		runner, err := localtelegram.NewRunner(server)
		if err != nil {
			return telegramComponents{}, err
		}
		account, err := telegramstore.NewGotdAccount(runner)
		if err != nil {
			return telegramComponents{}, fmt.Errorf("create local Telegram account gateway: %w", err)
		}
		storage := injected
		if storage == nil {
			storage = telegramstore.NewGotdStorage(runner, globalCache)
		}
		return telegramComponents{
			login: localTelegramLogin{}, verifier: localBotVerifier{}, account: account, storage: storage,
		}, nil

	case "remote":
		factory, err := newRemoteTelegramFactory(cfg, logger)
		if err != nil {
			return telegramComponents{}, err
		}
		login, err := logingateway.New(factory)
		if err != nil {
			return telegramComponents{}, fmt.Errorf("create Telegram login gateway: %w", err)
		}
		verifier, err := botgateway.NewGotdVerifier(factory)
		if err != nil {
			return telegramComponents{}, fmt.Errorf("create bot verifier: %w", err)
		}
		provider, err := telegramstore.NewDatabaseClientProvider(pool, cipher, factory)
		if err != nil {
			return telegramComponents{}, fmt.Errorf("create Telegram session provider: %w", err)
		}
		userRunner := telegramstore.ClientRunner{Provider: provider, Factory: factory}
		account, err := telegramstore.NewGotdAccount(userRunner)
		if err != nil {
			return telegramComponents{}, fmt.Errorf("create Telegram account gateway: %w", err)
		}
		storage := injected
		var downloadClients *telegramstore.DownloadClientPool
		if storage == nil {
			channelBotProvider, err := botgateway.NewChannelBotProvider(pool)
			if err != nil {
				return telegramComponents{}, fmt.Errorf("create channel bot provider: %w", err)
			}
			uploadRunner, err := botgateway.NewUploadAwareRunner(pool, cipher, factory, userRunner, cfg.Telegram.DownloadBots, cfg.Telegram.BotRotationBackend)
			if err != nil {
				return telegramComponents{}, fmt.Errorf("create upload-aware Telegram runner: %w", err)
			}
			options := []telegramstore.GotdStorageOption{
				telegramstore.WithBotProvider(channelBotProvider),
				telegramstore.WithDownloadReadBuffers(cfg.Telegram.DownloadReadBuffers),
				telegramstore.WithDownloadReadParallel(cfg.Telegram.DownloadReadParallel),
			}
			if cfg.Telegram.DownloadClientPool {
				downloadClients, err = telegramstore.NewDownloadClientPool(uploadRunner, telegramstore.DownloadClientPoolConfig{
					Clients:      max(1, cfg.Telegram.DownloadBots),
					ReadBuffers:  cfg.Telegram.DownloadReadBuffers,
					ReadParallel: cfg.Telegram.DownloadReadParallel,
				}, globalCache)
				if err != nil {
					return telegramComponents{}, fmt.Errorf("create Telegram download client pool: %w", err)
				}
				options = append(options, telegramstore.WithDownloadClientPool(downloadClients))
			}
			storage = telegramstore.NewGotdStorage(uploadRunner, globalCache, options...)
		}
		return telegramComponents{
			login: login, verifier: verifier, account: account, storage: storage, downloadClients: downloadClients,
		}, nil

	default:
		return telegramComponents{}, fmt.Errorf("unsupported Telegram backend %q", cfg.Telegram.Backend)
	}
}
