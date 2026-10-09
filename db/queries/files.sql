-- GetFileForUser returns one row of the user's catalogue by id, whatever its kind or
-- status, so trashed and deletion-pending rows are included.
-- name: GetFileForUser :one
SELECT *
FROM /* TEMPLATE: schema */files
WHERE id = sqlc.arg(file_id)
  AND user_id = sqlc.arg(user_id);

-- GetActiveFolderForUser returns the user's folder only while it is active, so a
-- trashed or deletion-pending folder returns no row.
-- name: GetActiveFolderForUser :one
SELECT *
FROM /* TEMPLATE: schema */files
WHERE id = sqlc.arg(folder_id)
  AND user_id = sqlc.arg(user_id)
  AND kind = 'folder'
  AND status = 'active';

-- ListFiles returns one page of the user's direct children of parent_id, ordered by
-- name and keyset-paged on (name, id); a trashed listing with a NULL parent also
-- surfaces trashed rows whose parent is not trashed, so a trashed subtree is listed
-- once at its root. Search matches by trigram or by substring.
-- name: ListFiles :many
SELECT *
FROM /* TEMPLATE: schema */files
WHERE files.user_id = sqlc.arg(user_id)
  AND (
    files.parent_id IS NOT DISTINCT FROM sqlc.narg(parent_id)::uuid
    OR (
      sqlc.arg(status)::/* TEMPLATE: schema */file_status = 'trashed'
      AND sqlc.narg(parent_id)::uuid IS NULL
      AND files.parent_id IS NOT NULL
      AND NOT EXISTS (
        SELECT 1
        FROM /* TEMPLATE: schema */files parent
        WHERE parent.id = files.parent_id
          AND parent.user_id = files.user_id
          AND parent.status = 'trashed'
      )
    )
  )
  AND files.status = sqlc.arg(status)::/* TEMPLATE: schema */file_status
  AND (sqlc.narg(kind)::/* TEMPLATE: schema */file_kind IS NULL OR files.kind = sqlc.narg(kind)::/* TEMPLATE: schema */file_kind)
  AND (
    sqlc.narg(search)::text IS NULL
    OR files.name % sqlc.narg(search)::text
    OR files.name ILIKE '%' || sqlc.narg(search)::text || '%'
  )
  AND (
    sqlc.narg(after_name)::text IS NULL
    OR (name, id) > (sqlc.narg(after_name)::text, sqlc.narg(after_id)::uuid)
  )
ORDER BY name, id
LIMIT sqlc.arg(page_size);

-- CreateFolder inserts an active folder row with the directory MIME type, no size and
-- no encryption, and returns it.
-- name: CreateFolder :one
INSERT INTO /* TEMPLATE: schema */files (
    id,
    user_id,
    parent_id,
    name,
    kind,
    mime_type,
    size,
    encryption,
    status,
    mod_time
) VALUES (
    sqlc.arg(id),
    sqlc.arg(user_id),
    sqlc.narg(parent_id),
    sqlc.arg(name),
    'folder',
    'inode/directory',
    NULL,
    false,
    'active',
    sqlc.arg(mod_time)
)
RETURNING *;

-- UpdateFileMetadata applies an optional rename and mod_time to one active file of the
-- user and bumps its generation; a supplied expected_generation turns it into a
-- compare-and-set that returns no row when it no longer matches.
-- name: UpdateFileMetadata :one
UPDATE /* TEMPLATE: schema */files
SET name = COALESCE(sqlc.narg(name), name),
    mod_time = COALESCE(sqlc.narg(mod_time), mod_time),
    generation = generation + 1,
    updated_at = now()
WHERE id = sqlc.arg(file_id)
  AND user_id = sqlc.arg(user_id)
  AND status = 'active'
  AND (
    sqlc.narg(expected_generation)::bigint IS NULL
    OR generation = sqlc.narg(expected_generation)::bigint
  )
RETURNING *;

-- MoveFile re-parents one active file or folder of the user and bumps its generation;
-- a stale expected_generation makes it a compare-and-set that returns no row, and a
-- NULL parent_id means the drive root.
-- name: MoveFile :one
UPDATE /* TEMPLATE: schema */files
SET parent_id = sqlc.narg(parent_id),
    generation = generation + 1,
    updated_at = now()
WHERE id = sqlc.arg(file_id)
  AND user_id = sqlc.arg(user_id)
  AND status = 'active'
  AND (
    sqlc.narg(expected_generation)::bigint IS NULL
    OR generation = sqlc.narg(expected_generation)::bigint
  )
RETURNING *;

-- TrashFile moves one active file of the user to the trash, stamping deleted_at and
-- bumping its generation; an already trashed or missing row returns no row.
-- name: TrashFile :one
UPDATE /* TEMPLATE: schema */files
SET status = 'trashed',
    deleted_at = now(),
    generation = generation + 1,
    updated_at = now()
WHERE id = sqlc.arg(file_id)
  AND user_id = sqlc.arg(user_id)
  AND status = 'active'
