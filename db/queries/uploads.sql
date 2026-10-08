-- CreateUploadSession opens a new upload session in the 'open' state and returns it;
-- parent_id, hashes, MIME type and key version are optional, and a NULL parent means
-- the drive root.
-- name: CreateUploadSession :one
INSERT INTO /* TEMPLATE: schema */upload_sessions (
    id,
    user_id,
    parent_id,
    name,
    expected_size,
    expected_hash_algorithm,
    expected_hash_value,
    mime_type,
    mod_time,
    encryption,
    encryption_key_version,
    conflict_policy,
    part_size,
    state,
    expires_at
) VALUES (
    sqlc.arg(id),
    sqlc.arg(user_id),
    sqlc.narg(parent_id),
    sqlc.arg(name),
    sqlc.arg(expected_size),
    sqlc.narg(expected_hash_algorithm),
    sqlc.narg(expected_hash_value),
    sqlc.narg(mime_type),
    sqlc.arg(mod_time),
    sqlc.arg(encryption),
    sqlc.narg(encryption_key_version),
    sqlc.arg(conflict_policy),
    sqlc.arg(part_size),
    'open',
    sqlc.arg(expires_at)
)
RETURNING *;

-- GetUploadSessionForUser returns one upload session by id only when it belongs to the
-- user, whatever its state.
-- name: GetUploadSessionForUser :one
SELECT *
FROM /* TEMPLATE: schema */upload_sessions
WHERE id = sqlc.arg(upload_id)
  AND user_id = sqlc.arg(user_id);


-- GetUploadSessionAnyOwner returns an upload session by id without checking the owner;
-- callers must authorize against its UserID or its parent file before acting on it.
-- name: GetUploadSessionAnyOwner :one
SELECT *
FROM /* TEMPLATE: schema */upload_sessions
WHERE id = sqlc.arg(upload_id);

-- ListUploadSessions returns one page of the user's upload sessions, newest first,
-- optionally limited to a single state, keyset-paged on (created_at, id).
-- name: ListUploadSessions :many
SELECT *
FROM /* TEMPLATE: schema */upload_sessions
WHERE user_id = sqlc.arg(user_id)
  AND (sqlc.narg(state)::/* TEMPLATE: schema */upload_state IS NULL OR state = sqlc.narg(state)::/* TEMPLATE: schema */upload_state)
  AND (
    sqlc.narg(after_created_at)::timestamptz IS NULL
    OR (created_at, id) < (
      sqlc.narg(after_created_at)::timestamptz,
      sqlc.narg(after_id)::uuid
    )
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_size);

-- FindResumableUploadSessions finds the user's open, unexpired replace-uploads of the
-- same destination, name, size, encryption and MIME type, newest first; a supplied
-- mod_time must match within one second, which absorbs timestamp rounding.
-- name: FindResumableUploadSessions :many
SELECT *
FROM /* TEMPLATE: schema */upload_sessions
WHERE user_id = sqlc.arg(user_id)
  AND parent_id IS NOT DISTINCT FROM sqlc.narg(parent_id)::uuid
  AND name = sqlc.arg(name)
  AND expected_size = sqlc.arg(expected_size)
  AND encryption = sqlc.arg(encryption)
  AND conflict_policy = 'replace'
  AND expires_at > now()
  AND state = 'open'
  AND mime_type IS NOT DISTINCT FROM sqlc.narg(mime_type)::text
  AND (
    NOT sqlc.arg(has_mod_time)::boolean
    OR abs(extract(epoch FROM (mod_time - sqlc.arg(mod_time)::timestamptz))) <= 1
  )
ORDER BY created_at DESC, id DESC;

-- GetUploadPart returns one part of an upload by number, whatever its state, or no row
-- when the part was never claimed.
-- name: GetUploadPart :one
SELECT *
FROM /* TEMPLATE: schema */upload_parts
WHERE upload_id = sqlc.arg(upload_id)
  AND part_no = sqlc.arg(part_no);

