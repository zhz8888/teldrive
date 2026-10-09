-- +goose Up

-- 000006 indexed users(role, created_at, user_id) as if the user list were filtered
-- by role, but no query does that: ListUsers filters on display name, username or
-- user id and orders by created_at, and the role updates match on the primary key,
-- so the leading column of this index is never constrained. It therefore buys no
-- read and costs a write on every user row, so it is dropped.
DROP INDEX IF EXISTS /* TEMPLATE: schema */users_role_idx;

-- +goose Down

CREATE INDEX users_role_idx
    ON /* TEMPLATE: schema */users (role, created_at, user_id);