RETURNING *;

-- RestoreFileSubtree restores one trashed file and its trashed descendants to active
-- and returns every row it changed; the root is accepted only when its parent is
-- active or gone, so a subtree is never restored under a still-trashed parent.
-- name: RestoreFileSubtree :many
WITH RECURSIVE target AS (
  SELECT root.id
  FROM /* TEMPLATE: schema */files root
  WHERE root.id = sqlc.arg(file_id)
    AND root.user_id = sqlc.arg(user_id)
    AND root.status = 'trashed'
    AND (
      root.parent_id IS NULL
      OR EXISTS (
        SELECT 1
        FROM /* TEMPLATE: schema */files parent
        WHERE parent.id = root.parent_id
          AND parent.user_id = root.user_id
          AND parent.status = 'active'
      )
    )
  UNION ALL
  SELECT child.id
  FROM /* TEMPLATE: schema */files child
  JOIN target parent ON child.parent_id = parent.id
  WHERE child.user_id = sqlc.arg(user_id)
    AND child.status = 'trashed'
)
UPDATE /* TEMPLATE: schema */files AS target_file
SET status = 'active',
    deleted_at = NULL,
    generation = target_file.generation + 1,
    updated_at = now()
WHERE target_file.user_id = sqlc.arg(user_id)
  AND target_file.id IN (SELECT target.id FROM target)
RETURNING target_file.*;

-- DeleteFileCatalogRowsByIDs deletes the user's catalogue rows for the given ids but
-- only where they are already deletion_pending, returning how many rows went away.
-- name: DeleteFileCatalogRowsByIDs :execrows
DELETE FROM /* TEMPLATE: schema */files
WHERE id = ANY(sqlc.arg(file_ids)::uuid[])
  AND user_id = sqlc.arg(user_id)
  AND status = 'deletion_pending';

-- Recursive move-cycle validation will be implemented as a hand-reviewed query in the file service.

-- ListFileParts returns every part of a file in part order; the query is not scoped by
-- user, so callers must have checked access to file_id first.
-- name: ListFileParts :many
SELECT *
FROM /* TEMPLATE: schema */file_parts
WHERE file_id = sqlc.arg(file_id)
ORDER BY part_no;

-- ListFilePartsByFileIDs returns the parts of many files in (file_id, part_no) order;
-- like ListFileParts it trusts the caller for ownership.
-- name: ListFilePartsByFileIDs :many
SELECT *
FROM /* TEMPLATE: schema */file_parts
WHERE file_id = ANY(sqlc.arg(file_ids)::uuid[])
ORDER BY file_id, part_no;

-- ResolveActiveChildFolder returns the id of the active folder with this name under
-- the given parent, NULL meaning the drive root, used to resolve one path component.
-- name: ResolveActiveChildFolder :one
SELECT id
FROM /* TEMPLATE: schema */files
WHERE user_id = sqlc.arg(user_id)
  AND parent_id IS NOT DISTINCT FROM sqlc.narg(parent_id)::uuid
  AND name = sqlc.arg(name)
  AND kind = 'folder'
  AND status = 'active';

-- ResolveActiveChild returns the active file or folder with this name under the given
-- parent, NULL meaning the drive root; trashed and deletion-pending rows are ignored.
-- name: ResolveActiveChild :one
SELECT *
FROM /* TEMPLATE: schema */files
WHERE user_id = sqlc.arg(user_id)
  AND parent_id IS NOT DISTINCT FROM sqlc.narg(parent_id)::uuid
  AND name = sqlc.arg(name)
  AND status = 'active';

