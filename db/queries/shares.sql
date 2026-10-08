-- CreateFileShare stores a new share link for one file and returns it; the password,
-- expiry and download limit are optional, and a NULL limit means unlimited downloads.
-- name: CreateFileShare :one
INSERT INTO /* TEMPLATE: schema */file_shares (
    id,
    file_id,
    owner_id,
    token_hash,
    password_hash,
    expires_at,
    max_downloads,
    permission
) VALUES (
    sqlc.arg(id),
    sqlc.arg(file_id),
    sqlc.arg(owner_id),
    sqlc.arg(token_hash),
    sqlc.narg(password_hash),
    sqlc.narg(expires_at),
    sqlc.narg(max_downloads),
    sqlc.arg(permission)
)
RETURNING *;

-- ListFileShares returns one page of the owner's share links for one file, newest
-- first, keyset-paged on (created_at, id), including revoked and expired ones.
-- name: ListFileShares :many
SELECT *
FROM /* TEMPLATE: schema */file_shares
WHERE owner_id = sqlc.arg(owner_id)
  AND file_id = sqlc.arg(file_id)
  AND (
    sqlc.narg(after_created_at)::timestamptz IS NULL
    OR (created_at, id) < (
      sqlc.narg(after_created_at)::timestamptz,
      sqlc.narg(after_id)::uuid
    )
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_size);

-- GetFileShareForOwner returns one share by id only when it belongs to the owner, so
-- an id from another account returns no row.
-- name: GetFileShareForOwner :one
SELECT *
FROM /* TEMPLATE: schema */file_shares
WHERE id = sqlc.arg(id)
  AND owner_id = sqlc.arg(owner_id);

-- UpdateFileShare partially updates a live share of the owner: password, expiry and
-- download limit are each set, cleared or left alone by a pair of arguments, and a new
-- limit may not fall below the downloads already counted.
-- name: UpdateFileShare :one
UPDATE /* TEMPLATE: schema */file_shares
SET password_hash = CASE
      WHEN sqlc.arg(clear_password)::boolean THEN NULL
      ELSE COALESCE(sqlc.narg(password_hash), password_hash)
    END,
    expires_at = CASE
      WHEN sqlc.arg(clear_expires_at)::boolean THEN NULL
      ELSE COALESCE(sqlc.narg(expires_at), expires_at)
    END,
    max_downloads = CASE
      WHEN sqlc.arg(clear_max_downloads)::boolean THEN NULL
      ELSE COALESCE(sqlc.narg(max_downloads), max_downloads)
    END,
    permission = COALESCE(sqlc.narg(permission)::/* TEMPLATE: schema */share_permission, permission)
WHERE id = sqlc.arg(id)
  AND owner_id = sqlc.arg(owner_id)
  AND revoked_at IS NULL
  AND (
    sqlc.arg(clear_max_downloads)::boolean
    OR sqlc.narg(max_downloads)::bigint IS NULL
    OR sqlc.narg(max_downloads)::bigint >= download_count
  )
RETURNING *;

-- GetActiveShareByTokenHash resolves a share token to its share plus the file name,
-- kind and status; a revoked, expired or exhausted share, or an inactive file,
-- returns no row.
-- name: GetActiveShareByTokenHash :one
SELECT fs.*, f.name AS file_name, f.kind AS file_kind, f.status AS file_status
FROM /* TEMPLATE: schema */file_shares fs
JOIN /* TEMPLATE: schema */files f ON f.id = fs.file_id
WHERE fs.token_hash = sqlc.arg(token_hash)
  AND fs.revoked_at IS NULL
  AND (fs.expires_at IS NULL OR fs.expires_at > now())
  AND (fs.max_downloads IS NULL OR fs.download_count < fs.max_downloads)
  AND f.status = 'active';

-- IncrementShareDownloadCount consumes one download of a live share and returns the
-- updated row; an exhausted, expired or revoked share returns no row.
-- name: IncrementShareDownloadCount :one
UPDATE /* TEMPLATE: schema */file_shares
SET download_count = download_count + 1
WHERE id = sqlc.arg(id)
  AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > now())
  AND (max_downloads IS NULL OR download_count < max_downloads)