-- ClaimUploadPart inserts the part or re-claims it for uploading with a new lease and
-- returns the row; a part that is already stored, or whose lease is still running, is
-- not re-claimed and returns no row.
-- name: ClaimUploadPart :one
-- The lease is granted from the database clock, which is also the clock the
-- conflict predicate below reads: a lease written from the application clock
-- would already be expired whenever the two drift apart.
INSERT INTO /* TEMPLATE: schema */upload_parts (
    upload_id,
    part_no,
    channel_id,
    plain_size,
    checksum,
    state,
    lease_token,
    lease_expires_at
) VALUES (
    sqlc.arg(upload_id),
    sqlc.arg(part_no),
    sqlc.arg(channel_id),
    sqlc.arg(plain_size),
    sqlc.narg(checksum),
    'uploading',
    sqlc.arg(lease_token),
    now() + make_interval(secs => sqlc.arg(lease_seconds)::int)
)
ON CONFLICT (upload_id, part_no) DO UPDATE
SET channel_id = EXCLUDED.channel_id,
    plain_size = EXCLUDED.plain_size,
    checksum = EXCLUDED.checksum,
    state = 'uploading',
    lease_token = EXCLUDED.lease_token,
    lease_expires_at = EXCLUDED.lease_expires_at,
    last_error_code = NULL,
    updated_at = now()
WHERE /* TEMPLATE: schema */upload_parts.state <> 'stored'
  AND (
    /* TEMPLATE: schema */upload_parts.lease_expires_at IS NULL
    OR /* TEMPLATE: schema */upload_parts.lease_expires_at < now()
  )
RETURNING *;

-- MarkUploadPartStored records the Telegram message, stored size and hashes of a part
-- and clears its lease, but only for the caller that still holds the lease of an
-- 'uploading' part; otherwise it returns no row.
-- name: MarkUploadPartStored :one
UPDATE /* TEMPLATE: schema */upload_parts
SET message_id = sqlc.arg(message_id),
    stored_size = sqlc.arg(stored_size),
    checksum = sqlc.narg(checksum),
    salt = sqlc.narg(salt),
    block_hashes = sqlc.narg(block_hashes),
    state = 'stored',
    lease_token = NULL,
    lease_expires_at = NULL,
    last_error_code = NULL,
    updated_at = now()
WHERE upload_id = sqlc.arg(upload_id)
  AND part_no = sqlc.arg(part_no)
  AND state = 'uploading'
  AND lease_token = sqlc.arg(lease_token)
RETURNING *;

-- RenewUploadPartLease pushes the part's deadline out by lease_seconds and returns
-- the rows changed, zero once the part was stored, failed or re-claimed elsewhere.
-- name: RenewUploadPartLease :execrows
-- The renewed deadline comes from the database clock, matching the claim.
UPDATE /* TEMPLATE: schema */upload_parts
SET lease_expires_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::int),
    updated_at = now()
WHERE upload_id = sqlc.arg(upload_id)
  AND part_no = sqlc.arg(part_no)
  AND state = 'uploading'
  AND lease_token = sqlc.arg(lease_token);

-- MarkUploadPartFailed marks a part the caller still leases as failed, clears its
-- lease and stores the error code; a lease that was lost returns no row.
-- name: MarkUploadPartFailed :one
UPDATE /* TEMPLATE: schema */upload_parts
SET state = 'failed',
    lease_token = NULL,
    lease_expires_at = NULL,
    last_error_code = sqlc.arg(error_code),
    updated_at = now()
WHERE upload_id = sqlc.arg(upload_id)
  AND part_no = sqlc.arg(part_no)
  AND lease_token = sqlc.arg(lease_token)
RETURNING *;

-- ListUploadParts returns one page of the upload's parts in part order, starting after
-- after_part_no when given, for clients that poll upload progress.
-- name: ListUploadParts :many
SELECT *
FROM /* TEMPLATE: schema */upload_parts
WHERE upload_id = sqlc.arg(upload_id)
  AND (
    sqlc.narg(after_part_no)::integer IS NULL
    OR part_no > sqlc.narg(after_part_no)::integer
  )
ORDER BY part_no
LIMIT sqlc.arg(page_size);