-- ListFilesAdvanced is the advanced file search: scope selects the direct children of
-- parent_id ('folder'), the whole drive ('drive') or one folder subtree ('recursive'),
-- while status, kind, search text or regex, category and update window filter the
-- rows; paging is a keyset over the requested sort column, and a trashed listing with
-- a NULL parent also surfaces trashed rows whose parent is not trashed.
-- name: ListFilesAdvanced :many
WITH RECURSIVE scope_files AS (
  SELECT root.id
  FROM /* TEMPLATE: schema */files root
  WHERE sqlc.arg(scope)::text IN ('drive', 'recursive')
    AND sqlc.narg(scope_folder)::uuid IS NULL
    AND root.user_id = sqlc.arg(user_id) AND root.parent_id IS NULL
  UNION ALL
  SELECT child.id
  FROM /* TEMPLATE: schema */files child
  JOIN /* TEMPLATE: schema */files selected ON selected.id = sqlc.narg(scope_folder)::uuid
  WHERE sqlc.arg(scope)::text = 'recursive'
    AND selected.user_id = sqlc.arg(user_id)
    AND selected.kind = 'folder' AND selected.status = 'active'
    AND child.parent_id = selected.id AND child.user_id = sqlc.arg(user_id)
  UNION ALL
  SELECT child.id
  FROM /* TEMPLATE: schema */files child
  JOIN scope_files parent ON child.parent_id = parent.id
  WHERE sqlc.arg(scope)::text IN ('drive', 'recursive')
    AND child.user_id = sqlc.arg(user_id)
)
SELECT f.*
FROM /* TEMPLATE: schema */files f
WHERE f.user_id = sqlc.arg(user_id)
  AND (
    (sqlc.arg(scope)::text = 'folder' AND f.parent_id IS NOT DISTINCT FROM sqlc.narg(parent_id)::uuid)
    OR (sqlc.arg(scope)::text <> 'folder' AND f.id IN (SELECT id FROM scope_files))
    OR (
      sqlc.arg(status)::/* TEMPLATE: schema */file_status = 'trashed'
      AND sqlc.narg(parent_id)::uuid IS NULL
      AND f.parent_id IS NOT NULL
      AND NOT EXISTS (
        SELECT 1
        FROM /* TEMPLATE: schema */files parent
        WHERE parent.id = f.parent_id
          AND parent.user_id = f.user_id
          AND parent.status = 'trashed'
      )
    )
  )
  AND f.status = sqlc.arg(status)::/* TEMPLATE: schema */file_status
  AND (sqlc.narg(kind)::/* TEMPLATE: schema */file_kind IS NULL OR f.kind = sqlc.narg(kind)::/* TEMPLATE: schema */file_kind)
  AND (
    sqlc.narg(search)::text IS NULL
    OR (sqlc.arg(search_type)::text = 'regex' AND f.name ~* sqlc.narg(search)::text)
    OR (
      sqlc.arg(search_type)::text = 'text'
      AND (
        f.name % sqlc.narg(search)::text
        OR f.name ILIKE '%' || sqlc.narg(search)::text || '%'
      )
    )
  )
  AND (
    cardinality(sqlc.arg(categories)::text[]) = 0
    OR (CASE
      WHEN f.kind = 'folder' THEN 'other'
      WHEN lower(COALESCE(f.mime_type, '')) LIKE 'image/%' THEN 'image'
      WHEN lower(COALESCE(f.mime_type, '')) LIKE 'audio/%' THEN 'audio'
      WHEN lower(COALESCE(f.mime_type, '')) LIKE 'video/%' THEN 'video'
      WHEN lower(COALESCE(f.mime_type, '')) LIKE 'text/%'
        OR lower(COALESCE(f.mime_type, '')) IN (
          'application/pdf', 'application/json', 'application/xml',
          'application/msword', 'application/rtf',
          'application/vnd.ms-excel', 'application/vnd.ms-powerpoint',
          'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
          'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
          'application/vnd.openxmlformats-officedocument.presentationml.presentation'
        ) THEN 'document'
      WHEN lower(COALESCE(f.mime_type, '')) IN (
          'application/zip', 'application/x-rar-compressed', 'application/x-7z-compressed',
          'application/x-tar', 'application/gzip', 'application/x-bzip2', 'application/x-xz'
        ) OR lower(f.name) ~ '\.(zip|rar|7z|tar|gz|tgz|bz2|xz)$' THEN 'archive'
      ELSE 'other'
    END) = ANY(sqlc.arg(categories)::text[])
  )
  AND (sqlc.narg(updated_after)::timestamptz IS NULL OR f.updated_at >= sqlc.narg(updated_after)::timestamptz)
  AND (sqlc.narg(updated_before)::timestamptz IS NULL OR f.updated_at < sqlc.narg(updated_before)::timestamptz)
  AND (
    sqlc.narg(after_id)::uuid IS NULL
    OR (
      sqlc.arg(sort_by)::text = 'name'
      AND sqlc.narg(after_name)::text IS NOT NULL
      AND (
        (sqlc.arg(sort_order)::text = 'asc' AND (f.name, f.id) > (sqlc.narg(after_name)::text, sqlc.narg(after_id)::uuid))
        OR (sqlc.arg(sort_order)::text = 'desc' AND (f.name, f.id) < (sqlc.narg(after_name)::text, sqlc.narg(after_id)::uuid))
      )
    )
    OR (
      sqlc.arg(sort_by)::text = 'updatedAt'
      AND sqlc.narg(after_updated_at)::timestamptz IS NOT NULL
      AND (
        (sqlc.arg(sort_order)::text = 'asc' AND (f.updated_at, f.id) > (sqlc.narg(after_updated_at)::timestamptz, sqlc.narg(after_id)::uuid))
        OR (sqlc.arg(sort_order)::text = 'desc' AND (f.updated_at, f.id) < (sqlc.narg(after_updated_at)::timestamptz, sqlc.narg(after_id)::uuid))
      )
    )
    OR (
      sqlc.arg(sort_by)::text = 'size'
      AND sqlc.narg(after_size)::bigint IS NOT NULL
      AND (
        (sqlc.arg(sort_order)::text = 'asc' AND (COALESCE(f.size, -1), f.id) > (sqlc.narg(after_size)::bigint, sqlc.narg(after_id)::uuid))
        OR (sqlc.arg(sort_order)::text = 'desc' AND (COALESCE(f.size, -1), f.id) < (sqlc.narg(after_size)::bigint, sqlc.narg(after_id)::uuid))
      )
    )
    OR (
      sqlc.arg(sort_by)::text = 'id'
      AND (
        (sqlc.arg(sort_order)::text = 'asc' AND f.id > sqlc.narg(after_id)::uuid)
        OR (sqlc.arg(sort_order)::text = 'desc' AND f.id < sqlc.narg(after_id)::uuid)
      )
    )
  )
