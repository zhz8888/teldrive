-- +goose Up

-- The prefix was stored so a share could be recognised in a list without the
-- token, but nothing ever read it: resolution goes through token_hash alone, and
-- every insert therefore carried a value with no reader. Dropping the column keeps
-- the schema honest about what the server implements.
ALTER TABLE /* TEMPLATE: schema */file_shares DROP COLUMN IF EXISTS token_prefix;

-- +goose Down

-- The original prefix cannot be reconstructed, so the column comes back empty for
-- existing rows rather than failing on the NOT NULL constraint.
ALTER TABLE /* TEMPLATE: schema */file_shares ADD COLUMN IF NOT EXISTS token_prefix TEXT NOT NULL DEFAULT '';