RETURNING *;

-- RevokeFileShare revokes one live share of the owner and returns the rows changed,
-- which is zero for an unknown, foreign or already revoked share.
-- name: RevokeFileShare :execrows
UPDATE /* TEMPLATE: schema */file_shares
SET revoked_at = now()
WHERE id = sqlc.arg(id)
  AND owner_id = sqlc.arg(owner_id)
  AND revoked_at IS NULL;

-- CreateFileAccessGrant grants one user access to one file and returns the row;
-- a second grant while an earlier one is live replaces its permission and expiry.
-- name: CreateFileAccessGrant :one
INSERT INTO /* TEMPLATE: schema */file_access_grants (
    id, file_id, owner_id, grantee_id, permission, expires_at
) VALUES (
    sqlc.arg(id), sqlc.arg(file_id), sqlc.arg(owner_id), sqlc.arg(grantee_id),
    sqlc.arg(permission), sqlc.narg(expires_at)
)
ON CONFLICT (file_id, grantee_id) WHERE revoked_at IS NULL
DO UPDATE SET
    permission = EXCLUDED.permission,
    expires_at = EXCLUDED.expires_at,
    updated_at = now()
RETURNING *;

-- ListFileAccessGrantsForOwner lists the live grants on one of the owner's files with
-- the grantee's display name and username, newest first.
-- name: ListFileAccessGrantsForOwner :many
SELECT g.*, u.display_name AS grantee_display_name, u.username AS grantee_username
FROM /* TEMPLATE: schema */file_access_grants g
JOIN /* TEMPLATE: schema */users u ON u.user_id = g.grantee_id
WHERE g.owner_id = sqlc.arg(owner_id)
  AND g.file_id = sqlc.arg(file_id)
  AND g.revoked_at IS NULL
ORDER BY g.created_at DESC, g.id DESC;

-- ListShared lists one page of the owner's active files that are currently shared, by
-- a live grant or a live share link, most recently updated first, keyset-paged on
-- (updated_at, id).
-- name: ListShared :many
SELECT f.*
FROM /* TEMPLATE: schema */files f
WHERE f.user_id = sqlc.arg(owner_id)
  AND f.status = 'active'
  AND (
    EXISTS (
      SELECT 1
      FROM /* TEMPLATE: schema */file_access_grants g
      WHERE g.owner_id = f.user_id
        AND g.file_id = f.id
        AND g.revoked_at IS NULL
        AND (g.expires_at IS NULL OR g.expires_at > now())
    )
    OR EXISTS (
      SELECT 1
      FROM /* TEMPLATE: schema */file_shares fs
      WHERE fs.owner_id = f.user_id
        AND fs.file_id = f.id
        AND fs.revoked_at IS NULL
        AND (fs.expires_at IS NULL OR fs.expires_at > now())
        AND (fs.max_downloads IS NULL OR fs.download_count < fs.max_downloads)
    )
  )
  AND (
    sqlc.narg(after_updated_at)::timestamptz IS NULL
    OR (f.updated_at, f.id) < (sqlc.narg(after_updated_at)::timestamptz, sqlc.narg(after_id)::uuid)
  )
ORDER BY f.updated_at DESC, f.id DESC
LIMIT sqlc.arg(page_size);


-- ListSharedWithMe lists one page of the active files shared with the grantee, each
-- with the grant's permission and id, newest grant update first, keyset-paged on the
-- grant's (updated_at, id).
-- name: ListSharedWithMe :many
-- The grant's own timestamp and id are the sort key and the cursor, so both are
-- returned next to the file the grant points at.
SELECT f.*, g.permission, g.updated_at AS grant_updated_at, g.id AS grant_id
FROM /* TEMPLATE: schema */file_access_grants g
JOIN /* TEMPLATE: schema */files f ON f.id = g.file_id AND f.user_id = g.owner_id
WHERE g.grantee_id = sqlc.arg(grantee_id)
  AND g.revoked_at IS NULL
  AND (g.expires_at IS NULL OR g.expires_at > now())
  AND f.status = 'active'
  AND (
    sqlc.narg(after_grant_updated_at)::timestamptz IS NULL
    OR (g.updated_at, g.id) < (sqlc.narg(after_grant_updated_at)::timestamptz, sqlc.narg(after_grant_id)::uuid)
  )