ORDER BY
  CASE WHEN sqlc.arg(sort_by)::text = 'name' AND sqlc.arg(sort_order)::text = 'asc' THEN f.name END ASC,
  CASE WHEN sqlc.arg(sort_by)::text = 'name' AND sqlc.arg(sort_order)::text = 'desc' THEN f.name END DESC,
  CASE WHEN sqlc.arg(sort_by)::text = 'updatedAt' AND sqlc.arg(sort_order)::text = 'asc' THEN f.updated_at END ASC,
  CASE WHEN sqlc.arg(sort_by)::text = 'updatedAt' AND sqlc.arg(sort_order)::text = 'desc' THEN f.updated_at END DESC,
  CASE WHEN sqlc.arg(sort_by)::text = 'size' AND sqlc.arg(sort_order)::text = 'asc' THEN COALESCE(f.size, -1) END ASC,
  CASE WHEN sqlc.arg(sort_by)::text = 'size' AND sqlc.arg(sort_order)::text = 'desc' THEN COALESCE(f.size, -1) END DESC,
  CASE WHEN sqlc.arg(sort_by)::text = 'id' AND sqlc.arg(sort_order)::text = 'asc' THEN f.id END ASC,
  CASE WHEN sqlc.arg(sort_by)::text = 'id' AND sqlc.arg(sort_order)::text = 'desc' THEN f.id END DESC,
  CASE WHEN sqlc.arg(sort_order)::text = 'asc' THEN f.id END ASC,
  CASE WHEN sqlc.arg(sort_order)::text = 'desc' THEN f.id END DESC
LIMIT sqlc.arg(page_size);

-- ListFileParentPaths builds the slash-separated path of the folders above each listed
-- file, from the root down, and returns '/' for a file sitting in the root.
-- name: ListFileParentPaths :many
WITH RECURSIVE ancestors AS (
  SELECT f.id AS listed_id, f.parent_id AS ancestor_id, 0 AS depth
  FROM /* TEMPLATE: schema */files f
  WHERE f.user_id = sqlc.arg(user_id) AND f.id = ANY(sqlc.arg(file_ids)::uuid[])
  UNION ALL
  SELECT a.listed_id, parent.parent_id, a.depth + 1
  FROM ancestors a
  JOIN /* TEMPLATE: schema */files parent ON parent.id = a.ancestor_id
  WHERE parent.user_id = sqlc.arg(user_id)
)
SELECT a.listed_id AS file_id,
       COALESCE('/' || string_agg(node.name, '/' ORDER BY a.depth DESC) FILTER (WHERE node.id IS NOT NULL), '/')::text AS parent_path
FROM ancestors a
LEFT JOIN /* TEMPLATE: schema */files node ON node.id = a.ancestor_id AND node.user_id = sqlc.arg(user_id)
GROUP BY a.listed_id;

-- ListFileCategoryStatistics counts and sums the user's active files per category
-- (image, audio, video, document, archive, other), ordered by category name.
-- name: ListFileCategoryStatistics :many
SELECT category, count(*)::bigint AS total_files, COALESCE(sum(size), 0)::bigint AS total_size
FROM (
  SELECT CASE
    WHEN lower(COALESCE(f.mime_type, '')) LIKE 'image/%' THEN 'image'
    WHEN lower(COALESCE(f.mime_type, '')) LIKE 'audio/%' THEN 'audio'
    WHEN lower(COALESCE(f.mime_type, '')) LIKE 'video/%' THEN 'video'
    WHEN lower(COALESCE(f.mime_type, '')) LIKE 'text/%'
      OR lower(COALESCE(f.mime_type, '')) IN (
        'application/pdf', 'application/json', 'application/xml',
        'application/msword', 'application/rtf',
        'application/vnd.ms-excel', 'application/vnd.ms-powerpoint',
        'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
        'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
        'application/vnd.openxmlformats-officedocument.presentationml.presentation'
      ) THEN 'document'
    WHEN lower(COALESCE(f.mime_type, '')) IN (
        'application/zip', 'application/x-rar-compressed', 'application/x-7z-compressed',
        'application/x-tar', 'application/gzip', 'application/x-bzip2', 'application/x-xz'
      ) OR lower(f.name) ~ '\.(zip|rar|7z|tar|gz|tgz|bz2|xz)$' THEN 'archive'
    ELSE 'other'
  END AS category, f.size
  FROM /* TEMPLATE: schema */files f
  WHERE f.user_id = sqlc.arg(user_id)
    AND f.kind = 'file'
    AND f.status = 'active'
) categorized
GROUP BY category
ORDER BY category;

