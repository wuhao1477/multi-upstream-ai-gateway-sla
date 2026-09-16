package store

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// all-api-hub 同步「备份里已移除即停用」的三条边界。
//
// 走真库（SLA_TEST_DSN，由 verify/test-migrate.sh 提供），不用 sqlmock ——
// 被测的几乎全是 SQL 的 WHERE 条件本身（source / status / ANY(...)），
// 给它加 mock 只会得到"测试 mock 有没有按我说的返回"（CLAUDE.md §1）。
func hubTestConn(t *testing.T) (*pgx.Conn, context.Context) {
	t.Helper()
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库（由 verify/test-migrate.sh 提供）")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn, ctx
}

// wipeChannelTree 按外键的反向顺序清掉一个测试渠道及其下挂。
//
// 只删 channels 是不够的：账号/凭证/Key 上有外键，删不动而错误又被吞掉，
// 表现是下一次 CreateChannel 撞 base_url 唯一约束（019）—— 而那个报错
// 指向"已有渠道使用地址…"，看起来像被测代码的问题，其实是上一轮没清干净。
func wipeChannelTree(ctx context.Context, conn *pgx.Conn, base string) {
	_, _ = conn.Exec(ctx, `
DELETE FROM upstream_keys WHERE account_id IN (
  SELECT a.id FROM upstream_accounts a JOIN channels c ON c.id=a.channel_id WHERE c.base_url=$1)`, base)
	_, _ = conn.Exec(ctx, `
DELETE FROM collector_credentials WHERE channel_id IN (
  SELECT id FROM channels WHERE base_url=$1)`, base)
	_, _ = conn.Exec(ctx, `
DELETE FROM upstream_accounts WHERE channel_id IN (
  SELECT id FROM channels WHERE base_url=$1)`, base)
	_, _ = conn.Exec(ctx, `DELETE FROM channels WHERE base_url=$1`, base)
}

func mkChannel(t *testing.T, ctx context.Context, conn *pgx.Conn, name, base, source string) int64 {
	t.Helper()
	wipeChannelTree(ctx, conn, base)
	id, err := CreateChannel(ctx, conn, Channel{
		Name: name, BaseURL: base, SiteFamily: "newapi", Source: source,
	})
	if err != nil {
		t.Fatalf("建渠道 %s: %v", name, err)
	}
	t.Cleanup(func() { wipeChannelTree(context.Background(), conn, base) })
	return id
}

