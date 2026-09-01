package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

func newAccount(t *testing.T, h http.Handler, tok string, channelID int64) int64 {
	t.Helper()
	code, body := do(t, h, tok, "POST", "/admin/accounts",
		fmt.Sprintf(`{"channel_id":%d,"external_user_id":"u-before","balance_group_key":"bg-before"}`,
			channelID))
	if code != http.StatusCreated {
		t.Fatalf("建账号应 201，得 %d：%s", code, body)
	}
	var out struct{ ID int64 }
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析建账号响应: %v（%s）", err, body)
	}
	return out.ID
}

func newKey(t *testing.T, h http.Handler, tok string, accountID int64, secret string) int64 {
	t.Helper()
	code, body := do(t, h, tok, "POST", "/admin/keys",
		fmt.Sprintf(`{"account_id":%d,"secret":%q,"external_ref":"ref-before"}`,
			accountID, secret))
	if code != http.StatusCreated {
		t.Fatalf("建 Key 应 201，得 %d：%s", code, body)
	}
	var out struct{ ID int64 }
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析建 Key 响应: %v（%s）", err, body)
	}
	return out.ID
}

func wipeCRUD(ctx context.Context, t *testing.T, conn *pgx.Conn, base string) {
	t.Helper()
	const match = `lower(rtrim(base_url,'/')) = lower(rtrim($1,'/'))`
	if _, err := conn.Exec(ctx, `
DELETE FROM upstream_keys
 WHERE account_id IN (
   SELECT a.id FROM upstream_accounts a
   JOIN channels c ON c.id = a.channel_id
   WHERE `+match+`
 ) OR channel_group_id IN (
   SELECT g.id FROM channel_groups g
   JOIN channels c ON c.id = g.channel_id
   WHERE `+match+`
 )`, base); err != nil {
		t.Fatalf("清 %s 的 Key 残留: %v", base, err)
	}
	wipe(ctx, t, conn, base)
}

func TestPatchAccountUpdatesMutableFields(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	_, h, tok := httpServer(t)
	base := "https://patch-account.example.invalid"
	wipeCRUD(ctx, t, conn, base)
	defer wipeCRUD(context.Background(), t, conn, base)

	channelID := newChannel(t, h, tok, "账号 PATCH 靶子", base)
	accountID := newAccount(t, h, tok, channelID)
	until := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)

	code, body := do(t, h, tok, "PATCH", fmt.Sprintf("/admin/accounts/%d", accountID),
		fmt.Sprintf(`{"external_user_id":"u-after","balance_group_key":"bg-after",`+
			`"status":"disabled","disabled_reason":"人工暂停","disabled_until":%q}`,
			until.Format(time.RFC3339)))
	if code != http.StatusOK {
		t.Fatalf("账号 PATCH 应 200，得 %d：%s", code, body)
	}

	var got struct {
		ExternalUserID  string
		BalanceGroupKey string
		Status          string
		DisabledReason  string
		DisabledUntil   time.Time
	}
	if err := conn.QueryRow(ctx, `
SELECT COALESCE(external_user_id,''), COALESCE(balance_group_key,''),
       status, COALESCE(disabled_reason,''), disabled_until
  FROM upstream_accounts WHERE id=$1`, accountID).Scan(&got.ExternalUserID,
		&got.BalanceGroupKey, &got.Status, &got.DisabledReason, &got.DisabledUntil); err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if got.ExternalUserID != "u-after" || got.BalanceGroupKey != "bg-after" ||
		got.Status != "disabled" || got.DisabledReason != "人工暂停" ||
		!got.DisabledUntil.Equal(until) {
		t.Fatalf("账号 PATCH 未按请求落库：%+v，期望 disabled/u-after/bg-after/%s",
			got, until.Format(time.RFC3339))
	}

	code, body = do(t, h, tok, "PATCH", fmt.Sprintf("/admin/accounts/%d", accountID),
		`{"status":"active"}`)
	if code != http.StatusOK {
		t.Fatalf("账号启用应 200，得 %d：%s", code, body)
	}
	var reason *string
	var disabledUntil *time.Time
	if err := conn.QueryRow(ctx,
		`SELECT disabled_reason, disabled_until FROM upstream_accounts WHERE id=$1`,
		accountID).Scan(&reason, &disabledUntil); err != nil {
		t.Fatalf("读账号启用结果: %v", err)
	}
	if reason != nil || disabledUntil != nil {
		t.Fatalf("账号启用必须清掉停用原因与期限，实际 reason=%v until=%v",
			reason, disabledUntil)
	}
}