-- GetDriveStatistics summarises the user's drive in one row: active files and folders,
-- their total size, trashed files, live shares and open or completing uploads.
-- name: GetDriveStatistics :one
SELECT
  count(*) FILTER (WHERE kind = 'file' AND status = 'active')::bigint AS total_files,
  count(*) FILTER (WHERE kind = 'folder' AND status = 'active')::bigint AS total_folders,
  COALESCE(sum(size) FILTER (WHERE kind = 'file' AND status = 'active'), 0)::bigint AS total_bytes,
  count(*) FILTER (WHERE kind = 'file' AND status = 'trashed')::bigint AS trashed_files,
  (SELECT count(*)::bigint FROM /* TEMPLATE: schema */file_shares WHERE owner_id = sqlc.arg(user_id) AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())) AS active_shares,
  (SELECT count(*)::bigint FROM /* TEMPLATE: schema */upload_sessions WHERE user_id = sqlc.arg(user_id) AND state IN ('open', 'completing')) AS open_uploads
FROM /* TEMPLATE: schema */files
WHERE user_id = sqlc.arg(user_id);

-- LockActiveFiles row-locks the user's active files with the given ids for the rest of
-- the transaction and returns them; ids that are not active are not locked. The rows are
-- locked in id order because the lock is a mutex over the whole set of concurrently
-- moving entries: a statement without ORDER BY locks rows in whatever order its plan
-- produces them, so two moves that name the same two folders in opposite roles could
-- still take their row locks in opposite orders and deadlock.
-- name: LockActiveFiles :many
SELECT *
FROM /* TEMPLATE: schema */files
WHERE user_id = sqlc.arg(user_id)
  AND id = ANY(sqlc.arg(file_ids)::uuid[])
  AND status = 'active'
ORDER BY id
FOR UPDATE;

-- LockActiveFolder row-locks one active folder of the user for the rest of the
-- transaction and returns it; a trashed or foreign folder returns no row.
-- name: LockActiveFolder :one
SELECT *
FROM /* TEMPLATE: schema */files
WHERE id = sqlc.arg(folder_id)
  AND user_id = sqlc.arg(user_id)
  AND kind = 'folder'
  AND status = 'active'
FOR UPDATE;

-- LockActiveDestinationEntries row-locks only the active destination children whose
-- names collide with the requested ones and returns their id and name.
-- name: LockActiveDestinationEntries :many
-- Locks only the destination entries the move collides with by name. Locking every
-- child of the destination held the whole folder for the length of the
-- transaction, which in a large folder contended with every other writer of that
-- folder. A name that appears after this statement is still caught by the unique
-- index on active child names.
SELECT id, name
FROM /* TEMPLATE: schema */files
WHERE user_id = sqlc.arg(user_id)
  AND parent_id IS NOT DISTINCT FROM sqlc.narg(parent_id)::uuid
  AND status = 'active'
  AND name = ANY(sqlc.arg(names)::text[])
FOR UPDATE;

-- ListActiveDestinationEntries lists the id and name of every active child of the
-- destination folder, NULL meaning the drive root, for name-conflict checks.
-- name: ListActiveDestinationEntries :many
SELECT id, name
FROM /* TEMPLATE: schema */files
WHERE user_id = sqlc.arg(user_id)
  AND parent_id IS NOT DISTINCT FROM sqlc.narg(parent_id)::uuid
  AND status = 'active';

-- ListFileAncestorIDs returns the file and every active ancestor above it up to the
-- root, scoped to the user, as an unordered id list; an unknown file yields no rows.
-- The walk stops at the first ancestor that is not active, so a file that hangs below
-- a trashed or deletion_pending folder never reports that folder's own ancestors and
-- its chain cannot reach a share root the caller is no longer entitled to.
-- name: ListFileAncestorIDs :many
WITH RECURSIVE ancestors AS (
  SELECT file.id, file.parent_id
  FROM /* TEMPLATE: schema */files AS file
  WHERE file.id = sqlc.arg(file_id)
    AND file.user_id = sqlc.arg(user_id)
  UNION ALL
  SELECT parent.id, parent.parent_id
  FROM /* TEMPLATE: schema */files AS parent
  JOIN ancestors AS child ON parent.id = child.parent_id
  WHERE parent.user_id = sqlc.arg(user_id)
    AND parent.status = 'active'
)
SELECT id FROM ancestors;

-- ListFileSubtreeIDs returns the file and every descendant below it in the user's tree
-- as an unordered id list, whatever their status.
-- name: ListFileSubtreeIDs :many
WITH RECURSIVE subtree AS (
  SELECT root.id FROM /* TEMPLATE: schema */files root WHERE root.id = sqlc.arg(file_id) AND root.user_id = sqlc.arg(user_id)
  UNION ALL
  SELECT f.id FROM /* TEMPLATE: schema */files f JOIN subtree s ON f.parent_id = s.id WHERE f.user_id = sqlc.arg(user_id)
)
SELECT id FROM subtree;

