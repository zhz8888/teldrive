-- AcquireUserBootstrapLock takes the fixed transaction-scoped advisory lock that
-- serializes user bootstrapping, so two logins cannot both create the first owner.
-- name: AcquireUserBootstrapLock :exec
SELECT pg_advisory_xact_lock(846742351);

-- UpsertUser inserts the Telegram profile of one user or refreshes it on the next
-- login and returns the row; the first user ever inserted is granted the owner role.
-- name: UpsertUser :one
INSERT INTO /* TEMPLATE: schema */users (
    user_id,
    display_name,
    username,
    premium,
    role
) VALUES (
    sqlc.arg(user_id),
    sqlc.narg(display_name),
    sqlc.narg(username),
    sqlc.arg(premium),
    CASE
        WHEN EXISTS (SELECT 1 FROM /* TEMPLATE: schema */users) THEN 'user'::/* TEMPLATE: schema */user_role
        ELSE 'owner'::/* TEMPLATE: schema */user_role
    END
)
ON CONFLICT (user_id) DO UPDATE
SET display_name = EXCLUDED.display_name,
    username = EXCLUDED.username,
    premium = EXCLUDED.premium,
    updated_at = now()
RETURNING *;

-- GetUser returns the full account row of one Telegram user id, including its
-- disabled_at, so callers apply their own policy on a disabled account.
-- name: GetUser :one
SELECT *
FROM /* TEMPLATE: schema */users
WHERE user_id = sqlc.arg(user_id);

-- ListUsers returns up to page_size accounts ordered by creation time, oldest first;
-- search matches display name or username case-insensitively, or the exact user id.
-- name: ListUsers :many
SELECT *
FROM /* TEMPLATE: schema */users
WHERE (
    sqlc.narg(search)::text IS NULL
    OR display_name ILIKE '%' || sqlc.narg(search)::text || '%'
    OR username ILIKE '%' || sqlc.narg(search)::text || '%'
    OR user_id::text = sqlc.narg(search)::text
)
ORDER BY created_at ASC, user_id ASC
LIMIT sqlc.arg(page_size);

-- UpdateUserRole sets a non-owner account to 'admin' or 'user' and returns the updated
-- row; targeting the owner, or any other role value, returns no row.
-- name: UpdateUserRole :one
UPDATE /* TEMPLATE: schema */users
SET role = sqlc.arg(role), updated_at = now()
WHERE user_id = sqlc.arg(user_id)
  AND role <> 'owner'
  AND sqlc.arg(role)::/* TEMPLATE: schema */user_role IN ('admin', 'user')
RETURNING *;

-- SetUserDisabled disables or re-enables a non-owner account, stamping disabled_at on
-- the first disable and clearing it on enable; the owner row is never touched.
-- name: SetUserDisabled :one
UPDATE /* TEMPLATE: schema */users
SET disabled_at = CASE WHEN sqlc.arg(disabled)::boolean THEN COALESCE(disabled_at, now()) ELSE NULL END,
    updated_at = now()
WHERE user_id = sqlc.arg(user_id)
  AND role <> 'owner'
RETURNING *;

-- RevokeAllSessionsForUser revokes every live session of the user and returns the row
-- count, leaving sessions revoked earlier with their original timestamp.
-- name: RevokeAllSessionsForUser :execrows
UPDATE /* TEMPLATE: schema */sessions
SET revoked_at = COALESCE(revoked_at, now())
WHERE user_id = sqlc.arg(user_id)
  AND revoked_at IS NULL;

-- RevokeAllAPIKeysForUser revokes every live API key of the user and returns the row
-- count, leaving already revoked keys untouched.
-- name: RevokeAllAPIKeysForUser :execrows
UPDATE /* TEMPLATE: schema */api_keys
SET revoked_at = COALESCE(revoked_at, now())
WHERE user_id = sqlc.arg(user_id)
  AND revoked_at IS NULL;

-- CreateTelegramLoginFlow stores a new pending Telegram login flow and returns it; the
-- phone number is optional because a QR-code flow does not know it yet.
-- name: CreateTelegramLoginFlow :one
INSERT INTO /* TEMPLATE: schema */telegram_login_flows (
    id,
    method,
    phone_number_ciphertext,
    telegram_state_ciphertext,
    password_required,
    expires_at
) VALUES (
    sqlc.arg(id),
    sqlc.arg(method),
    sqlc.narg(phone_number_ciphertext),
    sqlc.arg(telegram_state_ciphertext),
    sqlc.arg(password_required),
    sqlc.arg(expires_at)
)
RETURNING *;

