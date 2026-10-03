-- +goose Up

-- The table was created for an audit trail the server never wrote: no query
-- inserted a row, the two statements that named it had no callers, and nothing
-- read it back, so it could only ever be empty. Removing it keeps the schema
-- honest about what the server implements, exactly as the idempotency table was
-- removed once the contract stopped declaring that guarantee.
DROP TABLE IF EXISTS /* TEMPLATE: schema */audit_events;

-- +goose Down

CREATE TABLE /* TEMPLATE: schema */audit_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id BIGINT REFERENCES /* TEMPLATE: schema */users(user_id) ON DELETE SET NULL,
    action TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id TEXT,
    metadata JSONB,
    request_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT audit_events_action_not_blank CHECK (length(btrim(action)) > 0),
    CONSTRAINT audit_events_resource_type_not_blank CHECK (length(btrim(resource_type)) > 0)
);

CREATE INDEX audit_events_user_created_idx
    ON /* TEMPLATE: schema */audit_events (user_id, created_at DESC, id);