-- ListUploadPartsByUploadIDs returns every part of the given uploads in
-- (upload_id, part_no) order.
-- name: ListUploadPartsByUploadIDs :many
SELECT *
FROM /* TEMPLATE: schema */upload_parts
WHERE upload_id = ANY(sqlc.arg(upload_ids)::uuid[])
ORDER BY upload_id, part_no;

-- LockUploadSessionForCompletion row-locks one upload session of the user for the
-- transaction that turns it into a file, so two completions cannot run at once.
-- name: LockUploadSessionForCompletion :one
SELECT *
FROM /* TEMPLATE: schema */upload_sessions
WHERE id = sqlc.arg(upload_id)
  AND user_id = sqlc.arg(user_id)
FOR UPDATE;

-- MarkUploadCompleting moves an 'open' session of the user to 'completing' and returns
-- it; a session that is foreign or already past 'open' returns no row.
-- name: MarkUploadCompleting :one
UPDATE /* TEMPLATE: schema */upload_sessions
SET state = 'completing',
    updated_at = now()
WHERE id = sqlc.arg(upload_id)
  AND user_id = sqlc.arg(user_id)
  AND state = 'open'
RETURNING *;

-- FinalizeUploadExpectedSize records the real total size measured from the stored
-- parts when the client opened the session with the unknown-size sentinel -1, and
-- only for an 'open' session of the user.
-- name: FinalizeUploadExpectedSize :one
UPDATE /* TEMPLATE: schema */upload_sessions
SET expected_size = sqlc.arg(expected_size),
    updated_at = now()
WHERE id = sqlc.arg(upload_id)
  AND user_id = sqlc.arg(user_id)
  AND expected_size = -1
  AND state = 'open'
RETURNING *;

-- ListStoredUploadPartHashes returns the per-part block hashes of an upload's stored
-- parts in part order, which completion concatenates into the whole-file hash.
-- name: ListStoredUploadPartHashes :many
SELECT block_hashes
FROM /* TEMPLATE: schema */upload_parts
WHERE upload_id = sqlc.arg(upload_id)
  AND state = 'stored'
ORDER BY part_no;

-- InsertFileFromUpload creates the active file row from a session that is 'completing'
-- and owned by the user, copying its name, parent, size, encryption and mod_time, and
-- returns it; the hash columns come from the caller.
-- name: InsertFileFromUpload :one
INSERT INTO /* TEMPLATE: schema */files (
    id,
    user_id,
    parent_id,
    name,
    kind,
    mime_type,
    size,
    hash_algorithm,
    hash_value,
    encryption,
    encryption_key_version,
    status,
    mod_time
)
SELECT
    sqlc.arg(file_id),
    user_id,
    parent_id,
    name,
    'file',
    mime_type,
    expected_size,
    sqlc.narg(hash_algorithm),
    sqlc.narg(hash_value),
    encryption,
    encryption_key_version,
    'active',
    mod_time
FROM /* TEMPLATE: schema */upload_sessions us
WHERE us.id = sqlc.arg(upload_id)
  AND us.user_id = sqlc.arg(user_id)
  AND us.state = 'completing'
RETURNING *;

-- InsertFilePartsFromUpload copies the session's stored parts into file_parts in part
-- order and returns the number of rows inserted; unstored parts are skipped.
-- name: InsertFilePartsFromUpload :execrows
INSERT INTO /* TEMPLATE: schema */file_parts (
    file_id,
    part_no,
    channel_id,
    message_id,
    plain_size,
    stored_size,
    checksum,
    salt,
    block_hashes
)
SELECT
    sqlc.arg(file_id),
    part_no,
    channel_id,
    message_id,
    plain_size,
    stored_size,
    checksum,
    salt,
    block_hashes
FROM /* TEMPLATE: schema */upload_parts
WHERE upload_id = sqlc.arg(upload_id)
  AND state = 'stored'
ORDER BY part_no;

-- CompleteUploadSession marks a 'completing' session of the user as completed, links
-- the file it produced and stamps completed_at; any other state returns no row.
-- name: CompleteUploadSession :one
UPDATE /* TEMPLATE: schema */upload_sessions
SET state = 'completed',
    file_id = sqlc.arg(file_id),
    completed_at = now(),
    updated_at = now()
WHERE id = sqlc.arg(upload_id)
  AND user_id = sqlc.arg(user_id)
  AND state = 'completing'
