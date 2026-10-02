# Teldrive

[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/tgdrive/teldrive)

**English** · [简体中文](README.zh-CN.md)

Teldrive turns Telegram into self-hosted cloud storage. The server keeps file metadata in
PostgreSQL, stores file payloads in Telegram channels, and serves a web UI, an HTTP API and
rclone's native backend from one binary.

## Highlights

- **One binary** — the React web UI is embedded in the Go server, so the same process answers
  the browser, the API and `rclone`.
- **Contract-first API** — TypeSpec under `typespec/` owns the HTTP contract; the OpenAPI
  document, the Go server, the TypeScript client types and the API reference are generated
  from it.
- **Resumable uploads, streaming downloads** — durable upload sessions with per-part retries,
  HTTP range downloads, and background imports from URLs or server paths.
- **Background jobs** — uploads, cleanups and bot provisioning run as durable River jobs with
  retries, queues and pause/resume controls in the UI.
- **Sharing and access control** — public share links, per-file grants, API keys for external
  clients, and role-gated administration screens.
- **Optional content encryption** — chunked, seekable encryption with versioned keys, plus a
  data key that protects the stored Telegram credentials.
- **Scales past one process** — run one instance with the workers in-process, or split the API
  and worker roles across instances; PostgreSQL is the only shared state.

## Requirements

- PostgreSQL with persistent storage
- A Telegram account with outbound access to Telegram
- `security.signing-key` and `security.data-key`

For internet-facing deployments, terminate HTTPS in front of Teldrive and keep PostgreSQL on a
private network.

## Quick start

### Container

The [quick start guide](https://tgdrive.github.io/teldrive/getting-started/quick-start)
generates a `compose.yaml` for PostgreSQL plus Teldrive. Back up `security.data-key` and every
content-encryption key before you start: losing them can make protected data unrecoverable.

With PostgreSQL already available:

```bash
docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -e TELDRIVE_HTTP_ADDRESS=0.0.0.0:8080 \
  -e TELDRIVE_DATABASE_URL='postgres://teldrive:password@db.example:5432/teldrive?sslmode=require' \
  -e TELDRIVE_SECURITY_SIGNING_KEY='YOUR_SIGNING_KEY' \
  -e TELDRIVE_SECURITY_DATA_KEY='YOUR_DATA_KEY' \
  ghcr.io/tgdrive/teldrive:v2
```

Open <http://127.0.0.1:8080>, sign in with Telegram, and upload a small test file. Pin a
release tag (`ghcr.io/tgdrive/teldrive:vX.Y.Z`) instead of `latest` for controlled upgrades.

### Release binary

Download the archive for your platform from GitHub Releases and install `teldrive`, then point
it at PostgreSQL:

```bash
teldrive check     # validate configuration, run migrations, initialize dependencies
teldrive run       # serve the API and the UI (alias: serve)
teldrive version   # print build metadata
```

### From source

Requires Go 1.26, [Bun](https://bun.sh), [Just](https://github.com/casey/just), and PostgreSQL
to run the server.

```bash
git clone https://github.com/tgdrive/teldrive.git
cd teldrive
just install-tools
just build
./bin/teldrive version
```

`just build` regenerates the UI API client, builds the Vite/React UI, and embeds it into the Go
binary. A Nix flake is available as well: `nix develop` provides the toolchain.

## Configuration

Settings are read from a file, `TELDRIVE_*` environment variables and flags, in that order of
precedence. Start from `config.sample.yaml` (or `config.sample.toml`) and see the
[configuration reference](https://tgdrive.github.io/teldrive/configuration/overview).

`teldrive check` loads the configuration, applies the migrations and initializes every
dependency once, then exits — use it as a deployment pre-flight.

## Development

```bash
just dev         # backend plus the Vite dev server
just test-unit   # unit tests
just ui-check    # UI lint, typecheck, browser tests and build
just check       # full gate: generation, lint, tests, coverage, builds
```

Integration and race tests need Podman:

```bash
just test-integration
just test-race
```

Generated artifacts are owned by their sources: `typespec/*.tsp` for the HTTP contract
(`just generate-api`, `just generate-ui`, or `just generate`) and `db/queries/*.sql` for the
query layer (`just generate-db`). Do not hand-edit `openapi/`, `internal/api/gen/`,
`internal/db/sqlcgen/` or `ui/src/api/schema.ts`; `AGENTS.md` documents the repository layout,
generation rules and test harness.

## Documentation

- Guides: <https://tgdrive.github.io/teldrive>
- API reference: <https://tgdrive.github.io/teldrive/api/>
- rclone: choose the native `teldrive` backend and set `api_host` plus an API key created in
  **Settings → API keys**

## Best practices

**Do**

- Respect Telegram's limits. Teldrive is a wrapper over your Telegram account; misuse gets the
  account banned and the channel deleted.
- Keep `security.data-key`, the content-encryption keys and database backups together, and
  verify a restore before you need it.
- Store what serves a purpose, and upgrade deliberately.

**Don't**

- Hoard data: it violates Telegram's terms and gains nothing.

## Contributing

Issues and pull requests are welcome at <https://github.com/tgdrive/teldrive>. Run `just check`
before opening a pull request; `AGENTS.md` is the entry point for working in this repository.

## License

Teldrive is released under the
[MIT License](https://github.com/tgdrive/teldrive/blob/main/LICENSE) (Copyright © 2024
divyam234).

## Recognitions

<a href="https://trendshift.io/repositories/7568" target="_blank"><img src="https://trendshift.io/api/badge/repositories/7568" alt="divyam234%2Fteldrive | Trendshift" style="width: 250px; height: 55px;" width="250" height="55"/></a>

<a href="https://www.star-history.com/#tgdrive/teldrive&Date">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=tgdrive/teldrive&type=Date&theme=dark" />
    <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/svg?repos=tgdrive/teldrive&type=Date" />
    <img alt="Star History Chart" src="https://api.star-history.com/svg?repos=tgdrive/teldrive&type=Date" />
  </picture>
</a>
