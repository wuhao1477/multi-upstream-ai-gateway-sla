-- 032 已应用过，向前迁移删除撤回的账号密码/会话存储。
DROP TRIGGER browser_site_changed ON channels;
DROP TRIGGER browser_account_changed ON upstream_accounts;
DROP FUNCTION invalidate_browser_site();
DROP FUNCTION invalidate_browser_account();
DROP TABLE collector_browser_credentials;

CREATE TABLE collector_cookie_credentials (
    account_id BIGINT PRIMARY KEY REFERENCES upstream_accounts(id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT false,
    cookie_ciphertext BYTEA NOT NULL,
    state TEXT NOT NULL DEFAULT 'unverified'
        CHECK (state IN ('unverified', 'ready', 'expired', 'needs_action')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE FUNCTION invalidate_cookie_site() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.base_url IS DISTINCT FROM OLD.base_url
       OR NEW.site_family IS DISTINCT FROM OLD.site_family THEN
        DELETE FROM collector_cookie_credentials
         WHERE account_id IN (SELECT id FROM upstream_accounts WHERE channel_id=NEW.id);
    ELSIF NEW.status IS DISTINCT FROM OLD.status THEN
        UPDATE collector_cookie_credentials
           SET state='unverified', updated_at=clock_timestamp()
         WHERE account_id IN (SELECT id FROM upstream_accounts WHERE channel_id=NEW.id);
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER cookie_site_changed AFTER UPDATE OF base_url, site_family, status ON channels
    FOR EACH ROW EXECUTE FUNCTION invalidate_cookie_site();

CREATE FUNCTION invalidate_cookie_account() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.external_user_id IS DISTINCT FROM OLD.external_user_id
       OR NEW.status IS DISTINCT FROM OLD.status THEN
        UPDATE collector_cookie_credentials
           SET state='unverified', updated_at=clock_timestamp()
         WHERE account_id=NEW.id;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER cookie_account_changed AFTER UPDATE OF external_user_id, status ON upstream_accounts
    FOR EACH ROW EXECUTE FUNCTION invalidate_cookie_account();
