package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

type unavailableDB struct{}

func (unavailableDB) Acquire(context.Context) (*pgx.Conn, func(), error) {
	return nil, nil, errors.New("database unavailable")
}

func unavailableUpstreamHandler() http.Handler {
	s := NewServer(unavailableDB{}, "test-admin-token", nil, nil)
	mux := http.NewServeMux()
	s.UpstreamRoutes(mux)
	return mux
}

func TestKeyAutomationRequiresExplicitScope(t *testing.T) {
	h := unavailableUpstreamHandler()
	for _, path := range []string{"/admin/keys/import", "/admin/keys/provision?dry_run=true"} {
		code, body := do(t, h, "test-admin-token", http.MethodPost, path, `{}`)
		if code != http.StatusBadRequest || !strings.Contains(body, "channel_id") {
			t.Fatalf("POST %s = %d %s，期望缺少明确范围时返回 400", path, code, body)
		}
	}
}

func TestKeyAutomationRejectsMalformedJSON(t *testing.T) {
	code, body := do(t, unavailableUpstreamHandler(), "test-admin-token", http.MethodPost,
		"/admin/keys/import", `{"account_id":`)
	if code != http.StatusBadRequest || !strings.Contains(body, "请求体解析失败") {
		t.Fatalf("畸形 JSON = %d %s，期望返回解析错误", code, body)
	}
}

func TestProvisionKeysRequiresExplicitDryRun(t *testing.T) {
	h := unavailableUpstreamHandler()
	for _, path := range []string{"/admin/keys/provision", "/admin/keys/provision?dry_run=maybe"} {
		code, body := do(t, h, "test-admin-token", http.MethodPost, path, `{"all":true}`)
		if code != http.StatusBadRequest || !strings.Contains(body, "dry_run") {
			t.Fatalf("POST %s = %d %s，期望拒绝不明确的执行模式", path, code, body)
		}
	}
}

func TestProvisionKeysExpandsChannelToEveryAccount(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()

	s, h, token := httpServer(t)
	base := "https://provision-channel-accounts.example.invalid"
	wipeCRUD(ctx, t, conn, base)
	defer wipeCRUD(context.Background(), t, conn, base)
	channelID := newChannel(t, h, token, "批量补齐渠道", base)
	first := newAccount(t, h, token, channelID)
	second := newAccount(t, h, token, channelID)

	var called []int64
	s.ProvisionKeys = func(
		_ context.Context, _ *pgx.Conn, gotChannelID, accountID int64,
		request collector.KeyProvisionRequest,
	) (collector.KeyProvisionResult, error) {
		if gotChannelID != channelID || !request.DryRun || request.Model != "gpt-5.5" {
			t.Fatalf("补齐参数错误：channel=%d request=%+v", gotChannelID, request)
		}
		called = append(called, accountID)
		return collector.KeyProvisionResult{MatchedGroups: 1, WouldCreate: 1}, nil
	}
	code, body := do(t, h, token, http.MethodPost,
		"/admin/keys/provision?dry_run=true",
		fmt.Sprintf(`{"channel_id":%d,"model":"gpt-5.5"}`, channelID))
	if code != http.StatusOK {
		t.Fatalf("批量补齐预览 = %d %s", code, body)
	}
	sort.Slice(called, func(i, j int) bool { return called[i] < called[j] })
	want := []int64{first, second}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if !slices.Equal(called, want) {
		t.Fatalf("调用账号 = %v，期望 %v", called, want)
	}
	var result keyProvisionBatchResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	if result.Count != 2 || result.MatchedGroups != 2 || result.WouldCreate != 2 {
		t.Fatalf("批量结果 = %+v", result)
	}
}

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

