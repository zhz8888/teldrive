-- +goose Up

-- The shared-with-me listing orders and pages by (updated_at, id) on
-- file_access_grants, but the only index for a grantee was built on created_at, so
-- every page sorted that grantee's whole grant set and a grant updated while a
-- client was paging could be skipped or repeated. The index is rebuilt on the
-- columns the query actually uses.
DROP INDEX IF EXISTS /* TEMPLATE: schema */file_access_grants_grantee_idx;

CREATE INDEX file_access_grants_grantee_idx
    ON /* TEMPLATE: schema */file_access_grants (grantee_id, updated_at DESC, id)
    WHERE revoked_at IS NULL;

-- +goose Down

DROP INDEX IF EXISTS /* TEMPLATE: schema */file_access_grants_grantee_idx;

CREATE INDEX file_access_grants_grantee_idx
    ON /* TEMPLATE: schema */file_access_grants (grantee_id, created_at DESC, id)
    WHERE revoked_at IS NULL;