// 手工建的渠道**绝不能**被"备份里已移除"停掉。
//
// 这是 031 那一列存在的全部理由：没有它，定时同步每一轮都会把手工加的渠道
// 再关一次，人每次重新启用、下一轮又被关。而"不在备份里"对这两类渠道
// 在数据上长得一模一样。
func TestDisableHubRemovedChannelsNeverTouchesManualOnes(t *testing.T) {
	conn, ctx := hubTestConn(t)
	const keep = "https://hub-keep.example.invalid"
	const goneURL = "https://hub-gone.example.invalid"
	const manual = "https://hand-made.example.invalid"

	keepID := mkChannel(t, ctx, conn, "留着的", keep, "hub")
	goneID := mkChannel(t, ctx, conn, "备份里没了的", goneURL, "hub")
	manualID := mkChannel(t, ctx, conn, "手工加的", manual, "manual")

	// ⚠️ 只断言**本测试自己那三条**，不断言返回集合的大小。
	// 这是一个共享的测试库，里面还有别的用例与手工验证留下的渠道 ——
	// 按全局计数断言等于把"库里此刻有什么"写死进测试，而那会随下一个用例漂移
	// （仓库记忆「靶子别写死」同一条）。
	got, err := DisableHubRemovedChannels(ctx, conn, []string{keep}, "备份里已移除")
	if err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	touched := map[int64]bool{}
	for _, c := range got {
		touched[c.ID] = true
	}
	if !touched[goneID] {
		t.Fatal("备份里已移除的 hub 渠道没被停用")
	}
	if touched[manualID] {
		t.Fatal("手工建的渠道出现在停用名单里")
	}
	if touched[keepID] {
		t.Fatal("备份里还在的渠道出现在停用名单里")
	}
	status := func(id int64) string {
		var s string
		if err := conn.QueryRow(ctx, `SELECT status FROM channels WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatalf("查状态: %v", err)
		}
		return s
	}
	if status(manualID) != "enabled" {
		t.Fatal("手工建的渠道被「备份里已移除」停掉了 —— 它本来就不在任何备份里，这一停会每轮复发")
	}
	if status(keepID) != "enabled" {
		t.Fatal("备份里还在的渠道被误停")
	}
	if status(goneID) != "disabled" {
		t.Fatal("备份里已移除的 hub 渠道没被停用")
	}
}

// keep 为空时**什么都不做**。
//
// 一次取备份失败、或解析出空列表时，若照此停用，一轮就能把整个台账关掉。
// 空列表不是"全都没了"，是"这次没读到"——两者必须分开。
func TestDisableHubRemovedChannelsDoesNothingOnEmptyKeepList(t *testing.T) {
	conn, ctx := hubTestConn(t)
	id := mkChannel(t, ctx, conn, "别动我", "https://hub-empty-keep.example.invalid", "hub")

	got, err := DisableHubRemovedChannels(ctx, conn, nil, "备份里已移除")
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("空备份列表时不该停用任何渠道，实际停了 %d 个", len(got))
	}
	var status string
	if err := conn.QueryRow(ctx, `SELECT status FROM channels WHERE id=$1`, id).Scan(&status); err != nil {
		t.Fatalf("查状态: %v", err)
	}
	if status != "enabled" {
		t.Fatal("取备份失败（空列表）时把渠道停了 —— 一轮就能关掉整个台账")
	}
}

// 已经停用的不重写：否则每轮同步都会覆盖掉人自己填的停用原因。
func TestDisableHubRemovedChannelsKeepsExistingDisabledReason(t *testing.T) {
	conn, ctx := hubTestConn(t)
	id := mkChannel(t, ctx, conn, "早就停了", "https://hub-already-off.example.invalid", "hub")
	if _, err := conn.Exec(ctx,
		`UPDATE channels SET status='disabled', disabled_reason='站点跑路了' WHERE id=$1`, id); err != nil {
		t.Fatalf("预置停用态: %v", err)
	}

	if _, err := DisableHubRemovedChannels(ctx, conn,
		[]string{"https://something-else.example.invalid"}, "备份里已移除"); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	var reason string
	if err := conn.QueryRow(ctx,
		`SELECT COALESCE(disabled_reason,'') FROM channels WHERE id=$1`, id).Scan(&reason); err != nil {
		t.Fatalf("查原因: %v", err)
	}
	if reason != "站点跑路了" {
		t.Fatalf("不该覆盖人自己填的停用原因，实际变成了 %q", reason)
	}
}

// 凭证比对在 SQL 里做，且要能分清"没有凭证"与"有但一样"。
func TestCredentialTokenDiffers(t *testing.T) {
	conn, ctx := hubTestConn(t)
	chID := mkChannel(t, ctx, conn, "比凭证", "https://cred-diff.example.invalid", "hub")
	accID, err := CreateAccount(ctx, conn, Account{ChannelID: chID, ExternalUserID: "1663"})
	if err != nil {
		t.Fatalf("建账号: %v", err)
	}

	// 还没有凭证
	differs, exists, err := CredentialTokenDiffers(ctx, conn, accID, "whatever")
	if err != nil {
		t.Fatalf("比对失败: %v", err)
	}
	if exists {
		t.Fatal("这个账号还没有凭证，exists 应为 false —— 调用方据此走「新登记」而不是「更新」")
	}
	if differs {
		t.Fatal("没有凭证时不该报 differs")
	}
}