func TestPatchAccountCanClearEditableFields(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	_, h, tok := httpServer(t)
	base := "https://patch-account-clear.example.invalid"
	wipeCRUD(ctx, t, conn, base)
	defer wipeCRUD(context.Background(), t, conn, base)

	channelID := newChannel(t, h, tok, "清空账号字段靶子", base)
	accountID := newAccount(t, h, tok, channelID)
	code, body := do(t, h, tok, "PATCH", fmt.Sprintf("/admin/accounts/%d", accountID),
		`{"external_user_id":"","balance_group_key":""}`)
	if code != http.StatusOK {
		t.Fatalf("清空账号字段应 200，得 %d：%s", code, body)
	}
	var external, balance *string
	if err := conn.QueryRow(ctx, `
SELECT external_user_id, balance_group_key FROM upstream_accounts WHERE id=$1`, accountID).
		Scan(&external, &balance); err != nil {
		t.Fatalf("读清空后的账号: %v", err)
	}
	if external != nil || balance != nil {
		t.Fatalf("账号字段未清空：external=%v balance=%v", external, balance)
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

func TestListKeysIncludesGroupAndMultiplier(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	_, h, tok := httpServer(t)
	base := "https://list-key-group.example.invalid"
	wipeCRUD(ctx, t, conn, base)
	defer wipeCRUD(context.Background(), t, conn, base)

	channelID := newChannel(t, h, tok, "Key 分组列表靶子", base)
	accountID := newAccount(t, h, tok, channelID)
	if _, err := store.UpsertGroups(ctx, conn, []store.GroupRow{{
		ChannelID: channelID, GroupRef: "vip", RateMultiplier: floatPtr(0.5),
		DataSource: "manual", FetchedAt: time.Now(),
	}}); err != nil {
		t.Fatalf("建分组: %v", err)
	}
	keyID := newKey(t, h, tok, accountID, "sk-list-key-group")
	groupID, found, err := store.GroupIDByRef(ctx, conn, channelID, "vip")
	if err != nil || !found {
		t.Fatalf("查分组: found=%v err=%v", found, err)
	}
	code, body := do(t, h, tok, "PATCH", fmt.Sprintf("/admin/keys/%d", keyID),
		fmt.Sprintf(`{"channel_group_id":%d}`, groupID))
	if code != http.StatusOK {
		t.Fatalf("关联分组应 200，得 %d：%s", code, body)
	}

	code, body = do(t, h, tok, "GET", fmt.Sprintf("/admin/keys?channel_id=%d", channelID), "")
	if code != http.StatusOK {
		t.Fatalf("列 Key 应 200，得 %d：%s", code, body)
	}
	var out struct {
		Items []struct {
			GroupRef       string   `json:"group_ref"`
			RateMultiplier *float64 `json:"rate_multiplier"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析 Key 列表: %v（%s）", err, body)
	}
	if len(out.Items) != 1 || out.Items[0].GroupRef != "vip" ||
		out.Items[0].RateMultiplier == nil || *out.Items[0].RateMultiplier != 0.5 {
		t.Fatalf("Key 列表缺少分组/倍率：%s", body)
	}
}

func TestPatchKeyCanClearGroup(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	_, h, tok := httpServer(t)
	base := "https://patch-key-clear-group.example.invalid"
	wipeCRUD(ctx, t, conn, base)
	defer wipeCRUD(context.Background(), t, conn, base)

	channelID := newChannel(t, h, tok, "清空 Key 分组靶子", base)
	accountID := newAccount(t, h, tok, channelID)
	if _, err := store.UpsertGroups(ctx, conn, []store.GroupRow{{
		ChannelID: channelID, GroupRef: "vip", DataSource: "manual", FetchedAt: time.Now(),
	}}); err != nil {
		t.Fatalf("建分组: %v", err)
	}
	groupID, found, err := store.GroupIDByRef(ctx, conn, channelID, "vip")
	if err != nil || !found {
		t.Fatalf("查分组: found=%v err=%v", found, err)
	}
	keyID := newKey(t, h, tok, accountID, "sk-clear-group")
	code, body := do(t, h, tok, "PATCH", fmt.Sprintf("/admin/keys/%d", keyID),
		fmt.Sprintf(`{"channel_group_id":%d}`, groupID))
	if code != http.StatusOK {
		t.Fatalf("关联分组应 200，得 %d：%s", code, body)
	}
	code, body = do(t, h, tok, "PATCH", fmt.Sprintf("/admin/keys/%d", keyID),
		`{"channel_group_id":null}`)
	if code != http.StatusOK {
		t.Fatalf("清空 Key 分组应 200，得 %d：%s", code, body)
	}
	var got *int64
	if err := conn.QueryRow(ctx,
		`SELECT channel_group_id FROM upstream_keys WHERE id=$1`, keyID).Scan(&got); err != nil {
		t.Fatalf("读清空后的 Key 分组: %v", err)
	}
	if got != nil {
		t.Fatalf("Key 分组未清空：%d", *got)
	}
}

func floatPtr(v float64) *float64 { return &v }

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

func TestDeleteKeyDoesNotRequirePreviewToken(t *testing.T) {
	code, body := do(t, unavailableUpstreamHandler(), "test-admin-token",
		"DELETE", "/admin/keys/123", `{}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("DELETE 应直接进入数据库操作，期望数据库不可用的 503，得 %d：%s", code, body)
	}
}

func TestDeleteKeyNeedsNoPreviewTokenIntegration(t *testing.T) {
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
	if code != http.StatusOK {
		t.Fatalf("ADMIN_TOKEN 已鉴权的 DELETE 应直接成功，得 %d：%s", code, body)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM upstream_keys WHERE id=$1`, keyID).
		Scan(&n); err != nil {
		t.Fatalf("数 Key: %v", err)
	}
	if n != 0 {
		t.Fatalf("Key DELETE 后仍有 %d 行", n)
	}
}

func TestRotateRouteIsNotRegistered(t *testing.T) {
	code, body := do(t, unavailableUpstreamHandler(), "test-admin-token",
		"POST", "/admin/keys/123/rotate", `{}`)
	if code != http.StatusNotFound {
		t.Fatalf("P1 不应注册 rotate 路由，期望 404，得 %d：%s", code, body)
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