-- TrashFileSubtrees trashes the given active files together with their active
-- descendants and returns every changed row with its new deleted_at and generation.
-- name: TrashFileSubtrees :many
WITH RECURSIVE target AS (
  SELECT root.id
  FROM /* TEMPLATE: schema */files root
  WHERE root.user_id = sqlc.arg(user_id)
    AND root.id = ANY(sqlc.arg(file_ids)::uuid[])
    AND root.status = 'active'
  UNION
  SELECT child.id
  FROM /* TEMPLATE: schema */files child
  JOIN target parent ON child.parent_id = parent.id
  WHERE child.user_id = sqlc.arg(user_id)
    AND child.status = 'active'
)
UPDATE /* TEMPLATE: schema */files AS target_file
SET status = 'trashed',
    deleted_at = now(),
    generation = target_file.generation + 1,
    updated_at = now()
WHERE target_file.user_id = sqlc.arg(user_id)
  AND target_file.id IN (SELECT target.id FROM target)
RETURNING target_file.*;

-- RevokeSharesForFileSubtrees revokes the user's live shares on the given files and
-- all their descendants; shares revoked earlier keep their original timestamp.
-- name: RevokeSharesForFileSubtrees :exec
WITH RECURSIVE target AS (
  SELECT root.id FROM /* TEMPLATE: schema */files root WHERE root.user_id = sqlc.arg(user_id) AND root.id = ANY(sqlc.arg(file_ids)::uuid[])
  UNION
  SELECT child.id FROM /* TEMPLATE: schema */files child JOIN target parent ON child.parent_id = parent.id WHERE child.user_id = sqlc.arg(user_id)
)
UPDATE /* TEMPLATE: schema */file_shares AS share
SET revoked_at = COALESCE(share.revoked_at, now())
WHERE share.owner_id = sqlc.arg(user_id)
  AND share.revoked_at IS NULL
  AND share.file_id IN (SELECT target.id FROM target);

-- MarkFileSubtreeDeletionPending moves one active file and its active descendants to
-- deletion_pending, keeping an earlier deleted_at if there is one; nothing is
-- returned.
-- name: MarkFileSubtreeDeletionPending :exec
WITH RECURSIVE target AS (
  SELECT root.id FROM /* TEMPLATE: schema */files root WHERE root.id = sqlc.arg(file_id) AND root.user_id = sqlc.arg(user_id) AND root.status = 'active'
  UNION ALL
  SELECT child.id FROM /* TEMPLATE: schema */files child JOIN target parent ON child.parent_id = parent.id WHERE child.user_id = sqlc.arg(user_id) AND child.status = 'active'
)
UPDATE /* TEMPLATE: schema */files AS target_file
SET status = 'deletion_pending',
    deleted_at = COALESCE(target_file.deleted_at, now()),
    generation = target_file.generation + 1,
    updated_at = now()
WHERE target_file.user_id = sqlc.arg(user_id) AND target_file.id IN (SELECT target.id FROM target);

-- MarkFileSubtreesDeletionPending moves several active files and their active
-- descendants to deletion_pending, keeping an earlier deleted_at; nothing is returned.
-- name: MarkFileSubtreesDeletionPending :exec
WITH RECURSIVE target AS (
  SELECT root.id
  FROM /* TEMPLATE: schema */files AS root
  WHERE root.id = ANY(sqlc.arg(file_ids)::uuid[])
    AND root.user_id = sqlc.arg(user_id)
    AND root.status = 'active'
  UNION
  SELECT child.id
  FROM /* TEMPLATE: schema */files AS child
  JOIN target AS parent ON child.parent_id = parent.id
  WHERE child.user_id = sqlc.arg(user_id)
    AND child.status = 'active'
)
UPDATE /* TEMPLATE: schema */files AS target_file
SET status = 'deletion_pending',
    deleted_at = COALESCE(target_file.deleted_at, now()),
    generation = target_file.generation + 1,
    updated_at = now()
WHERE target_file.user_id = sqlc.arg(user_id)
  AND target_file.id IN (SELECT target.id FROM target);

-- RevokeSharesForFileSubtree revokes the user's live shares on one file and every
-- descendant of it, leaving already revoked shares untouched.
-- name: RevokeSharesForFileSubtree :exec
WITH RECURSIVE target AS (
  SELECT root.id FROM /* TEMPLATE: schema */files root WHERE root.id = sqlc.arg(file_id) AND root.user_id = sqlc.arg(user_id)
  UNION ALL
  SELECT child.id FROM /* TEMPLATE: schema */files child JOIN target parent ON child.parent_id = parent.id WHERE child.user_id = sqlc.arg(user_id)
)
UPDATE /* TEMPLATE: schema */file_shares AS share
SET revoked_at = COALESCE(share.revoked_at, now())
WHERE share.owner_id = sqlc.arg(user_id)
  AND share.revoked_at IS NULL
  AND share.file_id IN (SELECT target.id FROM target);

