set dotenv-load := true

# sqlc_version is the generator version the committed query layer was produced
# with; generate-db refuses to run with another one.
sqlc_version := "v1.31.1"
openapi_spec := "openapi/teldrive.openapi.yaml"
ui_dir := "ui"
docs_dir := "docs"
binary := "bin/teldrive"
version := env_var_or_default("VERSION", "dev")
default_commit := `git rev-parse --short HEAD 2>/dev/null || echo unknown`
default_build_date := `date -u +%Y-%m-%dT%H:%M:%SZ`
commit := env_var_or_default("COMMIT", default_commit)
build_date := env_var_or_default("BUILD_DATE", default_build_date)
ldflags := "-s -w -X main.version=" + version + " -X main.commit=" + commit + " -X main.date=" + build_date

_default:
    @just --list

install-tools:
    bun ci --cwd typespec
    bun ci --cwd {{ui_dir}}
    bun ci --cwd {{docs_dir}}

format:
    bun run --cwd typespec format
    bun run --cwd {{ui_dir}} format
    gofmt -w $(find . -name '*.go' -not -path './internal/api/gen/*' -not -path './internal/db/sqlcgen/*')

lint:
    bun run --cwd typespec lint
    bun run --cwd {{ui_dir}} lint
    go vet ./...

# TypeSpec is the source of truth for the HTTP contract.
generate-openapi:
    bun run --cwd typespec generate:openapi

# Generate the complete Go server/client contract.
#
# The generator is the version go.mod pins, so regenerating an unchanged tree
# reproduces the committed artifacts instead of following the newest release.
generate-api: generate-openapi
    go run github.com/ogen-go/ogen/cmd/ogen \
        --config ogen.yml \
        --target internal/api/gen \
        --package gen \
        --clean \
        {{openapi_spec}}
    test ! -e internal/api/gen/oas_unimplemented_gen.go
    test "$(grep -c '^\s*[A-Z][A-Za-z0-9]*(ctx context.Context' internal/api/gen/oas_server_gen.go)" -eq "$(grep -c 'operationId:' {{openapi_spec}})"

# Refuse to run the generator with a sqlc other than the pinned one.
check-sqlc-version:
    # generate-db runs this before clean-db: a version mismatch has to fail while
    # the tree is still intact, because deleting the generated package first
    # leaves a workspace that cannot compile. sqlc rewrites the whole generated
    # package, so a version other than the one the committed files were produced
    # with would show up as a large unrelated diff and can even make patchsqlc
    # fail to match what it patches.
    test "$(sqlc version)" = "{{sqlc_version}}" || { echo "sqlc {{sqlc_version}} is required, found $(sqlc version)" >&2; exit 1; }

# Remove the files sqlc owns in its output directory. schema_template.go and its
# test are handwritten and are left alone; everything else there is regenerated.
clean-db:
    rm -f internal/db/sqlcgen/db.go internal/db/sqlcgen/models.go internal/db/sqlcgen/querier.go
    find internal/db/sqlcgen -name '*.sql.go' -delete

# Generate the typed PostgreSQL query layer.
generate-db: check-sqlc-version clean-db
    sqlc generate
    go run ./internal/tools/patchsqlc

generate-ui: generate-openapi
    bun run --cwd {{ui_dir}} generate:api

# Generate documentation derived from server configuration and the API contract.
docs-generate: generate-openapi
    go run ./internal/tools/docsconfig

# The repository tracks every generated artifact and CI verifies the committed
# ones against a fresh run. ui/src/routeTree.gen.ts is the exception to the
# recipe list: the TanStack Router plugin owns it, so `ui build` rewrites it and
# the diff after `check` is what keeps the committed copy honest.
generate: generate-api generate-db generate-ui docs-generate nix-generate
    go mod tidy

ui-check: generate-ui
    bun run --cwd {{ui_dir}} lint
    bun run --cwd {{ui_dir}} typecheck
    bun run --cwd {{ui_dir}} test
    bun run --cwd {{ui_dir}} build

# Run the fully static Astro + Fumadocs documentation site.
docs-dev: docs-generate
    bun run --cwd {{docs_dir}} dev

docs-build: docs-generate
    bun run --cwd {{docs_dir}} build

build: generate-ui
    bun run --cwd {{ui_dir}} build
    mkdir -p bin
    CGO_ENABLED=0 go build -trimpath -ldflags '{{ldflags}}' -o {{binary}} ./cmd/teldrive

# Compile the UI bundle the server embeds.
ui-build:
    # ui/ui.go embeds ui/dist through //go:embed all:dist, and the repository
    # tracks only ui/dist/.gitkeep so that a fresh checkout compiles before the
    # interface has ever been built. Every recipe that runs the Go suite has to
    # compile the bundle first: without it internal/app's embedded-UI guard fails
    # with "inspect embedded UI index: open index.html: file does not exist",
    # which is what a gate that ran its tests before its build hit on CI.
    # Depending on this recipe instead of repeating the command also keeps `just`
    # from building the bundle twice in one invocation.
    bun run --cwd {{ui_dir}} build

# Regenerate the Nix module options from the Go config structs.
nix-generate:
    go run ./internal/tools/nixconfig

