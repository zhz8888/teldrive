# Repository Guide

## Sources Of Truth

- Use `justfile` for supported workflows; it loads a root `.env` automatically.
- TypeSpec files in `typespec/` own the HTTP contract. Do not hand-edit `openapi/teldrive.openapi.yaml`, `internal/api/gen/`, or `ui/src/api/schema.ts`; run `just generate-api`, `just generate-ui`, or `just generate` as appropriate.
- SQL lives in `db/queries/` and migrations in `db/migrations/`. Do not edit `internal/db/sqlcgen/` manually.
- Always run `just generate-db` after SQL changes. A bare `sqlc generate` omits the required `go run ./internal/tools/patchsqlc` step and breaks configurable PostgreSQL schema rewriting in `internal/db/sqlcgen/db.go`.
- Preserve `/* TEMPLATE: schema */` markers in SQL; the generated DB wrapper replaces them with the configured schema at runtime.

## Commands

- Run locally: `just dev` (backend plus the Vite dev server). `teldrive check` pre-flight-loads the configuration, applies migrations and initializes every dependency, then exits.
- Install pinned JS dependencies: `just install-tools` (uses Bun for both `typespec/` and `ui/`).
- Backend unit tests: `go test ./...`; focused package/test: `go test ./internal/transfer -run '^TestName$'`.
- Integration tests require Podman and must use the harness: `scripts/test-postgres.sh go test -tags=integration ./internal/uploads`; use `just test-integration` for all packages.
- Race tests also require the PostgreSQL harness: `just test-race`.
- Full project validation: `just check`. This regenerates artifacts and runs lint, UI checks/build, unit tests, and the Podman-backed 78% core coverage gate; it is intentionally expensive.
- UI checks: `just ui-check` (includes browser E2E with mocked API responses).
- Documentation site: `just docs-build`; `just check` builds it too, so a docs change fails the gate.
- Format only handwritten code with `just format`; generated Go directories are deliberately excluded.

## Configuration

- Start from `config.sample.yaml` or `config.sample.toml`. Settings are read from the file, then `TELDRIVE_*` environment variables, then flags; the justfile loads a root `.env` for every recipe.

## Testing

- Mutation-check guard tests: delete the guard line and confirm the test fails. Validation tests are the ones that keep passing while short-circuiting on an upstream guard, so a new guard test is unfinished until this has been done.
- Coverage is measured, not aspirational; raise the floor only with real coverage.
- Integration tests must go through `scripts/test-postgres.sh`. A bare `go test -tags=integration` fails on a missing `TEST_DATABASE_URL` and reads as a real failure.
- `river.Job` embeds `*rivertype.JobRow`, so a literal without it panics on the first `job.ID` read; give a worker its client with `rivertest.WorkContext(ctx, runtime.client.Client)`.
- A `river_job` fixture in a finalized state also needs `finalized_at`, and jobs land in the queue named by the args' `InsertOpts()`, not the default queue. Queue listings need a `river_queue` row seeded as well.
- A zero-valued service does not test an "unconfigured" guard: `sqlcgen.New(nil)` returns a usable `*Queries`, so arrange for the real dependency to be missing instead.
- PostgreSQL `jsonb` reformats whitespace; compare parsed values rather than strings.
- `users` carries `disabled_at timestamptz`, not a boolean.
- A visual-regression change needs baselines for both platforms: Playwright suffixes each capture with `-darwin` or `-linux`, CI runs on Linux and contributors on macOS, so a new surface means committing both sets or the suite fails on arrival.

## Architecture

- `cmd/teldrive` is the CLI entrypoint; `internal/app/app.go` is the composition root and owns migrations, services, HTTP routing, workers, and shutdown order.
- `internal/api` adapts the ogen contract; domain behavior belongs in packages such as `catalog`, `uploads`, `transfer`, `fileops`, and `shares`, not generated handlers.
- `internal/telegramstore` is the external storage boundary. Production uses gotd; integration and UI tests can use the filesystem backend.
- `db/migrations` includes application schema; startup also runs River/RiverPro migrations before opening the long-lived pool.
- The UI is a Vite/React app in `ui/`; `ui/ui.go` embeds `ui/dist`, so `just build` builds the UI before compiling the server binary.

## Generated Changes

- API generation intentionally fails if ogen emits an unimplemented-handler fallback or operation counts diverge; implement every generated handler method explicitly.
- Review generated diffs after `just generate`; generation can touch Go API code, SQL query code, OpenAPI, and the UI schema together.
