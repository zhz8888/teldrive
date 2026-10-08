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

Release builds are standalone, statically linked binaries (`CGO_ENABLED=0`) for Linux, macOS and
Windows; Windows archives are `.zip`. Download the archive for your platform from this fork's
[GitHub Releases](https://github.com/zhz8888/teldrive/releases) and install the binary:

```bash
tar -xzf teldrive-vX.Y.Z-linux-amd64.tar.gz
sudo install -m 0755 teldrive /usr/local/bin/teldrive
teldrive version
```

Targets are Linux `amd64`/`arm`/`arm64`, macOS `amd64`/`arm64` and Windows `amd64`/`arm64`.
Archives are built when a `v*` tag is pushed, so if the Releases list is still empty, use the
container image above or build [from source](#from-source).

Write the configuration file — a database URL and both security keys are the minimum, every other
setting has a default, and `config.sample.yaml` lists them all:

```bash
sudo install -d -m 0750 /etc/teldrive
sudo tee /etc/teldrive/config.yaml >/dev/null <<YAML
http:
  address: 127.0.0.1:8080

database:
  url: postgres://teldrive:password@127.0.0.1:5432/teldrive?sslmode=require

security:
  signing-key: "$(openssl rand -hex 32)"
  data-key: "$(openssl rand -base64 32)"
YAML
```

The two keys are generated once, here: `signing-key` takes at least 32 characters and `data-key`
must decode to exactly 32 bytes. Back `data-key` up together with the content-encryption keys,
because losing them can make protected data unrecoverable. The role in `database.url` also has to
be able to create the `pgcrypto` and `pg_trgm` extensions in the `public` schema.

Validate the configuration and let the migrations run, then start the server:

```bash
teldrive check --config /etc/teldrive/config.yaml   # config, migrations, dependencies, then exit
teldrive run   --config /etc/teldrive/config.yaml   # serve the API and the UI (alias: serve)
```

Open <http://127.0.0.1:8080> and sign in with Telegram. To keep it running across reboots, give
the service an account of its own, write this unit to `/etc/systemd/system/teldrive.service`, and
start it:

```ini
[Unit]
Description=Teldrive
After=network-online.target postgresql.service
Wants=network-online.target

[Service]
Type=simple
User=teldrive
Group=teldrive
ExecStart=/usr/local/bin/teldrive run --config /etc/teldrive/config.yaml
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin teldrive
sudo chown root:teldrive /etc/teldrive/config.yaml
sudo chmod 0640 /etc/teldrive/config.yaml
sudo systemctl enable --now teldrive
```

Every release target, the listener choice and the reverse-proxy advice are on the
[Release binary](https://zhz8888.github.io/teldrive/installation/binary) page.

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
