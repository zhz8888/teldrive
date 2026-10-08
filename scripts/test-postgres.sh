#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Docker and podman take the same run/exec/port/rm arguments here, so the only
# runtime-specific thing is which binary answers; see container-runtime.sh.
runtime="$("$script_dir/container-runtime.sh")"

image="${TELDRIVE_POSTGRES_IMAGE:-ghcr.io/tgdrive/postgres:18}"
name="teldrive-test-${USER:-user}-$$"
user="teldrive"
password="teldrive"
database="teldrive_test"

# cleanup removes the throwaway database container; it runs on every exit path,
# so a failed start must not be reported as a leaked container. Failures are
# ignored because the container may never have been created.
cleanup() {
  "$runtime" rm -f "$name" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# The empty host port is the one argument a reader may not expect: it asks the
# runtime for a free port on loopback. Verified on docker 29.8 and podman 6.1;
# if a future runtime rejects it, `127.0.0.1:0:5432` is the equivalent spelling.
"$runtime" run -d --name "$name" \
  -e POSTGRES_USER="$user" \
  -e POSTGRES_PASSWORD="$password" \
  -e POSTGRES_DB="$database" \
  -p 127.0.0.1::5432 \
  "$image" >/dev/null

port="$("$runtime" port "$name" 5432/tcp | awk -F: '{print $NF}')"
if [[ -z "$port" ]]; then
  echo "failed to determine PostgreSQL port" >&2
  "$runtime" logs "$name" >&2 || true
  exit 1
fi

# The official PostgreSQL entrypoint starts a temporary Unix-socket-only server
# during initialization and then restarts PostgreSQL normally. Probing the local
# socket can therefore report ready too early. Requiring a TCP SQL query avoids
# that restart race and verifies the configured user and database as well.
ready=false
for _ in $(seq 1 120); do
  if "$runtime" exec -e PGPASSWORD="$password" "$name" \
    psql -h 127.0.0.1 -U "$user" -d "$database" -Atqc 'SELECT 1' \
    2>/dev/null | grep -qx '1'; then
    ready=true
    break
  fi
  sleep 0.25
done

if [[ "$ready" != true ]]; then
  echo "PostgreSQL did not become ready" >&2
  "$runtime" logs "$name" >&2 || true
  exit 1
fi

export TEST_DATABASE_URL="postgres://${user}:${password}@127.0.0.1:${port}/${database}?sslmode=disable"

if [[ "$#" -eq 0 ]]; then
  set -- go test -tags=integration ./...
fi

"$@"
