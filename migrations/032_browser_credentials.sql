-- 与原令牌独立，关闭功能不改变已有令牌采集。
CREATE TABLE collector_browser_credentials (
    account_id BIGINT PRIMARY KEY REFERENCES upstream_accounts(id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT false,
    username TEXT NOT NULL CHECK (btrim(username) <> ''),
    password_ciphertext BYTEA NOT NULL,
    session_ciphertext BYTEA,
    state TEXT NOT NULL DEFAULT 'unverified'
        CHECK (state IN ('unverified', 'ready', 'invalid', 'needs_action')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

-- 地址/站型可被管理页和导入更新，统一在数据库撤销旧站凭据。
CREATE FUNCTION invalidate_browser_site() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.base_url IS DISTINCT FROM OLD.base_url
       OR NEW.site_family IS DISTINCT FROM OLD.site_family THEN
        DELETE FROM collector_browser_credentials
         WHERE account_id IN (SELECT id FROM upstream_accounts WHERE channel_id=NEW.id);
    ELSIF NEW.status IS DISTINCT FROM OLD.status THEN
        UPDATE collector_browser_credentials
           SET session_ciphertext=NULL, state='unverified', updated_at=clock_timestamp()
         WHERE account_id IN (SELECT id FROM upstream_accounts WHERE channel_id=NEW.id);
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER browser_site_changed AFTER UPDATE OF base_url, site_family, status ON channels
    FOR EACH ROW EXECUTE FUNCTION invalidate_browser_site();

CREATE FUNCTION invalidate_browser_account() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.external_user_id IS DISTINCT FROM OLD.external_user_id
       OR NEW.status IS DISTINCT FROM OLD.status THEN
        UPDATE collector_browser_credentials
           SET session_ciphertext=NULL, state='unverified', updated_at=clock_timestamp()
         WHERE account_id=NEW.id;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER browser_account_changed AFTER UPDATE OF external_user_id, status ON upstream_accounts
    FOR EACH ROW EXECUTE FUNCTION invalidate_browser_account();
