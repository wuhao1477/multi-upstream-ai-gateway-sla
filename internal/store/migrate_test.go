package store

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestLoadMigrationsOrdered 迁移顺序即依赖顺序（02 §9.1bis 建表顺序原则：
// 被引用的表先建）。文件名前缀编码了这个顺序，排序错了会撞前向外键。
func TestLoadMigrationsOrdered(t *testing.T) {
	ms, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	// ⚠️ **不写死条数**：新增迁移是常态（013 就是实现期补的），
	// 写死会让每次加文件都挂一次测试（这个坑在 shell 脚本里已踩过两次）。
	// 只断言"至少有基线的 12 个"与顺序性 —— 那才是本测试要守的。
	if len(ms) < 12 {
		t.Fatalf("迁移文件数 = %d，少于基线 12 个（是否被误删？）", len(ms))
	}
	for i := 1; i < len(ms); i++ {
		if ms[i-1].Name >= ms[i].Name {
			t.Errorf("顺序错乱: %s 应在 %s 之前", ms[i-1].Name, ms[i].Name)
		}
	}
	// 001 必须含金额域与注册表根（其余表都外键指向它们）
	if !strings.Contains(ms[0].SQL, "CREATE DOMAIN usd_amount") {
		t.Error("001 应含 usd_amount 域——它被几十个列引用")
	}
	if !strings.Contains(ms[0].SQL, "CREATE TABLE upstream_providers") {
		t.Error("001 应含 upstream_providers——channels 外键指向它")
	}
}

// TestChecksumStable 同一内容的校验和必须稳定，否则每次启动都会误判"内容已变"。
func TestChecksumStable(t *testing.T) {
	a, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for i := range a {
		if a[i].Checksum != b[i].Checksum {
			t.Errorf("%s 的校验和不稳定", a[i].Name)
		}
		if len(a[i].Checksum) != 16 {
			t.Errorf("%s 校验和长度 = %d，期望 16", a[i].Name, len(a[i].Checksum))
		}
	}
}

// TestP1TablesPresent P1 的三张新表与 upstream_keys 六列必须在迁移里
// （02 §1.3；它们是 #6/#7/#10 的落库目标）。
func TestP1TablesPresent(t *testing.T) {
	ms, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, m := range ms {
		all += m.SQL
	}
	for _, tbl := range []string{"channel_groups", "group_models", "channel_model_catalog"} {
		if !strings.Contains(all, "CREATE TABLE "+tbl) {
			t.Errorf("缺 P1 表 %s", tbl)
		}
	}
	for _, col := range []string{
		"channel_group_id", "remain_quota_usd", "used_quota_usd",
		"rpm_limit", "concurrency_limit", "quota_synced_at",
	} {
		if !strings.Contains(all, col) {
			t.Errorf("缺 upstream_keys 的 P1 列 %s", col)
		}
	}
}

func TestCatalogRoundColumnsPresent(t *testing.T) {
	ms, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var all string
	for _, migration := range ms {
		all += migration.SQL
	}
	for _, column := range []string{"catalog_sync_seq", "last_seen_seq"} {
		if !strings.Contains(all, column) {
			t.Errorf("缺真实目录轮次列 %s", column)
		}
	}
}

func TestCredentialRefreshLockMigrationPresent(t *testing.T) {
	ms, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, migration := range ms {
		if migration.Name == "021_credential_refresh_lock.sql" {
			sql = migration.SQL
			break
		}
	}
	if !strings.Contains(sql, "refresh_lock_key") ||
		!strings.Contains(sql, "rtrim(btrim(ch.base_url), '/')") {
		t.Fatal("021 必须包含 refresh_lock_key 存量回填")
	}
	if strings.Contains(sql, "lower(rtrim(ch.base_url") {
		t.Fatal("021 不得整串 lower base_url：path 大小写敏感，迁移必须只小写 scheme/host")
	}
	if !strings.Contains(sql, "collector_credentials_refresh_lock_key_check") {
		t.Fatal("021 必须约束 Sub2API 凭证有 refresh_lock_key")
	}
}

// ledgerTables 是 FR-112「不存正文」约束的作用域（02 §9.2 原文：
// 「禁止任何**账本表**出现 body/messages/prompt/completion_text/headers 命名列」）。
//
// ⚠️ 作用域必须限定在账本表，不能全库扫：`probe_templates.body` 是**合法**的
// —— 那是运维编写的探测请求体，不是用户正文，02 §6ter 明写「不受 FR-112 约束」。
// 首版全库扫就误报了它；一个会误报的守卫早晚会被当噪音关掉，比没有更糟。
var ledgerTables = []string{
	"requests", "attempts", "attempt_usage", "session_prefix_ledger", "ledger_outbox",
}

