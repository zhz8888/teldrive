-- +goose Up

-- The pending-deletion purge and the trash sweep both select rows by status
-- alone and then order by the timestamp they filter on. The existing
-- (user_id, parent_id, status, …) index cannot serve either query, so each sweep
-- scanned and sorted the whole files table; these two partial indexes make a
-- sweep proportional to the rows it is about to delete instead of to the size of
-- the catalog. They also supply the ordering, so no separate sort is needed.
CREATE INDEX files_deletion_pending_idx
    ON /* TEMPLATE: schema */files (updated_at, id)
    WHERE status = 'deletion_pending';

CREATE INDEX files_trashed_deleted_idx
    ON /* TEMPLATE: schema */files (deleted_at, id)
    WHERE status = 'trashed';

-- +goose Down

DROP INDEX /* TEMPLATE: schema */files_deletion_pending_idx;
DROP INDEX /* TEMPLATE: schema */files_trashed_deleted_idx;