ORDER BY g.updated_at DESC, g.id DESC
LIMIT sqlc.arg(page_size);

-- UpdateFileAccessGrant changes the permission or expiry of a live grant of the owner;
-- the flag clears the expiry, and an unknown, revoked or foreign grant returns no row.
-- name: UpdateFileAccessGrant :one
UPDATE /* TEMPLATE: schema */file_access_grants
SET permission = COALESCE(sqlc.narg(permission)::/* TEMPLATE: schema */share_permission, permission),
    expires_at = CASE
      WHEN sqlc.arg(clear_expires_at)::boolean THEN NULL
      ELSE COALESCE(sqlc.narg(expires_at), expires_at)
    END,
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND owner_id = sqlc.arg(owner_id)
  AND revoked_at IS NULL
RETURNING *;

-- RevokeFileAccessGrant revokes one live grant of the owner and returns the rows
-- changed, zero for an unknown, foreign or already revoked grant.
-- name: RevokeFileAccessGrant :execrows
UPDATE /* TEMPLATE: schema */file_access_grants
SET revoked_at = now(), updated_at = now()
WHERE id = sqlc.arg(id)
  AND owner_id = sqlc.arg(owner_id)
  AND revoked_at IS NULL;

-- ResolveFileAccessMany resolves the actor's access to each requested active file id:
-- ownership counts as edit, a live grant on the file or any ancestor contributes its
-- permission (edit only when require_edit is set), and one best row per file is
-- returned, preferring owned, then edit, then the most recent grant.
-- name: ResolveFileAccessMany :many
WITH RECURSIVE params AS (
  SELECT sqlc.arg(file_ids)::uuid[] AS file_ids,
         sqlc.arg(actor_id)::bigint AS actor_id,
         sqlc.arg(require_edit)::boolean AS require_edit
), ancestors AS (
  SELECT target.id AS target_file_id,
         target.id AS ancestor_file_id,
         target.parent_id,
         target.user_id AS owner_id
  FROM /* TEMPLATE: schema */files AS target
  CROSS JOIN params
  WHERE target.id = ANY(params.file_ids)
    AND target.status = 'active'
  UNION ALL
  SELECT ancestors.target_file_id,
         parent.id,
         parent.parent_id,
         ancestors.owner_id
  FROM /* TEMPLATE: schema */files AS parent
  JOIN ancestors ON parent.id = ancestors.parent_id
  WHERE parent.user_id = ancestors.owner_id
), candidates AS (
  SELECT ancestors.target_file_id,
         ancestors.owner_id,
         ancestors.target_file_id AS root_file_id,
         'edit'::/* TEMPLATE: schema */share_permission AS permission,
         true AS owned,
         NULL::timestamptz AS grant_created_at
  FROM ancestors
  CROSS JOIN params
  WHERE ancestors.target_file_id = ancestors.ancestor_file_id
    AND ancestors.owner_id = params.actor_id
  UNION ALL
  SELECT ancestors.target_file_id,
         ancestors.owner_id,
         access_grant.file_id AS root_file_id,
         access_grant.permission,
         false AS owned,
         access_grant.created_at AS grant_created_at
  FROM ancestors
  JOIN /* TEMPLATE: schema */file_access_grants AS access_grant
    ON access_grant.file_id = ancestors.ancestor_file_id
   AND access_grant.owner_id = ancestors.owner_id
  CROSS JOIN params
  WHERE access_grant.grantee_id = params.actor_id
    AND access_grant.revoked_at IS NULL
    AND (access_grant.expires_at IS NULL OR access_grant.expires_at > now())
    AND (NOT params.require_edit OR access_grant.permission = 'edit')
)
SELECT DISTINCT ON (target_file_id)
       target_file_id, owner_id, root_file_id, permission, owned
FROM candidates
ORDER BY target_file_id,
         owned DESC,
         (permission = 'edit') DESC,
         grant_created_at DESC NULLS LAST;