// TestNoForbiddenColumnsInLedger 是 FR-112 的机器守卫（#11 三道守卫之一）。
// 放在单测里而非只在 CI：本地 make test 就能挡住，不必等 CI 往返。
func TestNoForbiddenColumnsInLedger(t *testing.T) {
	ms, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, m := range ms {
		// 去注释再扫——注释里提到"不存 body"是合法的
		all += regexp.MustCompile(`--.*`).ReplaceAllString(m.SQL, "")
	}
	forbidden := regexp.MustCompile(
		`(?m)^\s+(body|messages|prompt|completion_text|headers|prompt_text)\s+[A-Za-z]`)

	for _, tbl := range ledgerTables {
		re := regexp.MustCompile(`(?s)CREATE TABLE ` + tbl + `\s*\((.*?)\n\)`)
		m := re.FindStringSubmatch(all)
		if m == nil {
			t.Errorf("找不到账本表 %s 的定义", tbl)
			continue
		}
		if loc := forbidden.FindString(m[1]); loc != "" {
			t.Errorf("账本表 %s 出现 FR-112 禁止的正文类列: %q",
				tbl, strings.TrimSpace(loc))
		}
	}
}

// TestForbiddenColumnGuardActuallyWorks 反向验证上一条守卫不是空转。
// 没有这条，"守卫存在"与"守卫有效"是两件事。
func TestForbiddenColumnGuardActuallyWorks(t *testing.T) {
	forbidden := regexp.MustCompile(
		`(?m)^\s+(body|messages|prompt|completion_text|headers|prompt_text)\s+[A-Za-z]`)
	fake := "  id UUID,\n  messages JSONB NOT NULL,\n"
	if !forbidden.MatchString(fake) {
		t.Fatal("守卫正则认不出 messages 列——它没在真正起作用")
	}
	ok := "  id UUID,\n  prompt_tokens INTEGER,\n" // prompt_tokens 是计数不是正文
	if forbidden.MatchString(ok) {
		t.Fatal("守卫误伤 prompt_tokens（token 计数是允许的，账本本就存它）")
	}
}

// TestPartitionParentsArePartitioned 账本三表必须是分区表，
// 否则保留窗口只能靠 DELETE 而非 DETACH+DROP（02 §9.1）。
func TestPartitionParentsArePartitioned(t *testing.T) {
	ms, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, m := range ms {
		all += m.SQL
	}
	for _, tbl := range []string{"requests", "attempts", "attempt_usage"} {
		re := regexp.MustCompile(`(?s)CREATE TABLE ` + tbl + `\s*\(.*?PARTITION BY RANGE`)
		if !re.MatchString(all) {
			t.Errorf("%s 应为 RANGE 分区表（02 §9.1 保留窗口靠 DETACH+DROP）", tbl)
		}
	}
}

// TestMigrateUsesBlockingLock 守住选主协议：必须用**阻塞式** pg_advisory_lock，
// 不能用 pg_try_advisory_lock。
//
// 背景（真 bug，由双实例 compose 抓到）：首版用 try 版本，取不到锁就 return nil
// 并注释"让调用方轮询就绪" —— 但没有任何调用方实现轮询，bootstrap.Run 紧接着
// 去灌种子，而抢到锁的实例还没建出 config_params。单实例测试无竞争，看不出来。
//
// 这条测试读源码而非跑库：意图是"锁的语义不许被改回 try"，而那是静态事实。
func TestMigrateUsesBlockingLock(t *testing.T) {
	src, err := os.ReadFile("migrate.go")
	if err != nil {
		t.Fatal(err)
	}
	// 去掉注释再扫 —— 上面那段说明本身就提到了 try 版本的名字。
	code := regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(string(src), "")
	if strings.Contains(code, "pg_try_advisory_lock") {
		t.Error("选主必须用阻塞式 pg_advisory_lock：try 版本的落败者会带着" +
			"未建好的 schema 继续执行，双实例启动必炸（见本测试注释）")
	}
	if !strings.Contains(code, "pg_advisory_lock") {
		t.Error("找不到 pg_advisory_lock —— 选主逻辑是否被删了？")
	}
}
