-- ListDeletionPendingRoots returns up to 1000 deletion-pending files that are roots
-- of their pending subtree, meaning no deletion-pending parent above them, oldest
-- update first, so the purge worker deletes whole subtrees from the top.
-- name: ListDeletionPendingRoots :many
SELECT f.user_id, f.id AS file_id
FROM /* TEMPLATE: schema */files f
WHERE f.status = 'deletion_pending'
  AND (
    f.parent_id IS NULL
    OR NOT EXISTS (
      SELECT 1
      FROM /* TEMPLATE: schema */files parent
      WHERE parent.id = f.parent_id
        AND parent.user_id = f.user_id
        AND parent.status = 'deletion_pending'
    )
  )
ORDER BY f.updated_at, f.id
LIMIT 1000;

-- ListTrashedRootsBefore returns up to 1000 trashed roots deleted at or before the
-- cutoff, skipping rows whose parent is itself trashed, oldest deletion first, so the
-- expiry worker purges a whole subtree once instead of walking into it.
-- name: ListTrashedRootsBefore :many
SELECT f.user_id, f.id AS file_id
FROM /* TEMPLATE: schema */files f
WHERE f.status = 'trashed'
  AND f.deleted_at IS NOT NULL
  AND f.deleted_at <= sqlc.arg(deleted_before)
  AND (
    f.parent_id IS NULL
    OR NOT EXISTS (
      SELECT 1
      FROM /* TEMPLATE: schema */files parent
      WHERE parent.id = f.parent_id
        AND parent.user_id = f.user_id
        AND parent.status = 'trashed'
    )
  )
ORDER BY f.deleted_at, f.id
LIMIT 1000;
