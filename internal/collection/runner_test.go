package collection

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

type fixedKeyResolver struct{}

func (fixedKeyResolver) ResolveKeySecret(context.Context, collector.Session, string) (string, error) {
	return "resolved-secret", nil
}

func TestRunnerUnknownFamilyFailsBeforeCredentialLoad(t *testing.T) {
	loaded := false
	r := &Runner{
		Client: collector.NewClient(0),
		LoadCredentials: func(context.Context, store.Channel) ([]collector.Credential, error) {
			loaded = true
			return nil, nil
		},
	}
	_, err := r.Sync(context.Background(), store.Channel{
		ID: 1, SiteFamily: string(collector.FamilyUnknown),
	}, nil)
	if !errors.Is(err, collector.ErrPrecondition) {
		t.Fatalf("未知站型应返回 ErrPrecondition，得到 %v", err)
	}
	if loaded {
		t.Fatal("未知站型不应读取凭证")
	}
}

func TestSelectProvisionGroupsFiltersModelAndExistingCoverage(t *testing.T) {
	groups := []collector.Group{
		{GroupRef: "default", AvailableModels: []string{"gpt-5.5"}},
		{GroupRef: "vip", AvailableModels: []string{"gpt-5.5", "claude-sonnet"}},
		{GroupRef: "legacy", AvailableModels: []string{"gpt-4"}},
	}
	keys := []collector.Key{{KeyRef: "1", GroupRef: "default"}}

	selected, skipped := selectProvisionGroups(keys, groups, "gpt-5.5", false)
	if skipped != "" {
		t.Fatalf("不应跳过账号：%s", skipped)
	}
	if len(selected) != 1 || selected[0].GroupRef != "vip" {
		t.Fatalf("待创建分组 = %+v，期望仅 vip", selected)
	}
}

func TestSelectProvisionGroupsSkipsAccountWithAnyRemoteKey(t *testing.T) {
	selected, skipped := selectProvisionGroups(
		[]collector.Key{{KeyRef: "1", GroupRef: "default"}},
		[]collector.Group{{GroupRef: "vip"}}, "", true,
	)
	if len(selected) != 0 || skipped == "" {
		t.Fatalf("待创建分组 = %+v，跳过原因 = %q", selected, skipped)
	}
}

func TestProvisionRemoteKeysCreatesOnlyMissingModelGroup(t *testing.T) {
	created := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/token":
			items := []map[string]any{{"id": 1, "group": "default", "remain_quota": 10}}
			if created {
				items = append(items, map[string]any{"id": 2, "group": "vip", "remain_quota": 10})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"page": 1, "page_size": 100, "total": len(items), "items": items,
			}})
		case "GET /api/pricing":
			_, _ = w.Write([]byte(`{"group_ratio":{"default":1,"vip":0.8},"data":[
				{"model_name":"gpt-5.5","quota_type":0,"model_ratio":1,
				 "enable_groups":["default","vip"]}]}`))
		case "POST /api/token/":
			created = true
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := collector.NewClient(0)
	client.HC = srv.Client()
	adapter := collector.NewNewAPIAdapter(client)
	var imported []collector.Key
	result, err := provisionRemoteKeys(
		context.Background(), adapter,
		collector.Session{BaseURL: srv.URL, Token: "session", QuotaPerUnit: 1},
		&collector.KeyProvisionRequest{Model: "gpt-5.5"},
		nil,
		func(key collector.Key) error {
			imported = append(imported, key)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Found != 1 || result.MatchedGroups != 2 || result.ExistingGroups != 1 ||
		result.WouldCreate != 1 || result.Created != 1 || result.Failed != 0 {
		t.Fatalf("批量创建结果 = %+v", result)
	}
	if len(imported) != 1 || imported[0].KeyRef != "2" || imported[0].GroupRef != "vip" {
		t.Fatalf("登记回调 = %+v，期望新建 vip Key", imported)
	}
}

func TestProvisionRemoteKeysStopsAfterRegistrationFailure(t *testing.T) {
	var createdGroups []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/token":
			items := make([]map[string]any, 0, len(createdGroups))
			for index, group := range createdGroups {
				items = append(items, map[string]any{"id": index + 1, "group": group, "remain_quota": 10})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"page": 1, "page_size": 100, "total": len(items), "items": items,
			}})
		case "GET /api/pricing":
			_, _ = w.Write([]byte(`{"group_ratio":{"default":1,"vip":0.8},"data":[
				{"model_name":"gpt-5.5","quota_type":0,"model_ratio":1,
				 "enable_groups":["default","vip"]}]}`))
		case "POST /api/token/":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			createdGroups = append(createdGroups, request["group"].(string))
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := collector.NewClient(0)
	client.HC = srv.Client()
	adapter := collector.NewNewAPIAdapter(client)
	result, err := provisionRemoteKeys(
		context.Background(), adapter,
		collector.Session{BaseURL: srv.URL, Token: "session", QuotaPerUnit: 1},
		&collector.KeyProvisionRequest{Model: "gpt-5.5"},
		nil,
		func(collector.Key) error { return errors.New("本地登记失败") },
	)
	if err == nil {
		t.Fatal("本地登记失败必须中止补齐")
	}
	if result.Created != 0 || result.Failed != 1 {
		t.Fatalf("补齐结果 = %+v，期望 created=0/failed=1", result)
	}
	if len(createdGroups) != 1 {
		t.Fatalf("远端创建次数 = %d，登记失败后不应继续下一分组", len(createdGroups))
	}
}