RETURNING *;

-- AbortUploadSession aborts an open or completing session of the user and returns it;
-- a session that already completed, expired or aborted returns no row.
-- name: AbortUploadSession :one
UPDATE /* TEMPLATE: schema */upload_sessions
SET state = 'aborted',
    updated_at = now()
WHERE id = sqlc.arg(upload_id)
  AND user_id = sqlc.arg(user_id)
  AND state IN ('open', 'completing')
RETURNING *;

-- ExpireUploadSessions marks up to 1000 expired open or completing sessions as expired
-- and returns them; the candidates are locked with SKIP LOCKED so parallel sweeps do
-- not collide.
-- name: ExpireUploadSessions :many
UPDATE /* TEMPLATE: schema */upload_sessions
SET state = 'expired',
    updated_at = now()
WHERE id IN (
    SELECT id
    FROM /* TEMPLATE: schema */upload_sessions
    WHERE state IN ('open', 'completing')
      AND expires_at <= now()
    ORDER BY expires_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1000
)
RETURNING *;

-- ListUploadPartsForCleanupMany returns the parts of the given aborted or expired
-- uploads that still hold a Telegram message, ordered by upload, channel and part.
-- name: ListUploadPartsForCleanupMany :many
SELECT up.*
FROM /* TEMPLATE: schema */upload_parts AS up
JOIN /* TEMPLATE: schema */upload_sessions AS us ON us.id = up.upload_id
WHERE us.id = ANY(sqlc.arg(upload_ids)::uuid[])
  AND us.state IN ('aborted', 'expired')
  AND up.message_id IS NOT NULL
ORDER BY up.upload_id, up.channel_id, up.part_no;

-- ListUploadSessionsPendingCleanup returns up to 1000 aborted or expired sessions,
-- oldest update first, including sessions that have no stored part at all.
-- name: ListUploadSessionsPendingCleanup :many
-- Every finalized session is listed, not only the ones with a stored part:
-- a session whose parts were claimed but never stored, or that has no parts at
-- all, still has a row to remove.
SELECT *
FROM /* TEMPLATE: schema */upload_sessions us
WHERE us.state IN ('aborted', 'expired')
ORDER BY us.updated_at, us.id
LIMIT 1000;

-- DeleteUploadSessionsForCleanup deletes the given aborted or expired sessions and
-- returns the rows removed; their remaining parts go with them by ON DELETE CASCADE.
-- name: DeleteUploadSessionsForCleanup :execrows
-- Removing the session also removes its remaining part rows, because
-- upload_parts references the session with ON DELETE CASCADE; the caller has
-- already deleted the Telegram messages of the parts that held one.
DELETE FROM /* TEMPLATE: schema */upload_sessions
WHERE id = ANY(sqlc.arg(upload_ids)::uuid[])
  AND state IN ('aborted', 'expired');

-- DeleteUploadPartsForCleanup deletes the listed parts of aborted or expired sessions
-- by upload, part number and message id, so a part whose message id changed is left
-- alone.
-- name: DeleteUploadPartsForCleanup :execrows
DELETE FROM /* TEMPLATE: schema */upload_parts AS part
USING /* TEMPLATE: schema */upload_sessions AS session,
      jsonb_to_recordset(sqlc.arg(parts)::jsonb) AS cleanup_part(
        upload_id uuid, part_no integer, message_id bigint
      )
WHERE part.upload_id = cleanup_part.upload_id
  AND part.part_no = cleanup_part.part_no
  AND part.message_id = cleanup_part.message_id
  AND session.id = part.upload_id
  AND session.state IN ('aborted', 'expired');

-- LockUploadDestinationConflict row-locks the active file at the destination with this
-- name, NULL parent meaning the drive root, so the replace decision cannot race.
-- name: LockUploadDestinationConflict :one
SELECT *
FROM /* TEMPLATE: schema */files
WHERE user_id = sqlc.arg(user_id)
  AND parent_id IS NOT DISTINCT FROM sqlc.narg(parent_id)::uuid
  AND name = sqlc.arg(name)
  AND status = 'active'
FOR UPDATE;

