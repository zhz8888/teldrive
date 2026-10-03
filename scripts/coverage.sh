#!/usr/bin/env bash
set -euo pipefail

minimum="${COVERAGE_MIN:-78}"
profile="${COVERAGE_PROFILE:-coverage.out}"

# The percentage gate measures deterministic business logic. The full test
# suite still runs, including generated-router, application-lifecycle, and
# boundary-adapter tests. Generated code, composition-only wiring, and the live
# gotd RPC adapter are not denominator padding for the core-logic metric.
#
# The list is the set of packages the unit suite is expected to cover in full.
# Packages exercised mainly through the Podman-backed harness, such as the event
# stream (which needs a live LISTEN connection) and the small cache and size
# helpers that other packages cover through their own tests, are outside the
# floor, so the number below describes the packages listed here and nothing more.
#
# The floor is 78 because that is what the suite actually measures, not an
# aspiration: the largest uncovered block left in these packages is five
# statements, and roughly nine hundred of the remainder are single-statement
# error branches, each of which needs its own failure injection. Chasing them
# with narrow tests produced tests that passed while the guard they named was
# deleted, so the number below is only meaningful alongside the mutation check
# described in AGENTS.md. Raise the floor when real coverage is added, never by
# lowering it to make a red run green.
core_patterns=(
  ./internal/authn
  ./internal/bots
  ./internal/catalog
  ./internal/channels
  ./internal/config
  ./internal/contentcrypto
  ./internal/database
  ./internal/dbtypes
  ./internal/fileops
  ./internal/health
  ./internal/jobs
  ./internal/principal
  ./internal/secureblob
  ./internal/shares
  ./internal/transfer
  ./internal/treehash
  ./internal/uploads
)
# Read the package list line by line instead of using mapfile, which is a bash 4
# builtin and therefore missing from the bash 3.2 that macOS ships.
core_packages=()
while IFS= read -r core_package; do
  core_packages+=("$core_package")
done < <(go list "${core_patterns[@]}")
coverpkg="$(IFS=,; echo "${core_packages[*]}")"

./scripts/test-postgres.sh go test \
  -tags=integration \
  -covermode=atomic \
  -coverpkg="$coverpkg" \
  -coverprofile="$profile" \
  ./...

total="$(go tool cover -func="$profile" | awk '/^total:/ {gsub(/%/, "", $3); print $3}')"
printf 'core business coverage: %s%% (minimum %s%%)\n' "$total" "$minimum"

awk -v total="$total" -v minimum="$minimum" 'BEGIN {
  if ((total + 0) < (minimum + 0)) {
    printf "coverage %.1f%% is below required %.1f%%\n", total, minimum > "/dev/stderr"
    exit 1
  }
}'