func TestProvisionRemoteKeysSkipsExistingKeyBeforeImport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"page": 1, "page_size": 100, "total": 1,
				"items": []map[string]any{{"id": 1, "group": "default", "remain_quota": 10}},
			}})
		case "GET /api/pricing":
			_, _ = w.Write([]byte(`{"group_ratio":{"default":1},"data":[
				{"model_name":"gpt-5.5","quota_type":0,"model_ratio":1,"enable_groups":["default"]}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := collector.NewClient(0)
	client.HC = srv.Client()
	adapter := collector.NewNewAPIAdapter(client)
	importCalled := false
	result, err := provisionRemoteKeys(
		context.Background(), adapter,
		collector.Session{BaseURL: srv.URL, Token: "session", QuotaPerUnit: 1},
		&collector.KeyProvisionRequest{OnlyWithoutKeys: true},
		func([]collector.Key, []collector.Group) error {
			importCalled = true
			return nil
		},
		func(collector.Key) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.SkippedReason == "" || importCalled {
		t.Fatalf("已有远端 Key 时应直接跳过且不读取明文：result=%+v import=%v", result, importCalled)
	}
}

func TestProvisionRemoteKeysRespectsCreateLimit(t *testing.T) {
	var createdGroups []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/token":
			items := make([]map[string]any, 0, len(createdGroups))
			for index, group := range createdGroups {
				items = append(items, map[string]any{"id": index + 1, "group": group, "remain_quota": 10})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"page": 1, "page_size": 100, "total": len(items), "items": items,
			}})
		case "GET /api/pricing":
			_, _ = w.Write([]byte(`{"group_ratio":{"default":1,"vip":0.8},"data":[
				{"model_name":"gpt-5.5","quota_type":0,"model_ratio":1,
				 "enable_groups":["default","vip"]}]}`))
		case "POST /api/token/":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			createdGroups = append(createdGroups, request["group"].(string))
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := collector.NewClient(0)
	client.HC = srv.Client()
	adapter := collector.NewNewAPIAdapter(client)
	result, err := provisionRemoteKeys(
		context.Background(), adapter,
		collector.Session{BaseURL: srv.URL, Token: "session", QuotaPerUnit: 1},
		&collector.KeyProvisionRequest{Model: "gpt-5.5", MaxCreates: 1},
		nil,
		func(collector.Key) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 || result.Deferred != 1 || len(createdGroups) != 1 {
		t.Fatalf("补齐结果 = %+v，远端创建=%v，期望仅创建一把并报告一把待处理", result, createdGroups)
	}
}

func TestProvisionRemoteKeysUsesLimitReducedDuringDiscovery(t *testing.T) {
	var createdGroups []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/token":
			items := make([]map[string]any, 0, len(createdGroups))
			for index, group := range createdGroups {
				items = append(items, map[string]any{"id": index + 1, "group": group, "remain_quota": 10})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"page": 1, "page_size": 100, "total": len(items), "items": items,
			}})
		case "GET /api/pricing":
			_, _ = w.Write([]byte(`{"group_ratio":{"default":1,"vip":0.8},"data":[
				{"model_name":"gpt-5.5","quota_type":0,"model_ratio":1,
				 "enable_groups":["default","vip"]}]}`))
		case "POST /api/token/":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			createdGroups = append(createdGroups, request["group"].(string))
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := collector.NewClient(0)
	client.HC = srv.Client()
	adapter := collector.NewNewAPIAdapter(client)
	request := &collector.KeyProvisionRequest{Model: "gpt-5.5", MaxCreates: 2}
	result, err := provisionRemoteKeys(
		context.Background(), adapter,
		collector.Session{BaseURL: srv.URL, Token: "session", QuotaPerUnit: 1},
		request,
		func([]collector.Key, []collector.Group) error {
			request.MaxCreates = 1
			return nil
		},
		func(collector.Key) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 || result.Deferred != 1 || len(createdGroups) != 1 {
		t.Fatalf("发现阶段收缩上限后仍创建过多 Key：result=%+v created=%v", result, createdGroups)
	}
}

func TestImportKeysSkipsExistingExternalRefAndCreatesNewKey(t *testing.T) {
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	base := "https://runner-import-keys.example.invalid"
	_, _ = conn.Exec(ctx, `DELETE FROM upstream_keys WHERE account_id IN
        (SELECT id FROM upstream_accounts WHERE channel_id IN
        (SELECT id FROM channels WHERE base_url=$1))`, base)
	_, _ = conn.Exec(ctx, `DELETE FROM upstream_accounts WHERE channel_id IN
        (SELECT id FROM channels WHERE base_url=$1)`, base)
	_, _ = conn.Exec(ctx, `DELETE FROM channels WHERE base_url=$1`, base)

	var channelID int64
	if err := conn.QueryRow(ctx, `
INSERT INTO channels (name, site_family, base_url, status)
VALUES ('runner-import-keys','newapi',$1,'enabled') RETURNING id`, base).Scan(&channelID); err != nil {
		t.Fatalf("创建测试渠道: %v", err)
	}
	defer func() {
		_, _ = conn.Exec(ctx, `DELETE FROM upstream_keys WHERE account_id IN
            (SELECT id FROM upstream_accounts WHERE channel_id=$1)`, channelID)
		_, _ = conn.Exec(ctx, `DELETE FROM upstream_accounts WHERE channel_id=$1`, channelID)
		_, _ = conn.Exec(ctx, `DELETE FROM channels WHERE id=$1`, channelID)
	}()

	accountID, err := store.CreateAccount(ctx, conn, store.Account{ChannelID: channelID, ExternalUserID: "42"})
	if err != nil {
		t.Fatalf("创建测试账号: %v", err)
	}
	if _, err := store.CreateKey(ctx, conn, accountID, "existing-secret", "101", nil); err != nil {
		t.Fatalf("创建已有 Key: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/user/self":
			_, _ = w.Write([]byte(`{"data":{"id":42,"quota":100,"used_quota":1}}`))
		case "GET /api/token":
			_, _ = w.Write([]byte(`{"data":[
                {"id":101,"remain_quota":90,"used_quota":10,"expired_time":-1},
                {"id":102,"remain_quota":80,"used_quota":20,"expired_time":-1}
            ]}`))
		case "POST /api/token/102/key":
			_, _ = w.Write([]byte(`{"data":{"key":"new-secret"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := collector.NewClient(0)
	client.HC = server.Client()
	runner := &Runner{Client: client}
	result, err := runner.ImportKeys(ctx, conn, store.Channel{
		ID: channelID, SiteFamily: string(collector.FamilyNewAPI), BaseURL: server.URL,
	}, []collector.Credential{{
		AccountID: accountID, ChannelID: channelID, Family: collector.FamilyNewAPI,
		BaseURL: server.URL, AccessToken: "session-token", ExternalUserID: "42",
		UserIDHeaderName: "New-API-User", QuotaPerUnit: 1,
	}}, collector.KeyImportRequest{})
	if err != nil {
		t.Fatalf("导入 Key: %v", err)
	}
	if result.Found != 2 || result.Skipped != 1 || result.Imported != 1 || result.Failed != 0 {
		t.Fatalf("Key 导入结果 = %+v，期望 found=2/skipped=1/imported=1/failed=0", result)
	}
	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM upstream_keys WHERE account_id=$1`, accountID).Scan(&count); err != nil {
		t.Fatalf("统计 Key: %v", err)
	}
	if count != 2 {
		t.Fatalf("Key 行数 = %d，期望 2", count)
	}
}

func TestImportFetchedKeysReusesInsertedKeyIDForDuplicateRef(t *testing.T) {
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()

	base := "https://runner-duplicate-key-ref.example.invalid"
	_, _ = conn.Exec(ctx, `DELETE FROM upstream_keys WHERE account_id IN
        (SELECT id FROM upstream_accounts WHERE channel_id IN
        (SELECT id FROM channels WHERE base_url=$1))`, base)
	_, _ = conn.Exec(ctx, `DELETE FROM upstream_accounts WHERE channel_id IN
        (SELECT id FROM channels WHERE base_url=$1)`, base)
	_, _ = conn.Exec(ctx, `DELETE FROM channels WHERE base_url=$1`, base)

	var channelID int64
	if err := conn.QueryRow(ctx, `
INSERT INTO channels (name, site_family, base_url, status)
VALUES ('runner-duplicate-key-ref','newapi',$1,'enabled') RETURNING id`, base).Scan(&channelID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = conn.Exec(ctx, `DELETE FROM upstream_keys WHERE account_id IN
            (SELECT id FROM upstream_accounts WHERE channel_id=$1)`, channelID)
		_, _ = conn.Exec(ctx, `DELETE FROM upstream_accounts WHERE channel_id=$1`, channelID)
		_, _ = conn.Exec(ctx, `DELETE FROM channels WHERE id=$1`, channelID)
	}()
	accountID, err := store.CreateAccount(ctx, conn, store.Account{ChannelID: channelID})
	if err != nil {
		t.Fatal(err)
	}

	runner := &Runner{}
	secretBudget := collector.DefaultKeySecretResolveLimit
	result, err := runner.importFetchedKeys(ctx, conn, store.Channel{ID: channelID}, collector.Credential{
		AccountID: accountID,
	}, collector.Session{}, fixedKeyResolver{}, []collector.Key{
		{KeyRef: "same-ref"}, {KeyRef: "same-ref"},
	}, &secretBudget)
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 1 || result.Skipped != 1 || result.Failed != 0 {
		t.Fatalf("导入结果 = %+v，期望 imported=1/skipped=1/failed=0", result)
	}
}

func TestProvisionKeysCreatesRemoteGroupKeyAndRegistersIt(t *testing.T) {
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	base := "https://runner-provision-keys.example.invalid"
	_, _ = conn.Exec(ctx, `DELETE FROM upstream_keys WHERE account_id IN
        (SELECT id FROM upstream_accounts WHERE channel_id IN
        (SELECT id FROM channels WHERE base_url=$1))`, base)
	_, _ = conn.Exec(ctx, `DELETE FROM upstream_accounts WHERE channel_id IN
        (SELECT id FROM channels WHERE base_url=$1)`, base)
	_, _ = conn.Exec(ctx, `DELETE FROM channels WHERE base_url=$1`, base)

	var channelID int64
	if err := conn.QueryRow(ctx, `
INSERT INTO channels (name, site_family, base_url, status)
VALUES ('runner-provision-keys','newapi',$1,'enabled') RETURNING id`, base).Scan(&channelID); err != nil {
		t.Fatalf("创建测试渠道: %v", err)
	}
	defer func() {
		_, _ = conn.Exec(ctx, `DELETE FROM upstream_keys WHERE account_id IN
            (SELECT id FROM upstream_accounts WHERE channel_id=$1)`, channelID)
		_, _ = conn.Exec(ctx, `DELETE FROM upstream_accounts WHERE channel_id=$1`, channelID)
		_, _ = conn.Exec(ctx, `DELETE FROM channels WHERE id=$1`, channelID)
	}()
	accountID, err := store.CreateAccount(ctx, conn, store.Account{ChannelID: channelID, ExternalUserID: "42"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
INSERT INTO channel_groups(channel_id, group_ref, rate_multiplier, data_source, fetched_at)
VALUES ($1,'vip',0.8,'auto_collect',now())`, channelID); err != nil {
		t.Fatal(err)
	}

	created := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/user/self":
			_, _ = w.Write([]byte(`{"data":{"id":42,"quota":100,"used_quota":1}}`))
		case "GET /api/token":
			items := []map[string]any{}
			if created {
				items = append(items, map[string]any{"id": 501, "group": "vip", "remain_quota": 100})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
		case "GET /api/pricing":
			_, _ = w.Write([]byte(`{"group_ratio":{"vip":0.8},"data":[
				{"model_name":"gpt-5.5","quota_type":0,"model_ratio":1,"enable_groups":["vip"]}]}`))
		case "POST /api/token/":
			created = true
			_, _ = w.Write([]byte(`{"success":true}`))
		case "POST /api/token/501/key":
			_, _ = w.Write([]byte(`{"data":{"key":"created-secret"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := collector.NewClient(0)
	client.HC = server.Client()
	runner := &Runner{Client: client}
	result, err := runner.ProvisionKeys(ctx, conn, store.Channel{
		ID: channelID, SiteFamily: string(collector.FamilyNewAPI), BaseURL: server.URL,
	}, collector.Credential{
		AccountID: accountID, ChannelID: channelID, Family: collector.FamilyNewAPI,
		BaseURL: server.URL, AccessToken: "session", ExternalUserID: "42",
		UserIDHeaderName: "New-API-User", QuotaPerUnit: 1,
	}, collector.KeyProvisionRequest{Model: "gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 || result.Imported != 1 || result.Failed != 0 {
		t.Fatalf("补齐结果 = %+v", result)
	}
	var secret, externalRef, groupRef string
	if err := conn.QueryRow(ctx, `
SELECT k.secret, k.external_ref, g.group_ref
  FROM upstream_keys k JOIN channel_groups g ON g.id=k.channel_group_id
 WHERE k.account_id=$1`, accountID).Scan(&secret, &externalRef, &groupRef); err != nil {
		t.Fatal(err)
	}
	if secret != "created-secret" || externalRef != "501" || groupRef != "vip" {
		t.Fatalf("登记结果 = secret:%q ref:%q group:%q", secret, externalRef, groupRef)
	}
}

func TestProvisionKeysDoesNotCreateAfterSecretBudgetIsConsumed(t *testing.T) {
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()

	base := "https://runner-provision-secret-budget.example.invalid"
	_, _ = conn.Exec(ctx, `DELETE FROM upstream_keys WHERE account_id IN
        (SELECT id FROM upstream_accounts WHERE channel_id IN
        (SELECT id FROM channels WHERE base_url=$1))`, base)
	_, _ = conn.Exec(ctx, `DELETE FROM upstream_accounts WHERE channel_id IN
        (SELECT id FROM channels WHERE base_url=$1)`, base)
	_, _ = conn.Exec(ctx, `DELETE FROM channels WHERE base_url=$1`, base)

	var channelID int64
	if err := conn.QueryRow(ctx, `
INSERT INTO channels (name, site_family, base_url, status)
VALUES ('runner-provision-secret-budget','newapi',$1,'enabled') RETURNING id`, base).Scan(&channelID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = conn.Exec(ctx, `DELETE FROM upstream_keys WHERE account_id IN
            (SELECT id FROM upstream_accounts WHERE channel_id=$1)`, channelID)
		_, _ = conn.Exec(ctx, `DELETE FROM upstream_accounts WHERE channel_id=$1`, channelID)
		_, _ = conn.Exec(ctx, `DELETE FROM channels WHERE id=$1`, channelID)
	}()
	accountID, err := store.CreateAccount(ctx, conn, store.Account{ChannelID: channelID, ExternalUserID: "42"})
	if err != nil {
		t.Fatal(err)
	}

	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/user/self":
			_, _ = w.Write([]byte(`{"data":{"id":42,"quota":100,"used_quota":1}}`))
		case "GET /api/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{
				"id": 1, "group": "default", "remain_quota": 100,
			}}})
		case "GET /api/pricing":
			_, _ = w.Write([]byte(`{"group_ratio":{"default":1,"vip":0.8},"data":[
				{"model_name":"gpt-5.5","quota_type":0,"model_ratio":1,
				 "enable_groups":["default","vip"]}]}`))
		case "POST /api/token/1/key":
			_, _ = w.Write([]byte(`{"data":{"key":"existing-secret"}}`))
		case "POST /api/token/":
			creates++
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := collector.NewClient(0)
	client.HC = server.Client()
	runner := &Runner{Client: client}
	result, err := runner.ProvisionKeys(ctx, conn, store.Channel{
		ID: channelID, SiteFamily: string(collector.FamilyNewAPI), BaseURL: server.URL,
	}, collector.Credential{
		AccountID: accountID, ChannelID: channelID, Family: collector.FamilyNewAPI,
		BaseURL: server.URL, AccessToken: "session", ExternalUserID: "42",
		UserIDHeaderName: "New-API-User", QuotaPerUnit: 1,
	}, collector.KeyProvisionRequest{Model: "gpt-5.5", MaxSecretResolves: 1})
	if err == nil {
		t.Fatal("明文预算耗尽时必须拒绝继续创建")
	}
	if creates != 0 || result.Created != 0 || result.Imported != 1 {
		t.Fatalf("预算耗尽后不应创建远端 Key：result=%+v creates=%d", result, creates)
	}
}

type runnerSavedItem struct {
	accountID int64
	ref       string
}

type runnerSink struct {
	accounts []runnerSavedItem
	keys     []runnerSavedItem
	groups   [][]collector.Group
	pricing  []collector.Pricing
	catalogs [][]collector.CatalogModel
}

func (s *runnerSink) SaveAccount(
	_ context.Context, _, accountID int64, account collector.Account,
) error {
	s.accounts = append(s.accounts, runnerSavedItem{accountID: accountID, ref: account.UserID})
	return nil
}

func (s *runnerSink) SaveGroups(
	_ context.Context, _ int64, groups []collector.Group,
) (int, error) {
	s.groups = append(s.groups, groups)
	return len(groups), nil
}

func (s *runnerSink) SaveKey(
	_ context.Context, _, accountID int64, key collector.Key,
) error {
	s.keys = append(s.keys, runnerSavedItem{accountID: accountID, ref: key.KeyRef})
	return nil
}

func (s *runnerSink) SavePricing(
	_ context.Context, _ int64, pricing collector.Pricing,
) (collector.PricingWriteResult, error) {
	s.pricing = append(s.pricing, pricing)
	return collector.PricingWriteResult{Snapshots: len(pricing.Models)}, nil
}

func (s *runnerSink) SaveCatalog(
	_ context.Context, _ int64, catalog []collector.CatalogModel,
) (int, error) {
	s.catalogs = append(s.catalogs, catalog)
	return len(catalog), nil
}

func TestRunnerCollectsEveryAccountAndMergesChannelData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		userID := "101"
		group := "account-a"
		model := "model-a"
		if token == "Bearer token-b" {
			userID, group, model = "202", "account-b", "model-b"
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/user/self":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"id": userID, "quota": 100, "used_quota": 10},
			})
		case "/api/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{
					"id": "key-" + userID, "group": group,
					"remain_quota": 50, "used_quota": 10, "expired_time": -1,
				}},
			})
		case "/api/pricing":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success":     true,
				"group_ratio": map[string]any{"shared": 1, group: 0.5},
				"data": []map[string]any{
					{"model_name": "shared-model", "quota_type": 0, "model_ratio": 1,
						"enable_groups": []string{"shared"}},
					{"model_name": model, "quota_type": 0, "model_ratio": 2,
						"enable_groups": []string{group}},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := collector.NewClient(0)
	client.HC = server.Client()
	sink := &runnerSink{}
	runner := &Runner{
		Client: client,
		Sink:   sink,
		LoadCredentials: func(context.Context, store.Channel) ([]collector.Credential, error) {
			return []collector.Credential{
				{AccountID: 11, ChannelID: 7, Family: collector.FamilyNewAPI,
					BaseURL: server.URL, AccessToken: "token-a", ExternalUserID: "101",
					UserIDHeaderName: "New-API-User", QuotaPerUnit: 100},
				{AccountID: 22, ChannelID: 7, Family: collector.FamilyNewAPI,
					BaseURL: server.URL, AccessToken: "token-b", ExternalUserID: "202",
					UserIDHeaderName: "New-API-User", QuotaPerUnit: 100},
			}, nil
		},
	}

	result, err := runner.Sync(context.Background(), store.Channel{
		ID: 7, SiteFamily: string(collector.FamilyNewAPI), BaseURL: server.URL,
	}, nil)
	if err != nil {
		t.Fatalf("多账号采集: %v", err)
	}
	if len(sink.accounts) != 2 || len(sink.keys) != 2 {
		t.Fatalf("账号级数据未逐账号保存: accounts=%+v keys=%+v", sink.accounts, sink.keys)
	}
	accountIDs := []int64{sink.accounts[0].accountID, sink.accounts[1].accountID}
	keyAccountIDs := []int64{sink.keys[0].accountID, sink.keys[1].accountID}
	sort.Slice(accountIDs, func(i, j int) bool { return accountIDs[i] < accountIDs[j] })
	sort.Slice(keyAccountIDs, func(i, j int) bool { return keyAccountIDs[i] < keyAccountIDs[j] })
	if accountIDs[0] != 11 || accountIDs[1] != 22 ||
		keyAccountIDs[0] != 11 || keyAccountIDs[1] != 22 {
		t.Fatalf("账号归属错误: accounts=%v keys=%v", accountIDs, keyAccountIDs)
	}
	if len(sink.groups) != 1 || len(sink.groups[0]) != 3 ||
		len(sink.pricing) != 1 || len(sink.pricing[0].Models) != 3 ||
		len(sink.catalogs) != 1 || len(sink.catalogs[0]) != 3 {
		t.Fatalf("渠道级数据未合并后单次保存: groups=%d/%d pricing=%d/%d catalog=%d/%d",
			len(sink.groups), firstLen(sink.groups), len(sink.pricing), firstPricingLen(sink.pricing),
			len(sink.catalogs), firstLen(sink.catalogs))
	}
	if len(result.Items) != 5 {
		t.Fatalf("同步结果应按能力合并为 5 项，得到 %+v", result.Items)
	}
}

func firstLen[T any](items [][]T) int {
	if len(items) == 0 {
		return 0
	}
	return len(items[0])
}

func firstPricingLen(items []collector.Pricing) int {
	if len(items) == 0 {
		return 0
	}
	return len(items[0].Models)
}
