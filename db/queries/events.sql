-- ListUserEventsAfter returns up to event_limit events of the user with an id above
-- after_id, ascending, so the caller advances its cursor to the last id it saw.
-- name: ListUserEventsAfter :many
SELECT id, user_id, event_type, resource_type, resource_id, generation, payload, occurred_at
FROM /* TEMPLATE: schema */user_events
WHERE user_id = sqlc.arg(user_id)
  AND id > sqlc.arg(after_id)
  AND (
    COALESCE(cardinality(sqlc.arg(event_types)::text[]), 0) = 0
    OR event_type = ANY(sqlc.arg(event_types)::text[])
  )
ORDER BY id
LIMIT sqlc.arg(event_limit);

-- GetUserEventCursorState reports whether the given after_id still exists for the
-- user, plus the oldest and newest retained event ids and the stream's high-water
-- mark; every id is 0 when the user has no events yet.
-- name: GetUserEventCursorState :one
SELECT
    EXISTS (
        SELECT 1
        FROM /* TEMPLATE: schema */user_events AS cursor_event
        WHERE cursor_event.user_id = sqlc.arg(cursor_user_id)
          AND cursor_event.id = sqlc.arg(after_id)
    ) AS cursor_exists,
    COALESCE(MIN(event_rows.id), 0)::bigint AS oldest_id,
    COALESCE(MAX(event_rows.id), 0)::bigint AS newest_id,
    COALESCE((
        SELECT stream_state.last_event_id
        FROM /* TEMPLATE: schema */user_event_stream_state AS stream_state
        WHERE stream_state.user_id = sqlc.arg(cursor_user_id)
    ), 0)::bigint AS last_event_id
FROM /* TEMPLATE: schema */user_events AS event_rows
WHERE event_rows.user_id = sqlc.arg(cursor_user_id);

-- DeleteUserEventsBefore drops every event that occurred before the cutoff, for all
-- users, and returns how many rows were removed.
-- name: DeleteUserEventsBefore :execrows
DELETE FROM /* TEMPLATE: schema */user_events
WHERE occurred_at < sqlc.arg(cutoff);

-- CreateEventStreamTicket stores the SHA-256 hash of a new SSE stream ticket for the
-- user together with its expiry; the plaintext token is never persisted.
-- name: CreateEventStreamTicket :exec
INSERT INTO /* TEMPLATE: schema */event_stream_tickets (token_hash, user_id, expires_at)
VALUES (sqlc.arg(token_hash), sqlc.arg(user_id), sqlc.arg(expires_at));

-- GetEventStreamTicketUser resolves a presented stream ticket hash to its user while
-- the ticket has not expired; an expired or unknown ticket returns no row.
-- name: GetEventStreamTicketUser :one
SELECT user_id
FROM /* TEMPLATE: schema */event_stream_tickets
WHERE token_hash = sqlc.arg(token_hash)
  AND expires_at > now();

-- DeleteExpiredEventStreamTickets removes the tickets whose expiry has passed and
-- returns the number of rows deleted.
-- name: DeleteExpiredEventStreamTickets :execrows
DELETE FROM /* TEMPLATE: schema */event_stream_tickets
WHERE expires_at <= now();
