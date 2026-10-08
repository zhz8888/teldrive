-- ListChannels returns one page of the user's channels, newest first, keyset-paged
-- on (created_at, channel_id) so paging stays stable while rows are inserted.
-- name: ListChannels :many
SELECT *
FROM /* TEMPLATE: schema */channels
WHERE user_id = sqlc.arg(user_id)
  AND (
    sqlc.narg(after_created_at)::timestamptz IS NULL
    OR (created_at, channel_id) < (
      sqlc.narg(after_created_at)::timestamptz,
      sqlc.narg(after_channel_id)::bigint
    )
  )
ORDER BY created_at DESC, channel_id DESC
LIMIT sqlc.arg(page_size);

-- GetChannelForUser returns the user's own channel row, so an id belonging to
-- another account returns no row.
-- name: GetChannelForUser :one
SELECT *
FROM /* TEMPLATE: schema */channels
WHERE user_id = sqlc.arg(user_id)
  AND channel_id = sqlc.arg(channel_id);

-- GetSelectedChannel returns the single channel the user selected as the upload
-- destination; a user without one gets no row.
-- name: GetSelectedChannel :one
SELECT *
FROM /* TEMPLATE: schema */channels
WHERE user_id = sqlc.arg(user_id)
  AND selected;

-- CreateChannel inserts a channel for the user, always unselected, and returns it;
-- health stays at its default until the first check.
-- name: CreateChannel :one
INSERT INTO /* TEMPLATE: schema */channels (
    channel_id,
    user_id,
    name,
    selected
) VALUES (
    sqlc.arg(channel_id),
    sqlc.arg(user_id),
    sqlc.arg(name),
    FALSE
)
RETURNING *;

-- ClearSelectedChannel deselects whichever channel the user currently has selected,
-- which the one-selected-per-user index requires before another can be promoted.
-- name: ClearSelectedChannel :exec
UPDATE /* TEMPLATE: schema */channels
SET selected = FALSE,
    updated_at = now()
WHERE user_id = sqlc.arg(user_id)
  AND selected;

-- SelectChannel marks one of the user's channels as selected and returns it; the
-- caller clears the previous selection first, and an unknown id returns no row.
-- name: SelectChannel :one
UPDATE /* TEMPLATE: schema */channels
SET selected = TRUE,
    updated_at = now()
WHERE user_id = sqlc.arg(user_id)
  AND channel_id = sqlc.arg(channel_id)
RETURNING *;

-- UpdateChannelHealth stores the latest health verdict and stamps last_checked_at
-- and updated_at; an unknown channel id returns no row.
-- name: UpdateChannelHealth :one
UPDATE /* TEMPLATE: schema */channels
SET health = sqlc.arg(health),
    last_checked_at = now(),
    updated_at = now()
WHERE user_id = sqlc.arg(user_id)
  AND channel_id = sqlc.arg(channel_id)
RETURNING *;

-- DeleteChannel removes one channel of the user and returns the rows deleted,
-- refusing the selected channel so the upload destination cannot disappear.
-- name: DeleteChannel :execrows
DELETE FROM /* TEMPLATE: schema */channels
WHERE user_id = sqlc.arg(user_id)
  AND channel_id = sqlc.arg(channel_id)
  AND NOT selected;
-- ListBots returns one page of the user's bots, newest first, keyset-paged on
-- (created_at, bot_id).
-- name: ListBots :many
SELECT *
FROM /* TEMPLATE: schema */bots
WHERE user_id = sqlc.arg(user_id)
  AND (
    sqlc.narg(after_created_at)::timestamptz IS NULL
    OR (created_at, bot_id) < (
      sqlc.narg(after_created_at)::timestamptz,
      sqlc.narg(after_bot_id)::bigint
    )
  )
ORDER BY created_at DESC, bot_id DESC
LIMIT sqlc.arg(page_size);

-- InsertPendingBots registers the given bot ids for the user in the pending,
-- disabled state and returns the stored rows; an existing bot is reused and reset.
-- name: InsertPendingBots :many
INSERT INTO /* TEMPLATE: schema */bots AS bot (
    bot_id, user_id, token_ciphertext, enabled
)
SELECT input.bot_id, sqlc.arg(user_id), decode(input.token_ciphertext, 'base64'), false
FROM jsonb_to_recordset(sqlc.arg(bots)::jsonb) AS input(
    bot_id bigint, token_ciphertext text
)
-- Registering a token again replaces the stored one and puts the bot back in the
-- pending state, which is the only way a bot that was disabled by a failed
-- provisioning can be tried again: the row is identified by its bot id, so an
-- existing row has to be reused rather than inserted next to. The failure history
-- is cleared with it, because the new attempt starts from nothing.
ON CONFLICT (user_id, bot_id) DO UPDATE
SET token_ciphertext = EXCLUDED.token_ciphertext,
    enabled = false,
    username = NULL,
    consecutive_failures = 0,
    last_error = NULL,
    retry_after = NULL,
    updated_at = now()
