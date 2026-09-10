package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
)

// TestListAccountsExposesCollectedBalance 验证账号读路径把采集器写进
// balance_signals 的余额带出来，且三件事分得开：
//
//	① 从未采过     → BalanceUSD == nil（**不是 0**）
//	② 采到 125.30  → 带金额、状态与确认时刻
//	③ 采集降级     → 不写新行，上一次成功值仍在（FR-026）
//
// 这三条是账号管理页「账号余额」列的全部前提。第 ① 条尤其要命：把 nil
// 渲染成 0 会让一个查不到余额的账号看起来像已耗尽，运维会去充一笔不需要的钱。
func TestListAccountsExposesCollectedBalance(t *testing.T) {
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库（由 verify/test-migrate.sh 提供）")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	pool, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("建连接池: %v", err)
	}
	defer pool.Close()

	const base = "https://account-balance-readpath.example.invalid"
	wipeInventoryTest(ctx, t, conn, base)
	defer wipeInventoryTest(context.Background(), t, conn, base)

	channelID, err := CreateChannel(ctx, conn, Channel{
		Name: "account-balance-readpath", SiteFamily: "newapi", BaseURL: base,
	})
	if err != nil {
		t.Fatalf("建渠道: %v", err)
	}
	accountID, err := CreateAccount(ctx, conn, Account{ChannelID: channelID})
	if err != nil {
		t.Fatalf("建账号: %v", err)
	}

	// ① 还没采过。
	got := oneAccount(ctx, t, conn, channelID)
	if got.BalanceUSD != nil {
		t.Fatalf("从未采集的账号余额必须是 nil（未采集）而不是 %v", *got.BalanceUSD)
	}
	if got.BalanceState != "" {
		t.Fatalf("从未采集的账号不该有余额状态，得到 %q", got.BalanceState)
	}

	// ② 采到一次。
	sink := NewCollectorSink(pool)
	confirmed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := sink.SaveAccount(ctx, channelID, accountID, collector.Account{
		BalanceUSD: 125.30,
		Meta:       collector.SourceMeta{FetchedAt: confirmed},
	}); err != nil {
		t.Fatalf("写账号余额: %v", err)
	}
	got = oneAccount(ctx, t, conn, channelID)
	if got.BalanceUSD == nil || *got.BalanceUSD != 125.30 {
		t.Fatalf("账号余额 = %v，期望 125.30", got.BalanceUSD)
	}
	if got.BalanceState != "normal" {
		t.Fatalf("余额状态 = %q，期望 normal", got.BalanceState)
	}
	if got.BalanceConfirmedAt == nil || !got.BalanceConfirmedAt.Equal(confirmed) {
		t.Fatalf("确认时刻 = %v，期望 %v", got.BalanceConfirmedAt, confirmed)
	}

	// ③ 降级采集（站型这轮没给余额）：不写新行，上一次成功值必须还在。
	if err := sink.SaveAccount(ctx, channelID, accountID, collector.Account{
		Meta: collector.SourceMeta{FetchedAt: time.Now(), Degraded: true},
	}); err != nil {
		t.Fatalf("降级采集: %v", err)
	}
	got = oneAccount(ctx, t, conn, channelID)
	if got.BalanceUSD == nil || *got.BalanceUSD != 125.30 {
		t.Fatalf("降级采集不得覆盖最近一次成功余额（FR-026），得到 %v", got.BalanceUSD)
	}
}

// TestListAccountsCountsKeys 验证 Key 计数按状态分开 ——
// 停用账号的确认框要写「将影响 N 把 Key」，用的是 KeysActive。
func TestListAccountsCountsKeys(t *testing.T) {
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	const base = "https://account-key-counts.example.invalid"
	wipeInventoryTest(ctx, t, conn, base)
	defer wipeInventoryTest(context.Background(), t, conn, base)

	channelID, err := CreateChannel(ctx, conn, Channel{
		Name: "account-key-counts", SiteFamily: "newapi", BaseURL: base,
	})
	if err != nil {
		t.Fatalf("建渠道: %v", err)
	}
	accountID, err := CreateAccount(ctx, conn, Account{ChannelID: channelID})
	if err != nil {
		t.Fatalf("建账号: %v", err)
	}
	for i, ref := range []string{"k1", "k2", "k3"} {
		keyID, err := CreateKey(ctx, conn, accountID, "sk-"+ref, ref, nil)
		if err != nil {
			t.Fatalf("建 Key %s: %v", ref, err)
		}
		if i == 2 {
			if err := DisableKey(ctx, conn, keyID); err != nil {
				t.Fatalf("停用 Key: %v", err)
			}
		}
	}

	got := oneAccount(ctx, t, conn, channelID)
	if got.KeysTotal != 3 || got.KeysActive != 2 {
		t.Fatalf("Key 计数 = %d/%d，期望 3 总数 / 2 可用", got.KeysTotal, got.KeysActive)
	}
}

