-- GetStorageDashboardTotals returns the user's active byte total with the active
-- file, active folder, trashed file and trash byte counts, zero where nothing matches.
-- name: GetStorageDashboardTotals :one
SELECT
    COALESCE(sum(f.size) FILTER (WHERE f.kind = 'file' AND f.status = 'active'), 0)::bigint AS logical_bytes,
    count(*) FILTER (WHERE f.kind = 'file' AND f.status = 'active')::bigint AS active_files,
    count(*) FILTER (WHERE f.kind = 'folder' AND f.status = 'active')::bigint AS active_folders,
    count(*) FILTER (WHERE f.kind = 'file' AND f.status = 'trashed')::bigint AS trashed_files,
    COALESCE(sum(f.size) FILTER (WHERE f.kind = 'file' AND f.status = 'trashed'), 0)::bigint AS trash_bytes
FROM /* TEMPLATE: schema */files f
WHERE f.user_id = sqlc.arg(user_id);

-- GetStorageCleanupStatistics returns the bytes held by the user's trashed files and
-- by expired open or completing uploads that still have stored parts, plus the count
-- of those expired uploads.
-- name: GetStorageCleanupStatistics :one
SELECT
    COALESCE((
        SELECT sum(fp.stored_size)
        FROM /* TEMPLATE: schema */file_parts fp
        JOIN /* TEMPLATE: schema */files f ON f.id = fp.file_id
        WHERE f.user_id = sqlc.arg(user_id)
          AND f.kind = 'file'
          AND f.status = 'trashed'
    ), 0)::bigint AS trash_bytes,
    COALESCE((
        SELECT sum(up.stored_size)
        FROM /* TEMPLATE: schema */upload_parts up
        JOIN /* TEMPLATE: schema */upload_sessions us ON us.id = up.upload_id
        WHERE us.user_id = sqlc.arg(user_id)
          AND us.state IN ('open', 'completing')
          AND us.expires_at <= now()
          AND up.state = 'stored'
    ), 0)::bigint AS stale_upload_bytes,
    COALESCE((
        SELECT count(*)
        FROM /* TEMPLATE: schema */upload_sessions us
        WHERE us.user_id = sqlc.arg(user_id)
          AND us.state IN ('open', 'completing')
          AND us.expires_at <= now()
    ), 0)::bigint AS stale_uploads;

-- ListStorageGrowth returns the last 30 days, one row per day with the bytes added
-- that day and the running logical total, which starts from everything created before
-- the window.
-- name: ListStorageGrowth :many
WITH days AS (
    SELECT generate_series(
        date_trunc('day', now()) - interval '29 days',
        date_trunc('day', now()),
        interval '1 day'
    )::date AS day
), daily AS (
    SELECT
        f.created_at::date AS day,
        COALESCE(sum(f.size), 0)::bigint AS added_bytes
    FROM /* TEMPLATE: schema */files f
    WHERE f.user_id = sqlc.arg(user_id)
      AND f.kind = 'file'
      AND f.status = 'active'
      AND f.created_at >= date_trunc('day', now()) - interval '29 days'
    GROUP BY f.created_at::date
), baseline AS (
    SELECT COALESCE(sum(f.size), 0)::bigint AS bytes
    FROM /* TEMPLATE: schema */files f
    WHERE f.user_id = sqlc.arg(user_id)
      AND f.kind = 'file'
      AND f.status = 'active'
      AND f.created_at < date_trunc('day', now()) - interval '29 days'
)
SELECT
    days.day,
    COALESCE(daily.added_bytes, 0)::bigint AS added_bytes,
    (
        baseline.bytes + sum(COALESCE(daily.added_bytes, 0)) OVER (ORDER BY days.day)
    )::bigint AS logical_bytes
FROM days
CROSS JOIN baseline
LEFT JOIN daily USING (day)
ORDER BY days.day;

-- ListStorageChannelStatistics returns each channel of the user with its health and
-- the number and stored size of the parts it holds for that user's active files,
-- selected channels first and then by descending stored size.
-- name: ListStorageChannelStatistics :many
SELECT
    c.channel_id,
    c.name,
    c.selected,
    c.health::text AS health,
    c.last_checked_at,
    COALESCE(count(f.id), 0)::bigint AS part_count,
    COALESCE(sum(fp.stored_size) FILTER (WHERE f.id IS NOT NULL), 0)::bigint AS stored_bytes
FROM /* TEMPLATE: schema */channels c
LEFT JOIN /* TEMPLATE: schema */file_parts fp ON fp.channel_id = c.channel_id
LEFT JOIN /* TEMPLATE: schema */files f ON f.id = fp.file_id AND f.user_id = c.user_id AND f.status = 'active'
WHERE c.user_id = sqlc.arg(user_id)
GROUP BY c.channel_id, c.name, c.selected, c.health, c.last_checked_at
ORDER BY c.selected DESC, stored_bytes DESC, c.channel_id;

-- ListRecentStorageActivity returns the user's newest file, upload, share and channel
-- events, up to activity_limit, newest first; other event types are never returned.
-- name: ListRecentStorageActivity :many
SELECT id, event_type, resource_type, resource_id, payload, occurred_at
FROM /* TEMPLATE: schema */user_events
WHERE user_id = sqlc.arg(user_id)
  AND event_type IN (
    'file.created', 'file.trashed', 'file.restored', 'file.purged',
    'upload.completed', 'upload.aborted', 'upload.expired',
    'share.created', 'share.deleted',
    'channel.created', 'channel.updated', 'channel.deleted'
  )
ORDER BY id DESC
LIMIT sqlc.arg(activity_limit);