RETURNING bot.*;

-- GetBot returns one bot of the user by id, or no row when the user has no such bot.
-- name: GetBot :one
SELECT *
FROM /* TEMPLATE: schema */bots
WHERE user_id = sqlc.arg(user_id)
  AND bot_id = sqlc.arg(bot_id);

-- ActivateBot records the username Telegram reported and clears the failure
-- history, putting the bot back into service; an unknown bot returns no row.
-- name: ActivateBot :one
UPDATE /* TEMPLATE: schema */bots
SET username = sqlc.arg(username),
    enabled = TRUE,
    consecutive_failures = 0,
    last_error = NULL,
    retry_after = NULL,
    updated_at = now()
WHERE user_id = sqlc.arg(user_id)
  AND bot_id = sqlc.arg(bot_id)
RETURNING *;

-- MarkBotProvisionFailure disables the bot, increments its failure counter and
-- stores the error, returning the rows changed.
-- name: MarkBotProvisionFailure :execrows
UPDATE /* TEMPLATE: schema */bots
SET enabled = FALSE,
    consecutive_failures = consecutive_failures + 1,
    last_error = sqlc.arg(last_error),
    updated_at = now()
WHERE user_id = sqlc.arg(user_id)
  AND bot_id = sqlc.arg(bot_id);

-- DeleteBot removes one bot of the user and returns the rows deleted.
-- name: DeleteBot :execrows
DELETE FROM /* TEMPLATE: schema */bots
WHERE user_id = sqlc.arg(user_id)
  AND bot_id = sqlc.arg(bot_id);

-- ListEnabledBots returns every enabled bot of the user ordered by id, ignoring
-- retry_after; callers that must honour the backoff need ListUploadEligibleBots.
-- name: ListEnabledBots :many
SELECT *
FROM /* TEMPLATE: schema */bots
WHERE user_id = sqlc.arg(user_id)
  AND enabled
ORDER BY bot_id;

-- ListUploadEligibleBots returns the enabled bots of the user whose retry_after
-- has passed, ordered by id, which is the pool uploads allocate from.
-- name: ListUploadEligibleBots :many
SELECT *
FROM /* TEMPLATE: schema */bots
WHERE user_id = sqlc.arg(user_id)
  AND enabled
  AND (retry_after IS NULL OR retry_after <= now())
ORDER BY bot_id;

-- NextBotSelectionValue advances the per-user, per-operation round-robin counter
-- and returns the value to use, so concurrent allocators get distinct values.
-- name: NextBotSelectionValue :one
INSERT INTO /* TEMPLATE: schema */bot_selection_counters (
    user_id,
    operation,
    next_value
) VALUES (
    sqlc.arg(user_id),
    sqlc.arg(operation),
    1
)
ON CONFLICT (user_id, operation) DO UPDATE
SET next_value = /* TEMPLATE: schema */bot_selection_counters.next_value + 1,
    updated_at = now()
RETURNING (next_value - 1)::bigint AS selection_value;
-- CountChannelReferences counts the file parts and upload parts that still point
-- at the channel for any user, to decide whether it may be forgotten.
-- name: CountChannelReferences :one
SELECT (
    (SELECT count(*) FROM /* TEMPLATE: schema */file_parts fp WHERE fp.channel_id = sqlc.arg(target_channel_id)) +
    (SELECT count(*) FROM /* TEMPLATE: schema */upload_parts up WHERE up.channel_id = sqlc.arg(target_channel_id))
)::bigint AS reference_count;

-- CountChannelStoredMessages counts the distinct Telegram messages referenced by
-- the channel's file and upload parts, ignoring upload parts without a message.
-- name: CountChannelStoredMessages :one
SELECT count(*)::bigint
FROM (
    SELECT fp.channel_id, fp.message_id
    FROM /* TEMPLATE: schema */file_parts fp
    WHERE fp.channel_id = sqlc.arg(target_channel_id)
    UNION
    SELECT up.channel_id, up.message_id
    FROM /* TEMPLATE: schema */upload_parts up
    WHERE up.channel_id = sqlc.arg(target_channel_id)
      AND message_id IS NOT NULL
) AS stored_messages;

