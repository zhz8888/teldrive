-- +goose Up

-- 000003 wrote emit_share_event before file_shares had a permission column, so
-- its change test compares only password, expiry, download limit and revocation.
-- Ordering makes this file run after 000006, which is what guarantees the column
-- below exists. UpdateFileShare can change permission on its own, and such an
-- update looked like a no-op to the trigger: it returned NEW early and emitted no
-- share.updated event, so an event-stream client kept showing the old permission
-- until some unrelated change forced it to refetch.
--
-- CREATE OR REPLACE FUNCTION redefines the body in place and leaves the
-- file_shares_emit_user_event trigger bound to it, so no trigger work is needed.
-- The event payload is left exactly as 000003 built it: this migration only stops
-- the trigger from treating a permission-only update as a no-op.
--
-- This is a forward fix that cannot be rolled back to the old body: the old body
-- is exactly the defect, and a schema at version 000006 or later always has the
-- column the new comparison reads.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION /* TEMPLATE: schema */emit_share_event() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    row_value RECORD;
    event_name TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        row_value := OLD;
        event_name := 'share.deleted';
    ELSIF TG_OP = 'INSERT' THEN
        row_value := NEW;
        event_name := 'share.created';
    ELSE
        row_value := NEW;
        IF ROW(OLD.password_hash, OLD.expires_at, OLD.max_downloads, OLD.revoked_at, OLD.permission)
            IS NOT DISTINCT FROM
           ROW(NEW.password_hash, NEW.expires_at, NEW.max_downloads, NEW.revoked_at, NEW.permission) THEN
            RETURN NEW;
        END IF;
        event_name := CASE
            WHEN OLD.revoked_at IS NULL AND NEW.revoked_at IS NOT NULL THEN 'share.revoked'
            ELSE 'share.updated'
        END;
    END IF;

    EXECUTE format(
        'INSERT INTO %I.user_events (
             user_id, event_type, resource_type, resource_id, payload
         ) VALUES ($1, $2, $3, $4, $5)',
        TG_TABLE_SCHEMA
    ) USING
        row_value.owner_id,
        event_name,
        'share',
        row_value.id::text,
        jsonb_strip_nulls(jsonb_build_object(
            'fileId', row_value.file_id,
            'expiresAt', row_value.expires_at,
            'revokedAt', row_value.revoked_at
        ));

    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose Down

-- The previous definition is deliberately not restored: it is the defect this
-- migration fixes, so rolling back would silently drop permission-only updates
-- again. The function stays as replaced even when this version is rolled back.
SELECT 1;