-- ListActiveNames returns the names of the user's active children of the given parent,
-- optionally excluding one id, which is the name-conflict check before an insert.
-- name: ListActiveNames :many
SELECT name
FROM /* TEMPLATE: schema */files
WHERE user_id = sqlc.arg(user_id)
  AND parent_id IS NOT DISTINCT FROM sqlc.narg(parent_id)::uuid
  AND status = 'active'
  AND (sqlc.narg(exclude_id)::uuid IS NULL OR id <> sqlc.narg(exclude_id)::uuid);

-- MoveFilesWithNames re-parents and renames several files in one statement, pairing
-- file_ids with names by array position and returning the updated rows; a stale
-- expected_generation leaves that file out of the result.
-- name: MoveFilesWithNames :many
WITH arrays AS (
  SELECT sqlc.arg(file_ids)::uuid[] AS file_ids,
         sqlc.arg(names)::text[] AS names
), input AS (
  SELECT arrays.file_ids[index] AS file_id,
         arrays.names[index] AS name
  FROM arrays
  CROSS JOIN LATERAL generate_subscripts(arrays.file_ids, 1) AS index
)
UPDATE /* TEMPLATE: schema */files AS file
SET parent_id = sqlc.narg(parent_id),
    name = input.name,
    generation = file.generation + 1,
    updated_at = now()
FROM input
WHERE file.id = input.file_id
  AND file.user_id = sqlc.arg(user_id)
  AND file.status = 'active'
  AND (sqlc.narg(expected_generation)::bigint IS NULL OR file.generation = sqlc.narg(expected_generation)::bigint)
RETURNING file.*;

-- LoadFileSubtree returns the file and its whole subtree, each row carrying its depth,
-- ordered by depth then id; rows of every status are included.
-- name: LoadFileSubtree :many
WITH RECURSIVE tree AS (
    SELECT f.*, 0::integer AS depth
    FROM /* TEMPLATE: schema */files f
    WHERE f.id = sqlc.arg(root_id) AND f.user_id = sqlc.arg(user_id)
    UNION ALL
    SELECT child.*, tree.depth + 1
    FROM /* TEMPLATE: schema */files child
    JOIN tree ON child.parent_id = tree.id
    WHERE child.user_id = sqlc.arg(user_id)
)
SELECT id, user_id, parent_id, name, kind, mime_type, size,
       hash_algorithm, hash_value, encryption, encryption_key_version, status,
       mod_time, generation, created_at, updated_at, deleted_at, depth
FROM tree
ORDER BY depth, id;

-- LoadFileSubtrees returns the subtrees of several roots with a depth column, ordered
-- by depth then id and including rows of every status.
-- name: LoadFileSubtrees :many
WITH RECURSIVE tree AS (
    SELECT f.*, 0::integer AS depth
    FROM /* TEMPLATE: schema */files f
    WHERE f.id = ANY(sqlc.arg(root_ids)::uuid[])
      AND f.user_id = sqlc.arg(user_id)
    UNION ALL
    SELECT child.*, tree.depth + 1
    FROM /* TEMPLATE: schema */files child
    JOIN tree ON child.parent_id = tree.id
    WHERE child.user_id = sqlc.arg(user_id)
)
SELECT id, user_id, parent_id, name, kind, mime_type, size,
       hash_algorithm, hash_value, encryption, encryption_key_version, status,
       mod_time, generation, created_at, updated_at, deleted_at, depth
FROM tree
ORDER BY depth, id;

-- InsertCopiedFiles inserts the file rows of a copy operation from a JSON array,
-- forcing the active status and generation 1, and returns the created rows.
-- name: InsertCopiedFiles :many
INSERT INTO /* TEMPLATE: schema */files AS file (
    id, user_id, parent_id, name, kind, mime_type, size,
    hash_algorithm, hash_value, encryption, encryption_key_version,
    status, mod_time, generation
)
SELECT input.id, input.user_id, input.parent_id, input.name,
       input.kind::/* TEMPLATE: schema */file_kind, input.mime_type, input.size,
       input.hash_algorithm, input.hash_value, input.encryption,
       input.encryption_key_version, 'active', input.mod_time, 1
FROM jsonb_to_recordset(sqlc.arg(files)::jsonb) AS input(
    id uuid, user_id bigint, parent_id uuid, name text,
    kind text, mime_type text, size bigint, hash_algorithm text, hash_value text,
    encryption boolean, encryption_key_version integer, mod_time timestamptz
)
RETURNING file.*;

-- InsertCopiedFileParts inserts the part rows of a copy operation, base64-decoding
-- their block hashes, and returns the number of rows inserted.
-- name: InsertCopiedFileParts :execrows
INSERT INTO /* TEMPLATE: schema */file_parts (
    file_id, part_no, channel_id, message_id, plain_size, stored_size,
    checksum, salt, block_hashes
)
SELECT input.file_id, input.part_no, input.channel_id, input.message_id,
       input.plain_size, input.stored_size, input.checksum, input.salt,
       decode(input.block_hashes, 'base64')