-- UpdateTelegramLoginFlowState replaces the stored Telegram state of a flow that is
-- still pending and unexpired; a completed or expired flow returns no row.
-- name: UpdateTelegramLoginFlowState :one
UPDATE /* TEMPLATE: schema */telegram_login_flows
SET telegram_state_ciphertext = sqlc.arg(telegram_state_ciphertext),
    password_required = sqlc.arg(password_required)
WHERE id = sqlc.arg(id)
  AND completed_at IS NULL
  AND expires_at > now()
RETURNING *;

-- CompleteTelegramLoginFlow marks a still-pending flow as completed and returns it; a
-- second completion, or an unknown id, returns no row.
-- name: CompleteTelegramLoginFlow :one
UPDATE /* TEMPLATE: schema */telegram_login_flows
SET completed_at = now()
WHERE id = sqlc.arg(id)
  AND completed_at IS NULL
RETURNING *;

-- DeleteExpiredTelegramLoginFlows deletes every flow whose expiry has passed and
-- returns the number of rows removed.
-- name: DeleteExpiredTelegramLoginFlows :execrows
DELETE FROM /* TEMPLATE: schema */telegram_login_flows
WHERE expires_at <= now();

-- CreateSession stores a refresh session for the user and returns it; telegram_session
-- is the serialized Telegram session used later to act on the user's behalf.
-- name: CreateSession :one
INSERT INTO /* TEMPLATE: schema */sessions (
    id,
    user_id,
    telegram_session,
    refresh_token_hash,
    expires_at
) VALUES (
    sqlc.arg(id),
    sqlc.arg(user_id),
    sqlc.arg(telegram_session),
    sqlc.arg(refresh_token_hash),
    sqlc.arg(expires_at)
)
RETURNING *;

-- GetSessionByRefreshTokenHash resolves a refresh token hash to its session and
-- returns no row once the session is revoked or expired.
-- name: GetSessionByRefreshTokenHash :one
SELECT *
FROM /* TEMPLATE: schema */sessions
WHERE refresh_token_hash = sqlc.arg(refresh_token_hash)
  AND revoked_at IS NULL
  AND expires_at > now();

-- RotateSessionRefreshToken swaps in a new refresh token hash and stamps last_used_at,
-- but only while the old hash still matches a live session, so a replay fails.
-- name: RotateSessionRefreshToken :one
UPDATE /* TEMPLATE: schema */sessions
SET refresh_token_hash = sqlc.arg(new_refresh_token_hash),
    last_used_at = now()
WHERE id = sqlc.arg(session_id)
  AND refresh_token_hash = sqlc.arg(old_refresh_token_hash)
  AND revoked_at IS NULL
  AND expires_at > now()
RETURNING *;

-- RevokeSession revokes one session of the given user and returns the row count, which
-- is zero when the session belongs to someone else or was already revoked.
-- name: RevokeSession :execrows
UPDATE /* TEMPLATE: schema */sessions
SET revoked_at = COALESCE(revoked_at, now())
WHERE id = sqlc.arg(session_id)
  AND user_id = sqlc.arg(user_id);

-- ListSessions returns one page of the user's live sessions, newest first,
-- keyset-paged on (created_at, id); revoked and expired sessions are never listed.
-- name: ListSessions :many
SELECT *
FROM /* TEMPLATE: schema */sessions
WHERE user_id = sqlc.arg(user_id)
  AND revoked_at IS NULL
  AND expires_at > now()
  AND (
    sqlc.narg(after_created_at)::timestamptz IS NULL
    OR (created_at, id) < (
      sqlc.narg(after_created_at)::timestamptz,
      sqlc.narg(after_id)::uuid
    )
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_size);
-- CreateAPIKey stores a new API key for the user and returns it; only the secret hash
-- and a display prefix are persisted, and a NULL expiry means the key never expires.
-- name: CreateAPIKey :one
INSERT INTO /* TEMPLATE: schema */api_keys (
    id,
    user_id,
    name,
    key_prefix,
    secret_hash,
    expires_at
) VALUES (
    sqlc.arg(id),
    sqlc.arg(user_id),
    sqlc.arg(name),
    sqlc.arg(key_prefix),
    sqlc.arg(secret_hash),
    sqlc.narg(expires_at)
)
RETURNING *;