func TestPatchKeyUpdatesSecretWithoutEchoingPlaintext(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	_, h, tok := httpServer(t)
	base := "https://patch-key.example.invalid"
	wipeCRUD(ctx, t, conn, base)
	defer wipeCRUD(context.Background(), t, conn, base)

	channelID := newChannel(t, h, tok, "Key PATCH 靶子", base)
	accountID := newAccount(t, h, tok, channelID)
	keyID := newKey(t, h, tok, accountID, "sk-before-secret")

	code, body := do(t, h, tok, "PATCH", fmt.Sprintf("/admin/keys/%d", keyID),
		`{"secret":"sk-after-secret","external_ref":"ref-after",`+
			`"status":"active","rpm_limit":120,"concurrency_limit":3}`)
	if code != http.StatusOK {
		t.Fatalf("Key PATCH 应 200，得 %d：%s", code, body)
	}
	if strings.Contains(body, "sk-after-secret") {
		t.Fatalf("Key PATCH 响应回显了明文：%s", body)
	}

	var secret, ref, status string
	var rpm, conc int
	if err := conn.QueryRow(ctx, `
SELECT secret, COALESCE(external_ref,''), status, rpm_limit, concurrency_limit
  FROM upstream_keys WHERE id=$1`, keyID).Scan(&secret, &ref, &status, &rpm, &conc); err != nil {
		t.Fatalf("读 Key: %v", err)
	}
	if secret != "sk-after-secret" || ref != "ref-after" ||
		status != "active" || rpm != 120 || conc != 3 {
		t.Fatalf("Key PATCH 未按请求落库：secret=%q ref=%q status=%q rpm=%d conc=%d",
			secret, ref, status, rpm, conc)
	}
}

func TestPatchKeyRejectsGroupFromAnotherChannel(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	_, h, tok := httpServer(t)
	baseA := "https://key-group-owner.example.invalid"
	baseB := "https://key-group-foreign.example.invalid"
	wipeCRUD(ctx, t, conn, baseA)
	wipeCRUD(ctx, t, conn, baseB)
	defer wipeCRUD(context.Background(), t, conn, baseA)
	defer wipeCRUD(context.Background(), t, conn, baseB)

	channelA := newChannel(t, h, tok, "Key 分组归属 A", baseA)
	channelB := newChannel(t, h, tok, "Key 分组归属 B", baseB)
	accountID := newAccount(t, h, tok, channelA)
	keyID := newKey(t, h, tok, accountID, "sk-group-owner")
	rows := []store.GroupRow{{
		ChannelID: channelB, GroupRef: "foreign", DataSource: "manual",
		FetchedAt: time.Now(),
	}}
	if _, err := store.UpsertGroups(ctx, conn, rows); err != nil {
		t.Fatalf("造外部渠道分组: %v", err)
	}
	foreignGroupID, found, err := store.GroupIDByRef(ctx, conn, channelB, "foreign")
	if err != nil || !found {
		t.Fatalf("读取外部渠道分组 id: found=%v err=%v", found, err)
	}

	code, body := do(t, h, tok, "PATCH", fmt.Sprintf("/admin/keys/%d", keyID),
		fmt.Sprintf(`{"channel_group_id":%d}`, foreignGroupID))
	if code != http.StatusBadRequest {
		t.Fatalf("Key 不可关联其它渠道分组，应 400，得 %d：%s", code, body)
	}
	var got *int64
	if err := conn.QueryRow(ctx,
		`SELECT channel_group_id FROM upstream_keys WHERE id=$1`, keyID).Scan(&got); err != nil {
		t.Fatalf("读 Key 分组: %v", err)
	}
	if got != nil {
		t.Fatalf("跨渠道分组被写入了：got=%d foreign=%d", *got, foreignGroupID)
	}
}