FROM jsonb_to_recordset(sqlc.arg(parts)::jsonb) AS input(
    file_id uuid, part_no integer, channel_id bigint, message_id bigint,
    plain_size bigint, stored_size bigint, checksum text, salt text,
    block_hashes text
);

-- MarkFileIDsDeletionPending moves the user's files with the given ids to
-- deletion_pending whatever their status, keeping an earlier deleted_at and bumping
-- the generation; nothing is returned.
-- name: MarkFileIDsDeletionPending :exec
UPDATE /* TEMPLATE: schema */files
SET status = 'deletion_pending',
    deleted_at = COALESCE(deleted_at, now()),
    updated_at = now(),
    generation = generation + 1
WHERE user_id = sqlc.arg(user_id)
  AND id = ANY(sqlc.arg(file_ids)::uuid[]);

-- QueueFileSubtreePurge moves one trashed file and every descendant below it, whatever
-- their status, to deletion_pending and returns the changed rows; a root that is not
-- trashed returns nothing.
-- name: QueueFileSubtreePurge :many
WITH RECURSIVE target AS (
  SELECT root.id
  FROM /* TEMPLATE: schema */files root
  WHERE root.id = sqlc.arg(file_id)
    AND root.user_id = sqlc.arg(user_id)
    AND root.status = 'trashed'
  UNION ALL
  SELECT child.id
  FROM /* TEMPLATE: schema */files child
  JOIN target parent ON child.parent_id = parent.id
  WHERE child.user_id = sqlc.arg(user_id)
)
UPDATE /* TEMPLATE: schema */files AS target_file
SET status = 'deletion_pending',
    deleted_at = COALESCE(target_file.deleted_at, now()),
    updated_at = now()
WHERE target_file.user_id = sqlc.arg(user_id)
  AND target_file.id IN (SELECT target.id FROM target)
RETURNING target_file.*;


-- MarkAllTrashedDeletionPending empties the user's trash by moving every trashed file
-- to deletion_pending and returns the affected ids.
-- name: MarkAllTrashedDeletionPending :many
UPDATE /* TEMPLATE: schema */files
SET status = 'deletion_pending',
    deleted_at = COALESCE(deleted_at, now()),
    updated_at = now(),
    generation = generation + 1
WHERE user_id = sqlc.arg(user_id)
  AND status = 'trashed'
RETURNING id;

-- ListFilePartMessageRefs returns the channel and message of every part of the given
-- files, ordered by channel, so those Telegram messages can be deleted first.
-- name: ListFilePartMessageRefs :many
SELECT channel_id, message_id
FROM /* TEMPLATE: schema */file_parts
WHERE file_id = ANY(sqlc.arg(file_ids)::uuid[])
ORDER BY channel_id, message_id;

-- DeleteFilePartsByFileIDs deletes every part row of the given files; the query is not
-- scoped by user, so ownership must have been resolved by the caller.
-- name: DeleteFilePartsByFileIDs :exec
DELETE FROM /* TEMPLATE: schema */file_parts
WHERE file_id = ANY(sqlc.arg(file_ids)::uuid[]);

-- ClearUploadSessionParentsByFileIDs detaches the user's upload sessions from the
-- given parent ids, which keeps them usable after their target folder was removed.
-- name: ClearUploadSessionParentsByFileIDs :exec
UPDATE /* TEMPLATE: schema */upload_sessions
SET parent_id = NULL,
    updated_at = now()
WHERE user_id = sqlc.arg(user_id)
  AND parent_id = ANY(sqlc.arg(file_ids)::uuid[]);

-- UpdateFilePartSizes fills in the plain and stored size of one part, but only while
-- at least one of the two is still NULL, so a known size is never overwritten.
-- name: UpdateFilePartSizes :execrows
UPDATE /* TEMPLATE: schema */file_parts
SET plain_size = sqlc.arg(plain_size),
    stored_size = sqlc.arg(stored_size)
WHERE file_id = sqlc.arg(file_id)
  AND part_no = sqlc.arg(part_no)
  AND (plain_size IS NULL OR stored_size IS NULL);

-- UpdateFilePartSizesMany fills in the sizes of many parts of one file from a JSON
-- array matched by part number, again only where a size is still missing.
-- name: UpdateFilePartSizesMany :execrows
UPDATE /* TEMPLATE: schema */file_parts AS part
SET plain_size = input.plain_size,
    stored_size = input.stored_size
FROM jsonb_to_recordset(sqlc.arg(parts)::jsonb) AS input(
    part_no integer, plain_size bigint, stored_size bigint
)
WHERE part.file_id = sqlc.arg(file_id)
  AND part.part_no = input.part_no
  AND (part.plain_size IS NULL OR part.stored_size IS NULL);