# Re-pin the UI node_modules fixed-output hash after a package.json/bun.lock
# change.
#
# ui/bun.lock pins per-OS and per-CPU binaries, so nix/ui.nix carries one hash
# per system and this recipe rewrites the entry for the machine it runs on. The
# dependency install stops with the hash it computed; that value is written back
# here, so the next `nix build` verifies the pin instead of failing.
update-ui-deps-hash:
    #!/usr/bin/env bash
    set -euo pipefail
    system=$(nix eval --impure --raw --expr 'builtins.currentSystem')
    echo "→ node_modules fixed-output hash for ${system}..."
    log=$(mktemp)
    trap 'rm -f "$log"' EXIT
    nix build .#teldrive --no-link >"$log" 2>&1 || true
    tail -n 15 "$log"
    # Read the hash out of the mismatch report for the UI node_modules
    # derivation only: a Go vendorHash mismatch prints a `got:` line as well,
    # and the two are indistinguishable by position.
    got=$(awk '
      /hash mismatch in fixed-output derivation/ {
        want = index($0, "teldrive-ui-node_modules") > 0
        next
      }
      want && /got:/ {
        sub(/.*got:[ \t]*/, "")
        gsub(/[ \t\r]+$/, "")
        print
        exit
      }
    ' "$log")
    if [ -z "$got" ]; then
      echo "no node_modules mismatch reported — nothing to re-pin for ${system}" >&2
      exit 0
    fi
    # Rewrite through a temp file so the recipe works with both GNU and BSD sed.
    tmp=$(mktemp)
    sed -E "s|\"${system}\" = [^;]*;|\"${system}\" = \"${got}\";|" nix/ui.nix > "$tmp"
    mv "$tmp" nix/ui.nix
    echo "  ${system} = ${got}"
    echo "done — re-run nix build .#teldrive to verify"

# Fast re-pin of the Go vendor hash without a full `nix build`.
# Uses the nixpkgs-provided toolchain so the pinned hash always matches
# what `nix build` will see — no host-toolchain drift, no content
# mismatch. Still seconds, not minutes (only vendors). The hash lives in
# nix/package.nix; the UI dependencies carry their own per-system hash in
# nix/ui.nix, pinned by `just update-ui-deps-hash`.
update-flake-hashes:
    #!/usr/bin/env bash
    set -euo pipefail
    echo "→ vendorHash (nixpkgs go + nix hash)..."
    rm -rf vendor
    trap 'rm -rf vendor' EXIT
    nix shell nixpkgs#go --command go mod vendor
    vendor_hash=$(nix hash path --sri vendor)
    rm -rf vendor
    trap - EXIT
    # Rewrite through a temp file so the recipe works with both GNU and BSD sed.
    tmp=$(mktemp)
    sed "s|vendorHash = \"[^\"]*\";|vendorHash = \"${vendor_hash}\";|" nix/package.nix > "$tmp"
    mv "$tmp" nix/package.nix
    echo "  vendorHash = ${vendor_hash}"
    echo "done — nix/package.nix pinned (nixpkgs toolchain)"

dev:
    #!/usr/bin/env bash
    set -euo pipefail

    backend_pid=""
    ui_pid=""

    cleanup() {
        trap - INT TERM EXIT
        if [ -n "$ui_pid" ]; then
            kill -TERM "$ui_pid" 2>/dev/null || true
        fi
        if [ -n "$backend_pid" ]; then
            kill -TERM "$backend_pid" 2>/dev/null || true
        fi
        if [ -n "$ui_pid" ]; then
            wait "$ui_pid" 2>/dev/null || true
        fi
        if [ -n "$backend_pid" ]; then
            wait "$backend_pid" 2>/dev/null || true
        fi
    }

    trap 'cleanup; exit 130' INT
    trap 'cleanup; exit 143' TERM
    trap cleanup EXIT

    go run ./cmd/teldrive run &
    backend_pid=$!


    bun run --cwd {{ui_dir}} dev -- --host &
    ui_pid=$!

    # `wait -n` with operands needs bash 5.1, but this recipe's shebang resolves
    # to the bash 3.2 that macOS ships, where it fails with "wait: -n: invalid
    # option" and would abort the recipe right after both servers start. Poll
    # instead: the first child that is gone ends the loop, and its status is
    # what the recipe reports, so either shell behaves the same.
    status=0
    while kill -0 "$backend_pid" 2>/dev/null && kill -0 "$ui_pid" 2>/dev/null; do
        sleep 0.2
    done
    for child_pid in "$backend_pid" "$ui_pid"; do
        if ! kill -0 "$child_pid" 2>/dev/null; then
            wait "$child_pid" || status=$?
            break
        fi
    done

    cleanup
    exit "$status"

# The container runtime is whichever engine answers: docker or podman. Override
# with TELDRIVE_CONTAINER_RUNTIME when both are installed and the choice matters.
image:
    "$(./scripts/container-runtime.sh)" build --build-arg VERSION={{version}} --build-arg COMMIT={{commit}} --build-arg BUILD_DATE={{build_date}} -t teldrive-backend:{{version}} .

# Run the Go unit suite; the embedded UI has to exist first, so build it.
test-unit: ui-build
    go test ./...

test-integration: ui-build
    ./scripts/test-postgres.sh go test -tags=integration ./...

test-race: ui-build
    ./scripts/test-postgres.sh go test -race -tags=integration ./...

coverage: ui-build
    ./scripts/coverage.sh

# Run the full gate a contributor and CI both use.
check: generate lint test-unit coverage
    # The UI build comes from test-unit's ui-build dependency, before the Go suite
    # runs: it is what rewrites the tracked route tree the drift check compares, and
    # the Go suite cannot pass without the bundle it embeds.
    bun run --cwd {{ui_dir}} typecheck
    bun run --cwd {{ui_dir}} test
    bun run --cwd {{docs_dir}} build

clean-generated: clean-db
    rm -rf openapi internal/api/gen ui/src/api/schema.ts ui/dist coverage.out
