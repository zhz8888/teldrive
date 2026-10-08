-- GetFileViewState returns the saved reading or playback state of one file for the
-- user, or no row when the user never opened it.
-- name: GetFileViewState :one
SELECT *
FROM /* TEMPLATE: schema */file_view_states
WHERE user_id = sqlc.arg(user_id)
  AND file_id = sqlc.arg(file_id);

-- UpsertFileViewState stores or replaces the user's viewer state for one file and
-- returns it; the insert only fires for an active file of that user, so a folder,
-- a trashed file or another owner's file stores nothing.
-- name: UpsertFileViewState :one
INSERT INTO /* TEMPLATE: schema */file_view_states (
    user_id, file_id, viewer_kind, position, preferences, bookmarks
) SELECT
    sqlc.arg(user_id), sqlc.arg(file_id), sqlc.arg(viewer_kind),
    sqlc.arg(position), sqlc.arg(preferences), sqlc.arg(bookmarks)
FROM /* TEMPLATE: schema */files
WHERE id = sqlc.arg(file_id)
  AND user_id = sqlc.arg(user_id)
  AND kind = 'file'
  AND status = 'active'
ON CONFLICT (user_id, file_id) DO UPDATE
SET viewer_kind = EXCLUDED.viewer_kind,
    position = EXCLUDED.position,
    preferences = EXCLUDED.preferences,
    bookmarks = EXCLUDED.bookmarks,
    updated_at = now()
RETURNING *;

-- DeleteFileViewState removes the user's saved state for one file and returns the
-- rows deleted, which is zero when there was nothing saved.
-- name: DeleteFileViewState :execrows
DELETE FROM /* TEMPLATE: schema */file_view_states
WHERE user_id = sqlc.arg(user_id)
  AND file_id = sqlc.arg(file_id);
