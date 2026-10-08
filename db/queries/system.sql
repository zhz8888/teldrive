-- AcquireAdvisoryLock blocks until the session-level advisory lock for lock_id is
-- free and then takes it; the lock stays on the connection until ReleaseAdvisoryLock.
-- name: AcquireAdvisoryLock :exec
SELECT pg_advisory_lock(sqlc.arg(lock_id));

-- ReleaseAdvisoryLock releases a session-level advisory lock and reports whether this
-- connection held it; false means the lock was already gone.
-- name: ReleaseAdvisoryLock :one
SELECT pg_advisory_unlock(sqlc.arg(lock_id));

-- TryAdvisoryLock takes the session-level advisory lock only if it is free and
-- reports whether it got it, without waiting, so a busy lock returns false.
-- name: TryAdvisoryLock :one
SELECT pg_try_advisory_lock(sqlc.arg(lock_id));

-- TryAdvisoryLocks tries every id in lock_ids without waiting and returns one row per
-- id with whether that lock was taken; the caller must release the ones it got.
-- name: TryAdvisoryLocks :many
SELECT candidate.lock_id::bigint AS lock_id, pg_try_advisory_lock(candidate.lock_id) AS locked
FROM unnest(sqlc.arg(lock_ids)::bigint[]) AS candidate(lock_id);

-- AcquireAdvisoryTransactionLock takes a transaction-scoped advisory lock, waiting if
-- it is busy; PostgreSQL releases it when the transaction ends.
-- name: AcquireAdvisoryTransactionLock :exec
SELECT pg_advisory_xact_lock(sqlc.arg(lock_id));
