package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// importOne 的失败原子性与半成品补齐 —— 打**真 PG**，由 test-migrate.sh 第 8 步驱动。
//
// 为什么必须有这两条（2026-08-29 二次评审查出）：`POST /admin/import/all-api-hub`
// 的写路径此前**零自动覆盖**。58 项浏览器验收只跑 `dry_run=true`（注释写明理由：
// 真导入会往库里写上百个渠道，那是运维的决定），于是四次独立写入中途失败会留下
// 半成品这件事，没有任何断言看得见。
//
// ⚠️ 关于 CLAUDE.md §1（禁 mock）：这里注入一个**必然失败**的 SaveCredential，
// 属"被测对象本身就是假的"那类例外 —— 被测的是**回滚行为**，而真依赖不肯按需
// 失败（真 PG 在凭证写入这一步好得很，你没法让它只在第四步报错）。判据是那条
// 原文：真依赖能不能按需产出这个输入。不能，才轮到造。
// 库是真的、渠道/账号/凭证三张表是真的、事务是真的 —— 造的只有那一个错误。
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库（由 verify/test-migrate.sh 提供）")
	}
	return dsn
}

func testServer() *Server {
	return &Server{
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelError, // 这两条会刻意制造失败，warn 噪音没用
		})),
	}
}

// wipe 清掉某个 base_url 的渠道（连带账号/凭证靠外键级联）。
//
// **每条测试开跑前都要调**，不能只在结束时清：上一轮若中途崩溃、或跑过一次
// 反向自验（破坏后的代码正好不回滚），库里就留着残渣，于是下一轮会红在
// "回滚不彻底"上 —— 而那时被测代码是好的。实测踩过：破坏自验之后三条里两条红，
// 看起来像事务没生效，实际是 7 行残渣。
// ⚠️ 必须逐张删子表：`channels` 的外键**多数是 NO ACTION 而非 CASCADE**
// （只有 channel_groups 与 channel_model_catalog 是 CASCADE）。第一版直接
// `DELETE FROM channels` 撞了 upstream_accounts_channel_id_fkey，而当时那句
// 错误被 `_, _ =` 吞掉，表现成"清了但没清掉"。
func wipe(ctx context.Context, t *testing.T, conn *pgx.Conn, base string) {
	t.Helper()
	for _, tbl := range []string{
		"upstream_accounts", "collector_credentials", "collector_snapshots",
	} {
		if _, err := conn.Exec(ctx, `DELETE FROM `+tbl+
			` WHERE channel_id IN (SELECT id FROM channels WHERE base_url=$1)`,
			base); err != nil {
			t.Fatalf("清 %s 里 %s 的残留: %v", base, tbl, err)
		}
	}
	if _, err := conn.Exec(ctx, `DELETE FROM channels WHERE base_url=$1`, base); err != nil {
		t.Fatalf("清 %s 的残留: %v", base, err)
	}
}

// hubAcct 造一条导出账号。site 必须唯一，否则会撞上"同 base_url 已存在"。
func hubAcct(site string) collector.HubAccount {
	var a collector.HubAccount
	a.SiteName = "事务测试-" + site
	a.SiteURL = "https://" + site + ".example.invalid"
	a.AccountInfo.ID = json.RawMessage(`"4242"`)
	a.AccountInfo.AccessToken = "tk-" + site
	return a
}

func detected() collector.DetectResult {
	return collector.DetectResult{
		Family: collector.FamilyNewAPI, Version: "v1-test", NoShield: true,
		QuotaPerUnit: 500000,
	}
}

func countFor(ctx context.Context, t *testing.T, conn *pgx.Conn, base string) (ch, acc, cred int) {
	t.Helper()
	err := conn.QueryRow(ctx, `
SELECT (SELECT count(*) FROM channels WHERE base_url=$1),
       (SELECT count(*) FROM upstream_accounts a
          JOIN channels c ON c.id=a.channel_id WHERE c.base_url=$1),
       (SELECT count(*) FROM collector_credentials cc
          JOIN channels c ON c.id=cc.channel_id WHERE c.base_url=$1)`,
		base).Scan(&ch, &acc, &cred)
	if err != nil {
		t.Fatalf("数行数: %v", err)
	}
	return
}