func TestCreateKeyRejectsUnknownGroupRef(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	_, h, tok := httpServer(t)
	base := "https://create-key-unknown-group.example.invalid"
	wipeCRUD(ctx, t, conn, base)
	defer wipeCRUD(context.Background(), t, conn, base)

	channelID := newChannel(t, h, tok, "Key 未知分组靶子", base)
	accountID := newAccount(t, h, tok, channelID)

	code, body := do(t, h, tok, "POST", "/admin/keys",
		fmt.Sprintf(`{"account_id":%d,"secret":"sk-unknown-group","group_ref":"missing"}`,
			accountID))
	if code != http.StatusBadRequest {
		t.Fatalf("未知 group_ref 应 400，得 %d：%s", code, body)
	}
	var n int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM upstream_keys WHERE account_id=$1`, accountID).Scan(&n); err != nil {
		t.Fatalf("数 Key: %v", err)
	}
	if n != 0 {
		t.Fatalf("未知 group_ref 不应创建 Key，实际 %d 行", n)
	}
}

func TestDeleteKeyRequiresAndConsumesConfirmToken(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	_, h, tok := httpServer(t)
	base := "https://delete-key.example.invalid"
	wipeCRUD(ctx, t, conn, base)
	defer wipeCRUD(context.Background(), t, conn, base)

	channelID := newChannel(t, h, tok, "Key DELETE 靶子", base)
	accountID := newAccount(t, h, tok, channelID)
	keyID := newKey(t, h, tok, accountID, "sk-delete-secret")

	code, body := do(t, h, tok, "DELETE", fmt.Sprintf("/admin/keys/%d", keyID), `{}`)
	if code != http.StatusBadRequest {
		t.Fatalf("没有 confirm_token 的 DELETE 应 400，得 %d：%s", code, body)
	}

	code, body = do(t, h, tok, "POST", fmt.Sprintf("/admin/keys/%d/delete-preview", keyID), `{}`)
	if code != http.StatusOK {
		t.Fatalf("delete-preview 应 200，得 %d：%s", code, body)
	}
	var preview struct {
		ConfirmToken string `json:"confirm_token"`
	}
	if err := json.Unmarshal([]byte(body), &preview); err != nil {
		t.Fatalf("解析 delete-preview: %v（%s）", err, body)
	}
	if preview.ConfirmToken == "" {
		t.Fatalf("delete-preview 未返回 confirm_token：%s", body)
	}

	code, body = do(t, h, tok, "DELETE", fmt.Sprintf("/admin/keys/%d", keyID),
		fmt.Sprintf(`{"confirm_token":%q}`, preview.ConfirmToken))
	if code != http.StatusOK {
		t.Fatalf("带 confirm_token 的 DELETE 应 200，得 %d：%s", code, body)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM upstream_keys WHERE id=$1`, keyID).
		Scan(&n); err != nil {
		t.Fatalf("数 Key: %v", err)
	}
	if n != 0 {
		t.Fatalf("Key DELETE 后仍有 %d 行", n)
	}

	code, body = do(t, h, tok, "DELETE", fmt.Sprintf("/admin/keys/%d", keyID),
		fmt.Sprintf(`{"confirm_token":%q}`, preview.ConfirmToken))
	if code != http.StatusBadRequest {
		t.Fatalf("confirm_token 必须一次性消费，重放应 400，得 %d：%s", code, body)
	}
}

