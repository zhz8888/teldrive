-- +goose Up

-- The table backed an idempotency guarantee the contract no longer declares: the
-- Idempotency-Key header was removed from the TypeSpec operations, no query ever
-- reserved or replayed a key, and no code ever wrote a row. Dropping the table
-- keeps the schema honest about what the server implements.
DROP TABLE IF EXISTS /* TEMPLATE: schema */idempotency_keys;

-- +goose Down

CREATE TABLE /* TEMPLATE: schema */idempotency_keys (
    user_id BIGINT NOT NULL REFERENCES /* TEMPLATE: schema */users(user_id) ON DELETE CASCADE,
    scope TEXT NOT NULL,
    key UUID NOT NULL,
    request_hash BYTEA NOT NULL,
    resource_type TEXT,
    resource_id TEXT,
    response_ciphertext BYTEA,
    completed_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, scope, key),
    CONSTRAINT idempotency_scope_not_blank CHECK (length(btrim(scope)) > 0)
);

CREATE INDEX idempotency_keys_expiry_idx
    ON /* TEMPLATE: schema */idempotency_keys (expires_at);
