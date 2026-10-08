#!/usr/bin/env bash
# Print the container runtime the harnesses should drive: docker or podman.
#
# TELDRIVE_CONTAINER_RUNTIME overrides the choice, which is also how CI pins the
# runtime it has preinstalled. Without an override the first runtime whose engine
# actually answers wins, docker before podman because CI runners and Docker
# Desktop hosts ship it. A binary on PATH is deliberately not enough: a podman
# install without a machine answers `podman info` with an error, and picking it
# on that basis is the failure this check exists to prevent.
set -euo pipefail

# require_engine checks that the named runtime is both installed and answering,
# and explains which of the two is missing on stderr. It is used for an explicit
# TELDRIVE_CONTAINER_RUNTIME, where a silent fallback would run the wrong engine.
require_engine() {
  local runtime="$1"
  command -v "$runtime" >/dev/null 2>&1 || {
    echo "$runtime is not on PATH" >&2
    return 1
  }
  "$runtime" info >/dev/null 2>&1 || {
    echo "$runtime is installed but its engine does not answer; start it (Docker Desktop, or 'podman machine start') or set TELDRIVE_CONTAINER_RUNTIME to a runtime that is up" >&2
    return 1
  }
}

if [[ -n "${TELDRIVE_CONTAINER_RUNTIME:-}" ]]; then
  runtime="${TELDRIVE_CONTAINER_RUNTIME}"
  require_engine "$runtime" || exit 1
  printf '%s' "$runtime"
  exit 0
fi

for candidate in docker podman; do
  if command -v "$candidate" >/dev/null 2>&1 && "$candidate" info >/dev/null 2>&1; then
    printf '%s' "$candidate"
    exit 0
  fi
done

echo "no container runtime is available: start Docker Desktop, or create and start a podman machine, or set TELDRIVE_CONTAINER_RUNTIME" >&2
exit 1