// TestImportRollsBackOnCredentialFailure 凭证写入失败必须回滚掉渠道与账号。
//
// 这是 Codex 二次评审那条 [high] 的守卫：原先渠道/快照/账号已提交、只有凭证失败，
// 条目标成 failed 而库里留下一行采不到数据的渠道 —— 且没有 DELETE 渠道的入口。
func TestImportRollsBackOnCredentialFailure(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer conn.Close(ctx)

	s := testServer()
	boom := errors.New("刻意失败：模拟凭证写入报错")
	s.SaveCredential = func(context.Context, store.DBTX, collector.Credential) error {
		return boom
	}
	s.SaveDetected = func(ctx context.Context, db store.DBTX, id int64, d collector.DetectResult) error {
		return (&store.CredentialStore{}).SaveDetected(ctx, db, id, d)
	}

	a := hubAcct("rollback")
	base := a.SiteURL
	wipe(ctx, t, conn, base)
	defer wipe(context.Background(), t, conn, base)

	var it collector.HubImportItem
	err = s.importOne(ctx, conn, a, detected(), &it)
	if err == nil {
		t.Fatal("SaveCredential 报错了，importOne 却返回 nil —— 失败被吞掉了")
	}
	if !errors.Is(err, boom) {
		t.Errorf("错误没包住原因：%v", err)
	}

	ch, acc, cred := countFor(ctx, t, conn, base)
	if ch != 0 || acc != 0 || cred != 0 {
		t.Errorf("回滚不彻底：渠道 %d 行 / 账号 %d 行 / 凭证 %d 行，全应为 0\n"+
			"   半成品的后果：那行渠道每轮 sync 都报 ErrPrecondition（读不到凭证），"+
			"而重导会按 base_url 判为已存在；库里也没有 DELETE 渠道的入口。",
			ch, acc, cred)
	}
	if it.Status == "imported" {
		t.Errorf("条目状态是 %q，失败的导入不该报 imported", it.Status)
	}
}

// TestImportRepairsIncompleteChannel 已存在但缺凭证的渠道要能被重导补齐。
//
// 与上一条互为另一半：光有回滚不够 —— 万一半成品已经在库里（本轮之前留下的，
// 或凭证被手工删过），重导必须能补上，而原先它会直接 skipped 让那行永久残缺。
func TestImportRepairsIncompleteChannel(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer conn.Close(ctx)

	a := hubAcct("repair")
	base := a.SiteURL
	wipe(ctx, t, conn, base)
	defer wipe(context.Background(), t, conn, base)

	// 先造一个半成品：只有渠道行，没有账号也没有凭证。
	// 直接写库而不是"跑一次失败的导入"—— 后者依赖上一条测试的行为，
	// 两条测试互相依赖时，一条坏了会让另一条以看不懂的方式红。
	var chID int64
	if err := conn.QueryRow(ctx, `
INSERT INTO channels (name, base_url, site_family, status)
VALUES ($1,$2,'newapi','enabled') RETURNING id`,
		"事务测试-半成品", base).Scan(&chID); err != nil {
		t.Fatalf("造半成品: %v", err)
	}

	s := testServer()
	var saved int
	s.SaveCredential = func(ctx context.Context, db store.DBTX, c collector.Credential) error {
		saved++
		if c.ChannelID != chID {
			return fmt.Errorf("补到了别的渠道 %d，应为 %d", c.ChannelID, chID)
		}
		return (&store.CredentialStore{}).SaveTx(ctx, db, c)
	}
	s.SaveDetected = func(ctx context.Context, db store.DBTX, id int64, d collector.DetectResult) error {
		return (&store.CredentialStore{}).SaveDetected(ctx, db, id, d)
	}

	var it collector.HubImportItem
	if err := s.importOne(ctx, conn, a, detected(), &it); err != nil {
		t.Fatalf("补齐失败: %v", err)
	}
	if it.Status != "imported" {
		t.Errorf("状态 %q，补齐后应为 imported（原先这里是 skipped —— "+
			"于是半成品永不自愈）", it.Status)
	}
	if it.ChannelID != chID {
		t.Errorf("补齐建了新渠道 %d，应复用已有的 %d", it.ChannelID, chID)
	}
	if saved != 1 {
		t.Errorf("SaveCredential 调了 %d 次，应恰好 1 次", saved)
	}
	ch, acc, cred := countFor(ctx, t, conn, base)
	if ch != 1 || acc != 1 || cred != 1 {
		t.Errorf("补齐后应是 1/1/1，实际渠道 %d / 账号 %d / 凭证 %d", ch, acc, cred)
	}
}

// TestImportRejectsBadBaseURL 导入必须复用 createChannel 的 base_url 校验。
//
// 原先导入侧把导出文件里的 SiteURL 直传 Detect，连 http/https 前缀都不查 ——
// 同一个字段两个入口两套规矩，而更宽的那个恰好是不经人眼逐条确认的批量路径。
func TestImportRejectsBadBaseURL(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer conn.Close(ctx)

	s := testServer()
	s.SaveCredential = func(context.Context, store.DBTX, collector.Credential) error { return nil }

	for _, bad := range []string{"file:///etc/passwd", "ftp://x.example", "not-a-url", ""} {
		a := hubAcct("badurl")
		a.SiteURL = bad
		wipe(ctx, t, conn, bad)
		var it collector.HubImportItem
		if err := s.importOne(ctx, conn, a, detected(), &it); err == nil {
			t.Errorf("base_url %q 被接受了 —— 校验没生效", bad)
		}
		wipe(ctx, t, conn, bad)
	}
}