-- ListAPIKeys returns one page of the user's API keys, newest first, keyset-paged on
-- (created_at, id), including revoked and expired ones.
-- name: ListAPIKeys :many
SELECT *
FROM /* TEMPLATE: schema */api_keys
WHERE user_id = sqlc.arg(user_id)
  AND (
    sqlc.narg(after_created_at)::timestamptz IS NULL
    OR (created_at, id) < (
      sqlc.narg(after_created_at)::timestamptz,
      sqlc.narg(after_id)::uuid
    )
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_size);

-- GetActiveAPIKeyByHash resolves a presented secret to its key row, ignoring revoked
-- keys and keys whose expiry has passed; a NULL expiry never expires.
-- name: GetActiveAPIKeyByHash :one
SELECT *
FROM /* TEMPLATE: schema */api_keys
WHERE secret_hash = sqlc.arg(secret_hash)
  AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > now());

-- TouchAPIKey records the last use of a key by id, with no owner check and no failure
-- when the id no longer exists.
-- name: TouchAPIKey :exec
UPDATE /* TEMPLATE: schema */api_keys
SET last_used_at = now()
WHERE id = sqlc.arg(id);

-- RevokeAPIKey revokes one key of the user and returns the rows changed, which is zero
-- for an unknown, foreign or already revoked key.
-- name: RevokeAPIKey :execrows
UPDATE /* TEMPLATE: schema */api_keys
SET revoked_at = now()
WHERE id = sqlc.arg(id)
  AND user_id = sqlc.arg(user_id)
  AND revoked_at IS NULL;

-- GetTelegramLoginFlow returns a login flow that is still pending and unexpired; a
-- completed or expired flow, or a reused id, returns no row.
-- name: GetTelegramLoginFlow :one
SELECT *
FROM /* TEMPLATE: schema */telegram_login_flows
WHERE id = sqlc.arg(id)
  AND completed_at IS NULL
  AND expires_at > now();

-- GetActiveSession returns a session only when it belongs to the user and is neither
-- revoked nor expired.
-- name: GetActiveSession :one
SELECT *
FROM /* TEMPLATE: schema */sessions
WHERE id = sqlc.arg(session_id)
  AND user_id = sqlc.arg(user_id)
  AND revoked_at IS NULL
  AND expires_at > now();

-- GetLatestActiveSessionForUser returns the user's most recently used live session,
-- ordering sessions that were never used by creation time instead.
-- name: GetLatestActiveSessionForUser :one
SELECT *
FROM /* TEMPLATE: schema */sessions
WHERE user_id = sqlc.arg(user_id)
  AND revoked_at IS NULL
  AND expires_at > now()
ORDER BY last_used_at DESC NULLS LAST, created_at DESC
LIMIT 1;

-- TouchSession stamps last_used_at on a session that is not revoked; an unknown or
-- revoked id is silently ignored.
-- name: TouchSession :exec
UPDATE /* TEMPLATE: schema */sessions
SET last_used_at = now()
WHERE id = sqlc.arg(session_id)
  AND revoked_at IS NULL;

-- UpdateSessionTelegramSession replaces the stored Telegram session of one live
-- session and refreshes last_used_at, returning the rows changed.
-- name: UpdateSessionTelegramSession :execrows
-- The write is scoped to the owner as well, so a session id can never be used to
-- rewrite another account's stored Telegram session.
UPDATE /* TEMPLATE: schema */sessions
SET telegram_session = sqlc.arg(telegram_session),
    last_used_at = now()
WHERE id = sqlc.arg(session_id)
  AND user_id = sqlc.arg(user_id)
  AND revoked_at IS NULL
  AND expires_at > now();


-- SearchUsersForShare finds share recipients by display name, username or exact
-- user id, excluding the caller and disabled accounts, ordered by username.
-- name: SearchUsersForShare :many
SELECT *
FROM /* TEMPLATE: schema */users
WHERE user_id <> sqlc.arg(exclude_user_id)
  AND disabled_at IS NULL
  AND (
    display_name ILIKE '%' || sqlc.arg(search)::text || '%'
    OR username ILIKE '%' || sqlc.arg(search)::text || '%'
    OR user_id::text = sqlc.arg(search)::text
  )
ORDER BY username ASC NULLS LAST, display_name ASC NULLS LAST, user_id ASC
LIMIT sqlc.arg(page_size);