// TestSaveKeyPersistsUnlimitedQuota 验证「不限额度」落库并被读路径带出。
//
// 不落这一列的后果是可复现的：NewAPI 对 unlimited_quota 的 Key 回
// remain_quota=0，界面于是渲染 $0.0000 —— 与"额度耗尽"完全无法区分。
// 本用例连着断言了**改回有限额时必须能改回来**：这一列是每轮覆写而不是
// COALESCE 保留，写成 COALESCE 的话上游收回无限额后界面永远显示"不限额度"。
func TestSaveKeyPersistsUnlimitedQuota(t *testing.T) {
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	pool, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("建连接池: %v", err)
	}
	defer pool.Close()

	const base = "https://key-unlimited-quota.example.invalid"
	wipeInventoryTest(ctx, t, conn, base)
	defer wipeInventoryTest(context.Background(), t, conn, base)

	channelID, err := CreateChannel(ctx, conn, Channel{
		Name: "key-unlimited-quota", SiteFamily: "newapi", BaseURL: base,
	})
	if err != nil {
		t.Fatalf("建渠道: %v", err)
	}
	accountID, err := CreateAccount(ctx, conn, Account{ChannelID: channelID})
	if err != nil {
		t.Fatalf("建账号: %v", err)
	}
	keyID, err := CreateKey(ctx, conn, accountID, "sk-unlimited", "unlimited-key", nil)
	if err != nil {
		t.Fatalf("建 Key: %v", err)
	}

	sink := NewCollectorSink(pool)
	// 无限额度：上游回 remain_quota=0，靠 Unlimited 区分它不是耗尽。
	if err := sink.SaveKey(ctx, channelID, accountID, collector.Key{
		KeyRef: "unlimited-key", Unlimited: true, RemainQuotaUSD: quotaPtr(0),
		Meta: collector.SourceMeta{FetchedAt: time.Now()},
	}); err != nil {
		t.Fatalf("保存无限额度 Key: %v", err)
	}
	k, err := GetKey(ctx, conn, keyID)
	if err != nil {
		t.Fatalf("读 Key: %v", err)
	}
	if !k.UnlimitedQuota {
		t.Fatal("unlimited_quota 未落库：无限额度 Key 会被渲染成 $0.0000（形同额度耗尽）")
	}

	// 上游收回无限额：必须能改回 false。
	if err := sink.SaveKey(ctx, channelID, accountID, collector.Key{
		KeyRef: "unlimited-key", Unlimited: false, RemainQuotaUSD: quotaPtr(5),
		Meta: collector.SourceMeta{FetchedAt: time.Now()},
	}); err != nil {
		t.Fatalf("保存有限额度 Key: %v", err)
	}
	k, err = GetKey(ctx, conn, keyID)
	if err != nil {
		t.Fatalf("重读 Key: %v", err)
	}
	if k.UnlimitedQuota {
		t.Fatal("unlimited_quota 必须每轮覆写，否则上游收回无限额后界面永远显示「不限额度」")
	}
}

// oneAccount 取该渠道下唯一的那个账号。用 ListAccounts 而不是裸 SQL ——
// 被测的正是这个读路径。
func oneAccount(ctx context.Context, t *testing.T, conn *pgx.Conn, channelID int64) Account {
	t.Helper()
	as, err := ListAccounts(ctx, conn, channelID)
	if err != nil {
		t.Fatalf("列账号: %v", err)
	}
	if len(as) != 1 {
		t.Fatalf("账号数 = %d，期望 1", len(as))
	}
	return as[0]
}
