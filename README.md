# Teldrive

[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/zhz8888/teldrive)

**English** · [简体中文](README.zh-CN.md)

> **This repository is a fork.** It is maintained at
> [zhz8888/teldrive](https://github.com/zhz8888/teldrive) by
> [zhz8888](https://github.com/zhz8888), and it builds on
> [tgdrive/teldrive](https://github.com/tgdrive/teldrive) by
> [divyam234](https://github.com/divyam234). The releases, container images, documentation,
> issues and pull requests for this fork live here rather than upstream.

Teldrive turns Telegram into self-hosted cloud storage. The server keeps file metadata in
PostgreSQL, stores file payloads in Telegram channels, and serves a web UI and an HTTP API from
one binary.

## Highlights

- **One binary** — the React web UI is embedded in the Go server, so the same process answers
  the browser and the API.
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

The [quick start guide](https://zhz8888.github.io/teldrive/getting-started/quick-start)
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
  ghcr.io/zhz8888/teldrive:v2
```

Open <http://127.0.0.1:8080>, sign in with Telegram, and upload a small test file. Pin a
release tag (`ghcr.io/zhz8888/teldrive:vX.Y.Z`) instead of `latest` for controlled upgrades.

### Release binary

Download the archive for your platform from this fork's
[GitHub Releases](https://github.com/zhz8888/teldrive/releases) and install `teldrive`, then
point it at PostgreSQL:

```bash
teldrive check     # validate configuration, run migrations, initialize dependencies
teldrive run       # serve the API and the UI (alias: serve)
teldrive version   # print build metadata
```

### From source

Requires Go 1.26, [Bun](https://bun.sh), [Just](https://github.com/casey/just), and PostgreSQL
to run the server.

```bash
git clone https://github.com/zhz8888/teldrive.git
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
[configuration reference](https://zhz8888.github.io/teldrive/configuration/overview).

`teldrive check` loads the configuration, applies the migrations and initializes every
dependency once, then exits — use it as a deployment pre-flight.

## Development

```bash
just dev         # backend plus the Vite dev server
just test-unit   # unit tests
just ui-check    # UI lint, typecheck, browser tests and build
just check       # full gate: generation, lint, tests, coverage, builds
```

Integration and race tests need a container runtime, Docker or Podman:

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

- Guides: <https://zhz8888.github.io/teldrive>
- API reference: <https://zhz8888.github.io/teldrive/api/>
- rclone: this repository ships **no rclone backend**. `rclone config` will not offer a
  `teldrive` type unless you obtained one separately; what Teldrive provides is the HTTP API such
  a backend drives, authenticated with an API key created in **Settings → API keys**. See
  [Rclone setup](https://zhz8888.github.io/teldrive/rclone/setup)

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

Issues and pull requests are welcome at <https://github.com/zhz8888/teldrive>. Run `just check`
before opening a pull request; `AGENTS.md` is the entry point for working in this repository.

## License

Teldrive is released under the [MIT License](LICENSE).

- Original project — Copyright © 2024 [divyam234](https://github.com/divyam234)
  ([tgdrive/teldrive](https://github.com/tgdrive/teldrive)).
- This fork and the changes in it — Copyright © 2026
  [zhz8888](https://github.com/zhz8888).

## Acknowledgements

This fork builds on the original [tgdrive/teldrive](https://github.com/tgdrive/teldrive) by
[divyam234](https://github.com/divyam234) and its contributors. The upstream copyright notice and
the MIT license are kept unchanged; every fix, feature and rewrite this fork carries is work on
top of that foundation.