func TestDeleteKeyTokenExpiresWhenKeyChanges(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	_, h, tok := httpServer(t)
	base := "https://delete-token-version.example.invalid"
	wipeCRUD(ctx, t, conn, base)
	defer wipeCRUD(context.Background(), t, conn, base)

	channelID := newChannel(t, h, tok, "Key DELETE 版本靶子", base)
	accountID := newAccount(t, h, tok, channelID)
	keyID := newKey(t, h, tok, accountID, "sk-token-before")

	code, body := do(t, h, tok, "POST", fmt.Sprintf("/admin/keys/%d/delete-preview", keyID), `{}`)
	if code != http.StatusOK {
		t.Fatalf("delete-preview 应 200，得 %d：%s", code, body)
	}
	var preview struct {
		ConfirmToken string `json:"confirm_token"`
	}
	if err := json.Unmarshal([]byte(body), &preview); err != nil {
		t.Fatalf("解析 delete-preview: %v（%s）", err, body)
	}

	code, body = do(t, h, tok, "PATCH", fmt.Sprintf("/admin/keys/%d", keyID),
		`{"secret":"sk-token-after"}`)
	if code != http.StatusOK {
		t.Fatalf("修改 Key 应 200，得 %d：%s", code, body)
	}

	code, body = do(t, h, tok, "DELETE", fmt.Sprintf("/admin/keys/%d", keyID),
		fmt.Sprintf(`{"confirm_token":%q}`, preview.ConfirmToken))
	if code != http.StatusBadRequest {
		t.Fatalf("Key 已修改后旧 token 不可删除，应 400，得 %d：%s", code, body)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM upstream_keys WHERE id=$1`, keyID).
		Scan(&n); err != nil {
		t.Fatalf("数 Key: %v", err)
	}
	if n != 1 {
		t.Fatalf("旧 token 删除后 Key 行数=%d，应仍为 1", n)
	}
}

func TestDeleteKeyDoesNotDeleteConcurrentNewVersion(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	_, h, tok := httpServer(t)
	base := "https://delete-token-race.example.invalid"
	wipeCRUD(ctx, t, conn, base)
	defer wipeCRUD(context.Background(), t, conn, base)

	channelID := newChannel(t, h, tok, "Key DELETE 竞争靶子", base)
	accountID := newAccount(t, h, tok, channelID)
	keyID := newKey(t, h, tok, accountID, "sk-race-before")

	code, body := do(t, h, tok, "POST", fmt.Sprintf("/admin/keys/%d/delete-preview", keyID), `{}`)
	if code != http.StatusOK {
		t.Fatalf("delete-preview 应 200，得 %d：%s", code, body)
	}
	var preview struct {
		ConfirmToken string `json:"confirm_token"`
	}
	if err := json.Unmarshal([]byte(body), &preview); err != nil {
		t.Fatalf("解析 delete-preview: %v（%s）", err, body)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("开竞争事务: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `
UPDATE upstream_keys SET external_ref='raced', updated_at=now() WHERE id=$1`, keyID); err != nil {
		t.Fatalf("竞争更新 Key: %v", err)
	}

	type result struct {
		code int
		body string
	}
	done := make(chan result, 1)
	go func() {
		code, body := do(t, h, tok, "DELETE", fmt.Sprintf("/admin/keys/%d", keyID),
			fmt.Sprintf(`{"confirm_token":%q}`, preview.ConfirmToken))
		done <- result{code: code, body: body}
	}()

	time.Sleep(150 * time.Millisecond)
	select {
	case got := <-done:
		t.Fatalf("DELETE 没等待行锁就返回了：%d %s", got.code, got.body)
	default:
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交竞争更新: %v", err)
	}

	var got result
	select {
	case got = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("DELETE 等待竞争更新后未返回")
	}
	if got.code != http.StatusConflict {
		t.Fatalf("旧 token 不可删除并发更新后的 Key，应 409，得 %d：%s", got.code, got.body)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM upstream_keys WHERE id=$1`, keyID).
		Scan(&n); err != nil {
		t.Fatalf("数 Key: %v", err)
	}
	if n != 1 {
		t.Fatalf("并发更新后旧 token 仍删除了 Key，行数=%d，应为 1", n)
	}
}

func TestExpiredDisabledUntilAllowsSync(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	s, h, tok := httpServer(t)
	base := "https://expired-disabled-sync.example.invalid"
	wipeCRUD(ctx, t, conn, base)
	defer wipeCRUD(context.Background(), t, conn, base)

	channelID := newChannel(t, h, tok, "过期停用 sync 靶子", base)
	past := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	code, body := do(t, h, tok, "PATCH", fmt.Sprintf("/admin/channels/%d", channelID),
		fmt.Sprintf(`{"status":"disabled","disabled_reason":"维护已结束","disabled_until":%q}`,
			past.Format(time.RFC3339)))
	if code != http.StatusOK {
		t.Fatalf("停用渠道应 200，得 %d：%s", code, body)
	}

	called := false
	s.Sync = func(_ context.Context, _ store.Channel) (*collector.SyncResult, error) {
		called = true
		return &collector.SyncResult{ChannelID: channelID}, nil
	}

	code, body = do(t, h, tok, "POST", fmt.Sprintf("/admin/channels/%d/sync", channelID), `{}`)
	if code != http.StatusOK {
		t.Fatalf("disabled_until 已过期后 sync 应 200，得 %d：%s", code, body)
	}
	if !called {
		t.Fatal("disabled_until 已过期后应真正进入 Sync")
	}
}