-- ListChannelsForOrphanCleanup returns every channel of every user ordered by user
-- and channel id, because the orphan sweep walks the Telegram side per channel.
-- name: ListChannelsForOrphanCleanup :many
SELECT *
FROM /* TEMPLATE: schema */channels
ORDER BY user_id, channel_id;

-- ListReferencedMessageIDs returns which of the given message ids a file or upload
-- part still references in the channel, so the rest can be deleted from Telegram.
-- name: ListReferencedMessageIDs :many
SELECT message_id
FROM (
    SELECT fp.message_id
    FROM /* TEMPLATE: schema */file_parts fp
    WHERE fp.channel_id = sqlc.arg(target_channel_id)
      AND fp.message_id = ANY(sqlc.arg(message_ids)::bigint[])
    UNION
    SELECT up.message_id
    FROM /* TEMPLATE: schema */upload_parts up
    WHERE up.channel_id = sqlc.arg(target_channel_id)
      AND up.message_id = ANY(sqlc.arg(message_ids)::bigint[])
) AS referenced_messages;

-- ListChannelReferencedParts lists the channel's parts that belong to active files
-- of one user with their file name and size, ordered by file name.
-- name: ListChannelReferencedParts :many
SELECT fp.message_id::bigint AS message_id, f.id AS file_id, f.name AS file_name, f.size AS file_size
FROM /* TEMPLATE: schema */file_parts fp
JOIN /* TEMPLATE: schema */files f ON f.id = fp.file_id
WHERE fp.channel_id = sqlc.arg(target_channel_id)
  AND f.user_id = sqlc.arg(target_user_id)
  AND f.status = 'active'
ORDER BY f.name, fp.message_id;

-- UpsertDiscoveredChannels records the channels found by discovery, inserting them
-- unselected with unknown health and refreshing the name of the existing ones.
-- name: UpsertDiscoveredChannels :many
INSERT INTO /* TEMPLATE: schema */channels AS channel (
    channel_id, user_id, name, selected, health
)
SELECT input.channel_id, sqlc.arg(user_id), input.name, false, 'unknown'
FROM jsonb_to_recordset(sqlc.arg(channels)::jsonb) AS input(
    channel_id bigint, name text
)
ON CONFLICT (user_id, channel_id) DO UPDATE
SET name = EXCLUDED.name,
    updated_at = now()
RETURNING channel.*;

-- UpdateBotSession stores the serialized Telegram session of one bot and returns
-- the rows changed, so a stale bot id is reported as zero.
-- name: UpdateBotSession :execrows
UPDATE /* TEMPLATE: schema */bots
SET session = sqlc.arg(session),
    updated_at = now()
WHERE user_id = sqlc.arg(user_id)
  AND bot_id = sqlc.arg(bot_id);

-- MarkBotUploadSuccess clears the failure state of the bot and stamps last_used_at,
-- which puts it back at the front of the allocation order.
-- name: MarkBotUploadSuccess :execrows
UPDATE /* TEMPLATE: schema */bots
SET consecutive_failures = 0,
    last_error = NULL,
    last_used_at = now(),
    retry_after = NULL,
    updated_at = now()
WHERE user_id = sqlc.arg(user_id)
  AND bot_id = sqlc.arg(bot_id);

-- MarkBotUploadFailure increments the bot's failure counter, keeps its last error
-- and defers the next attempt with an exponential backoff.
-- name: MarkBotUploadFailure :execrows
UPDATE /* TEMPLATE: schema */bots
SET consecutive_failures = consecutive_failures + 1,
    last_error = left(sqlc.arg(last_error), 1000),
    -- The wait doubles with every consecutive failure, from thirty seconds up to
    -- six hours, so a bot that keeps failing is retried rarely instead of every
    -- thirtieth second while a transient failure is retried promptly. A success
    -- resets the counter, which puts the bot back at the front of the queue.
    retry_after = now() + make_interval(secs => LEAST(30 * power(2, LEAST(consecutive_failures, 16)), 21600)),
    updated_at = now()
WHERE user_id = sqlc.arg(user_id)
  AND bot_id = sqlc.arg(bot_id);
