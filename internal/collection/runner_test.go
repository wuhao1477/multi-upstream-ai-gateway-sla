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
	}})
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
