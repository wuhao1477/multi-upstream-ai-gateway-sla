package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
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
// ⚠️ 匹配是**大小写与尾斜杠都不敏感**的（2026-09-01 改）。原先是 `base_url=$1`
// 精确匹配，于是每加一种"规范化会把它折叠掉"的拼法，调用方就得多传一次 ——
// 而漏掉一种的后果是：破坏自验那轮真的插进了 `https://UNIQ-GUARD.…`（scheme 被
// url.Parse 小写、host 没被小写），精确匹配清不掉它，下一轮就红在"落了 2 行"上，
// 而那时被测代码是好的。实测踩过一次。
// 这里刻意与被测的规范化**同口径而不共用它**：共用等于让清理跟着被破坏的那个
// 函数一起失效，那正是这条清理要防的情形。
func wipe(ctx context.Context, t *testing.T, conn *pgx.Conn, base string) {
	t.Helper()
	const match = `lower(rtrim(base_url,'/')) = lower(rtrim($1,'/'))`
	for _, tbl := range []string{
		"upstream_accounts", "collector_credentials", "collector_snapshots",
	} {
		if _, err := conn.Exec(ctx, `DELETE FROM `+tbl+
			` WHERE channel_id IN (SELECT id FROM channels WHERE `+match+`)`,
			base); err != nil {
			t.Fatalf("清 %s 里 %s 的残留: %v", base, tbl, err)
		}
	}
	if _, err := conn.Exec(ctx,
		`DELETE FROM channels WHERE `+match, base); err != nil {
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

func TestUnknownImportedChannelNeedsFamilyRepair(t *testing.T) {
	if !needsFamilyRepair("unknown", collector.FamilyNewAPI) {
		t.Fatal("已有渠道为 unknown、探测得到 newapi 时必须补写站型")
	}
	if needsFamilyRepair("newapi", collector.FamilyNewAPI) {
		t.Fatal("已有站型正确时不应重复修改")
	}
}

func TestImportRepairUpdatesCredentialFamily(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	a := hubAcct("family-repair")
	base := a.SiteURL
	wipe(ctx, t, conn, base)
	defer wipe(context.Background(), t, conn, base)

	var chID int64
	if err := conn.QueryRow(ctx, `
INSERT INTO channels (name, base_url, site_family, status)
VALUES ($1,$2,'unknown','enabled') RETURNING id`,
		"事务测试-站型补齐", base).Scan(&chID); err != nil {
		t.Fatalf("造 unknown 渠道: %v", err)
	}
	if _, err := store.CreateAccount(ctx, conn, store.Account{
		ChannelID: chID, ExternalUserID: a.UserID(),
	}); err != nil {
		t.Fatalf("造账号: %v", err)
	}
	credentials := &store.CredentialStore{}
	if err := credentials.SaveTx(ctx, conn, collector.Credential{
		ChannelID: chID, Family: collector.FamilyUnknown,
		CredType: "newapi_access_token", AccessToken: a.AccountInfo.AccessToken,
		ExternalUserID: a.UserID(),
	}); err != nil {
		t.Fatalf("造 unknown 凭证: %v", err)
	}
	if err := credentials.SaveDetected(ctx, conn, chID, detected()); err != nil {
		t.Fatalf("造探测快照: %v", err)
	}

	var it collector.HubImportItem
	if err := testServer().importOne(ctx, conn, a, detected(), &it); err != nil {
		t.Fatalf("补站型失败: %v", err)
	}
	var channelFamily, credentialFamily string
	if err := conn.QueryRow(ctx, `
SELECT c.site_family, cc.site_family
  FROM channels c JOIN collector_credentials cc ON cc.channel_id=c.id
 WHERE c.id=$1`, chID).Scan(&channelFamily, &credentialFamily); err != nil {
		t.Fatalf("读补齐结果: %v", err)
	}
	if channelFamily != "newapi" || credentialFamily != "newapi" {
		t.Fatalf("站型未同步补齐：channel=%q credential=%q，期望均为 newapi",
			channelFamily, credentialFamily)
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
	defer func() { _ = conn.Close(ctx) }()

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
	defer func() { _ = conn.Close(ctx) }()

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

// TestImportRepairAddsSnapshotAndKeepsWarning 补齐要补上探测快照，且不吃掉已有 warning。
//
// 两条 2026-09-01 自审查出的缺陷合成一条测试（同一次 importOne 就能同时暴露）：
//
//  1. incompleteParts 原先只查账号与凭证两张表 —— "有账号有凭证但没有 __detect__
//     快照"的渠道被判为完整 → skipped，而 QuotaPerUnit() 取不到会返 0、
//     FetchAccount 直接报错（04 §2：不猜，猜错差 50 万倍）。实测过：手工建一个
//     只有渠道行的半成品再导入，报 imported 而快照仍是 0 行。
//  2. 补齐分支原先是 `it.Warning = …` **整句覆盖** —— 探测阶段写下的
//     「turnstile…需转人工录入」或「导出里没有凭证」被抹掉，而 finishImport
//     靠 strings.Contains(it.Warning, …) 统计 Shielded / NoCredential，
//     覆盖之后这两个计数直接少计。
func TestImportRepairAddsSnapshotAndKeepsWarning(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	a := hubAcct("snapwarn")
	base := a.SiteURL
	wipe(ctx, t, conn, base)
	defer wipe(context.Background(), t, conn, base)

	// 半成品：只有渠道行。等价于 auto_detect=false 建出来的渠道。
	var chID int64
	if err := conn.QueryRow(ctx, `
INSERT INTO channels (name, base_url, site_family, status)
VALUES ($1,$2,'newapi','enabled') RETURNING id`,
		"事务测试-缺快照", base).Scan(&chID); err != nil {
		t.Fatalf("造半成品: %v", err)
	}

	s := testServer()
	s.SaveCredential = func(ctx context.Context, db store.DBTX, c collector.Credential) error {
		return (&store.CredentialStore{}).SaveTx(ctx, db, c)
	}
	s.SaveDetected = func(ctx context.Context, db store.DBTX, id int64, d collector.DetectResult) error {
		return (&store.CredentialStore{}).SaveDetected(ctx, db, id, d)
	}

	// 探测阶段会写的那种 warning —— 补齐分支不得把它吃掉
	const pre = "该站开启 turnstile 人机验证，服务端自动采集不可行，需转人工录入（04 §6）"
	it := collector.HubImportItem{Warning: pre}
	if err := s.importOne(ctx, conn, a, detected(), &it); err != nil {
		t.Fatalf("补齐失败: %v", err)
	}
	if it.Status != "imported" {
		t.Errorf("状态 %q，应为 imported", it.Status)
	}
	if !strings.Contains(it.Warning, "turnstile") {
		t.Errorf("补齐把探测阶段的 warning 覆盖了：%q\n"+
			"   → finishImport 靠 Contains 统计 Shielded/NoCredential，覆盖后计数少计，"+
			"界面上「需转人工录入」的提示也消失。", it.Warning)
	}
	if !strings.Contains(it.Warning, "补齐") {
		t.Errorf("warning 里没有补齐说明：%q", it.Warning)
	}

	var snaps int
	if err := conn.QueryRow(ctx, `
SELECT count(*) FROM collector_snapshots
 WHERE channel_id=$1 AND scope_type='pricing' AND scope_id='__detect__'`, chID).
		Scan(&snaps); err != nil {
		t.Fatalf("数探测快照: %v", err)
	}
	if snaps != 1 {
		t.Errorf("补齐后 __detect__ 快照 %d 行，应为 1\n"+
			"   → 缺它则 QuotaPerUnit() 返 0，FetchAccount 报错：报了 imported 的渠道"+
			"其实一次都采不了。", snaps)
	}
}

// TestImportRejectsBadBaseURL 导入必须复用 createChannel 的 base_url 校验。

// 原先导入侧把导出文件里的 SiteURL 直传 Detect，连 http/https 前缀都不查 ——
// 同一个字段两个入口两套规矩，而更宽的那个恰好是不经人眼逐条确认的批量路径。
func TestImportRejectsBadBaseURL(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

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