-- MarkActiveFileDeletionPendingForReplace retires the file that a replace upload is
-- about to overwrite and returns the rows changed, which is zero when the file is no
-- longer active.
-- name: MarkActiveFileDeletionPendingForReplace :execrows
UPDATE /* TEMPLATE: schema */files
SET status = 'deletion_pending',
    deleted_at = COALESCE(deleted_at, now()),
    updated_at = now(),
    generation = generation + 1
WHERE id = sqlc.arg(file_id)
  AND user_id = sqlc.arg(user_id)
  AND status = 'active';

-- RevokeActiveSharesForFile revokes every live share the user created on one file,
-- which the replace flow does because the file it pointed at is being replaced.
-- name: RevokeActiveSharesForFile :exec
UPDATE /* TEMPLATE: schema */file_shares
SET revoked_at = COALESCE(revoked_at, now())
WHERE file_id = sqlc.arg(file_id)
  AND owner_id = sqlc.arg(user_id)
  AND revoked_at IS NULL;

-- RenameUploadSession changes the destination name of one session of the user and
-- returns the rows changed.
-- name: RenameUploadSession :execrows
UPDATE /* TEMPLATE: schema */upload_sessions
SET name = sqlc.arg(name),
    updated_at = now()
WHERE id = sqlc.arg(upload_id)
  AND user_id = sqlc.arg(user_id);

-- GetAllUploadPartSummary returns the part count, stored count and stored plain bytes
-- of an upload plus the lowest and highest stored part numbers, which are 0 while
-- nothing is stored yet.
-- name: GetAllUploadPartSummary :one
SELECT
    count(*)::bigint AS total_parts,
    count(*) FILTER (WHERE state = 'stored')::bigint AS stored_parts,
    COALESCE(sum(plain_size) FILTER (WHERE state = 'stored'), 0)::bigint AS stored_plain_size,
    COALESCE(min(part_no) FILTER (WHERE state = 'stored'), 0)::integer AS min_part_no,
    COALESCE(max(part_no) FILTER (WHERE state = 'stored'), 0)::integer AS max_part_no
FROM /* TEMPLATE: schema */upload_parts
WHERE upload_id = sqlc.arg(upload_id);

-- CountInvalidOpenEndedUploadParts counts the stored parts before the final one whose
-- plain size is not exactly part_size; a non-zero result means the stored parts do not
-- tile the file, so completion must fail.
-- name: CountInvalidOpenEndedUploadParts :one
WITH final_part AS (
    SELECT COALESCE(max(part_no), 0)::integer AS part_no
    FROM /* TEMPLATE: schema */upload_parts
    WHERE upload_id = sqlc.arg(upload_id)
      AND state = 'stored'
)
SELECT count(*)::bigint
FROM /* TEMPLATE: schema */upload_parts parts
CROSS JOIN final_part
WHERE parts.upload_id = sqlc.arg(upload_id)
  AND parts.state = 'stored'
  AND parts.part_no < final_part.part_no
  AND parts.plain_size <> sqlc.arg(part_size);

-- ListUploadDailyStatistics returns one row per day over the last days days for the
-- user, with the bytes and file count completed that day, zero-filled for empty days.
-- name: ListUploadDailyStatistics :many
WITH days AS (
  SELECT generate_series(
    (CURRENT_DATE - (sqlc.arg(days)::integer - 1))::date,
    CURRENT_DATE,
    interval '1 day'
  )::date AS day
), totals AS (
  SELECT completed_at::date AS day,
         COALESCE(sum(expected_size), 0)::bigint AS uploaded_bytes,
         count(*)::bigint AS completed_files
  FROM /* TEMPLATE: schema */upload_sessions
  WHERE user_id = sqlc.arg(user_id)
    AND state = 'completed'
    AND completed_at >= CURRENT_DATE - (sqlc.arg(days)::integer - 1)
  GROUP BY completed_at::date
)
SELECT d.day, COALESCE(t.uploaded_bytes, 0)::bigint AS uploaded_bytes,
       COALESCE(t.completed_files, 0)::bigint AS completed_files
FROM days d
LEFT JOIN totals t USING (day)
ORDER BY d.day;
