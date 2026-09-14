package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 退出标准③「Key 明文在响应/日志/抓包中一处都不出现」的**唯一 CI 可跑守卫**。
//
// 为什么需要它（2026-08-29 发版复评查出）：那条退出标准此前有三个验证点，
// **没有一个能在 CI 里跑**：
//   · 整份 DOM grep（verify-ui.mjs）—— 在需真上游的 58 项里。那个门禁是真的，
//     不是继承来的：登记 Key 前要先登记账号，而 `#acc-uid` 必须是**真的**上游
//     用户 ID（SaveAccount 先按 external_user_id 匹配，填假 uid 会走兜底分支，
//     匹配逻辑本身就没被验到）；`#key-ref` 同样要真实存在于上游 /api/token。
//   · core 日志无明文（ui-stack.sh 末步）—— 同一条门禁。
//   · 真库 DOM grep（verify-remote.mjs）—— 需内网真库。
// 而 #9 的完成标准原文写着「CI 脱敏断言覆盖新端点」。那一条从未落地：
// 三个验证点都在本地，CI 侧**零覆盖**。
//
// 本测试补的是其中**不需要任何真依赖**的那一层：源码级断言"读路径不 SELECT 明文"。
// 它挡不住"取了前缀又在别处打日志"，但能挡住这道防线的**根**被拆掉 ——
// 而那正是最容易在重构里静默发生的一种：把 `left(secret,8)||'…'` 改成 `secret`
// 只是一次"简化"，本地不跑那 58 项就完全看不出来。
//
// ⚠️ 不用真库：这三条全是对**源码文本**的断言，故属 CLAUDE.md §1 允许的形态 ——
//    被测对象就是源码本身，不是"假的数据库替真数据库作证"。真库那一侧的举证
//    仍归 verify-remote.mjs 的三条 DOM 断言（跑在 channel 1 夹具上，见其注释）。

// readSource 读 internal/store 与 internal/admin 的全部非测试 .go 源码。
func readSource(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range []string{".", "../admin"} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("读目录 %s: %v", dir, err)
		}
		for _, e := range ents {
			n := e.Name()
			if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, n))
			if err != nil {
				t.Fatalf("读 %s: %v", n, err)
			}
			out[filepath.Join(dir, n)] = string(b)
		}
	}
	if len(out) < 5 {
		t.Fatalf("只读到 %d 个源文件 —— 路径错了？（这会让下面三条断言全部空转）", len(out))
	}
	return out
}

// stripComments 去掉行注释与字符串外的说明文字。
//
// 必须去：本文件与 channels.go 的注释里大量出现 "secret"、"明文" 这些词，
// 不去掉的话断言会对着注释报警，而一个会误报的守卫早晚被当噪音关掉
// （同 ledgerTables 那处 probe_templates.body 的教训）。
func stripComments(src string) string {
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, "")
	return regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(src, "")
}

// sqlLiterals 取出源码里的**反引号原始串**，并把 `…` + 常量 + `…` 的拼接接回一条。
//
// ⚠️ 必须先切成一条条字面量再匹配，不能直接对整份源码跑
// `(?is)SELECT (.*?) FROM upstream_keys`：`.*?` 在 dotall 下会**跨语句**匹配 ——
// 文件前面一条 `SELECT … FROM channels` 会一路吃到后面那条 `FROM upstream_keys`，
// 把整个文件当成"列表"报出来。第一版就是这样，错误信息有 6KB 且指错了文件。
//
// 拼接要接回来，因为 ListKeys 正是 `SELECT …, ` + secretPrefixExpr + `, … FROM upstream_keys`
// —— 三段字面量各自都不完整：第一段有 SELECT 没 FROM，第三段有 FROM 没 SELECT。
// 不接回去，这条查询会整个逃过检查。
func sqlLiterals(src string) []string {
	// 先把 `…` + ident + `…` 里的 ident 换成它的常量值（只认本包**显式列出**的这几个）。
	//
	// 逐个列而不是反射查全包常量：这份名单就是"我确认过这段 SQL 里没有明文列"
	// 的记录。加一个新的拼接常量时必须来这里加一行 —— 那一刻正是该确认的时候。
	for _, kv := range []struct{ ident, value string }{
		{"secretPrefixExpr", secretPrefixExpr},
		// 账号默认分组的 LEFT JOIN，只碰 channel_groups，不选任何 upstream_keys 列。
		{"accountGroupJoin", accountGroupJoin},
		// 动态倍率区间的两个子查询，只读 channel_groups，不选任何 upstream_keys 列。
		{"dynamicRateRange", dynamicRateRange},
	} {
		src = strings.ReplaceAll(src, "`+"+kv.ident+"+`", kv.value)
		src = strings.ReplaceAll(src, "` + "+kv.ident+" + `", kv.value)
	}
	// 还剩别的 `+ident+` 拼接就留个显式标记 —— 未知拼接不能当成"没问题"。
	src = regexp.MustCompile("`\\s*\\+\\s*(\\w+)\\s*\\+\\s*`").
		ReplaceAllString(src, "UNRESOLVED_$1")
	var out []string
	for _, m := range regexp.MustCompile("(?s)`([^`]*)`").FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	return out
}

// selectRe 在**单条**字面量内匹配 SELECT … FROM upstream_keys 的选出列。
var selectRe = regexp.MustCompile(`(?is)SELECT\s+(.*?)\s+FROM\s+upstream_keys`)

// TestKeyReadPathNeverSelectsPlaintext 读路径不得 SELECT 明文 secret。
//
// 判据是"选出来的列里有没有裸 secret"，不是"源码里有没有 secret 这个词"：
// 后者会把 INSERT/UPDATE（写入必须用明文列）与变量名一起误报。
func TestKeyReadPathNeverSelectsPlaintext(t *testing.T) {
	srcs := readSource(t)
	found, unresolved := 0, 0
	for path, raw := range srcs {
		for _, lit := range sqlLiterals(stripComments(raw)) {
			if strings.Contains(lit, "UNRESOLVED_") && strings.Contains(lit, "upstream_keys") {
				unresolved++
				t.Errorf("%s：upstream_keys 的查询里有本测试解析不了的字面量拼接 —— "+
					"未知拼接不能当成没问题，请把它写成常量或直接内联：%.120q", path, lit)
			}
			m := selectRe.FindStringSubmatch(lit)
			if m == nil {
				continue
			}
			found++
			cols := m[1]
			// 允许 left(secret,…)、length(secret) 这类**不还原明文**的用法；
			// 禁的是把 secret 本身作为一个选出列。
			//
			// ⚠️ 必须认**带限定名**的形态（`k.secret`、`upstream_keys.secret`）。
			//    第一版写的是 `(^|[\s,(])secret([\s,)]|$)` —— 要求 secret 前面是
			//    空白/逗号/左括号，于是 `k.secret` 前面是个点，一个字都匹配不到。
			//    而 JOIN 查询里**带限定名才是常态**：反向自验 A（把
			//    `+secretPrefixExpr+` 换成 `k.secret`）当场证明这条守卫是空的。
			bare := regexp.MustCompile(`(?i)(^|[\s,(])(\w+\.)?secret([\s,)]|$)`)
			// 先挖掉所有 f(secret) / f(k.secret) 形式的调用，剩下的才是裸列
			masked := regexp.MustCompile(`(?i)\w+\s*\(\s*(\w+\.)?secret\b[^)]*\)`).
				ReplaceAllString(cols, "MASKED")
			if bare.MatchString(masked) {
				t.Errorf("%s：读路径把明文 secret 选了出来 —— 退出标准③要求"+
					"「响应/日志/抓包一处都不出现」。列表：%q\n"+
					"   脱敏必须在 SQL 侧（secretPrefixExpr = %s），"+
					"完整 secret 不出库才能少一处被日志带出的路径。",
					path, strings.TrimSpace(cols), secretPrefixExpr)
			}
		}
	}
	if found == 0 {
		t.Fatal("一条 SELECT … FROM upstream_keys 都没找到 —— 正则失效了？" +
			"（这会让本断言变成永远绿的空壳）")
	}
	t.Logf("扫过 %d 条 upstream_keys 读查询（未解析拼接 %d 处）", found, unresolved)
}

// TestSecretPrefixExprTruncates 脱敏表达式必须真的截断，且不是原样返回。
//
// 没有这条，`secretPrefixExpr = "secret"` 会让上一条测试全绿 ——
// 它检查的是"有没有裸 secret 出现在列里"，而常量替换后那个位置是个标识符，
// 上一条看到的是 `secretPrefixExpr` 这个 Go 变量名，不是 SQL 文本。
func TestSecretPrefixExprTruncates(t *testing.T) {
	if !strings.Contains(secretPrefixExpr, "left(") {
		t.Errorf("secretPrefixExpr = %q，应当用 left() 截断", secretPrefixExpr)
	}
	if !regexp.MustCompile(`left\(\s*secret\s*,\s*\d+\s*\)`).MatchString(secretPrefixExpr) {
		t.Errorf("secretPrefixExpr = %q，未见 left(secret, N) 形态", secretPrefixExpr)
	}
	// 截断长度必须远小于真实 Key 长度：真库那两把是 40 / 21 字符。
	//
	// 上界取 8（= 09 的设计值，列表只显示 secret_prefix）。**放宽要改这条断言**，
	// 于是"回显变长"必然是一次显式决定，而不是某次调参的副作用；收紧（<8）随便改。
	//
	// ⚠️ 第一版写的是 `len(n[1]) > 2` —— 判的是**位数**不是**取值**，于是
	//    `left(secret, 32)` 因为"32 只有两位"而通过。32 对真库那把 21 字符的 Key
	//    等于整条回显。反向自验 B2 当场证明这条守卫是空的。
	const maxPrefix = 8
	n := regexp.MustCompile(`left\(\s*secret\s*,\s*(\d+)\s*\)`).
		FindStringSubmatch(secretPrefixExpr)
	if n == nil {
		t.Fatalf("secretPrefixExpr = %q，取不出截断长度 —— 上面那条已经报过形态，"+
			"这里直接停，避免拿 nil 继续判", secretPrefixExpr)
	}
	got, err := strconv.Atoi(n[1])
	if err != nil {
		t.Fatalf("截断长度 %q 解析失败：%v", n[1], err)
	}
	if got < 1 || got > maxPrefix {
		pct := func(keyLen int) float64 {
			// 截到比 Key 还长就是整条回显，封顶 100% —— 不然会打出 "152%"
			if got >= keyLen {
				return 100
			}
			return float64(got) * 100 / float64(keyLen)
		}
		t.Errorf("截断长度取 %d，应在 1~%d —— 真库那两把 Key 是 40 / 21 字符，"+
			"取 %d 位等于回显 %.0f%% / %.0f%%，已不是「只显前缀」",
			got, maxPrefix, got, pct(40), pct(21))
	}
}

// TestAdminResponsesNeverIncludePlaintext 管理接口不得回显完整 secret。
func TestAdminResponsesNeverIncludePlaintext(t *testing.T) {
	var src string
	for name, body := range readSource(t) {
		if strings.HasPrefix(name, "../admin/") {
			src += "\n" + body
		}
	}
	if src == "" {
		t.Fatal("找不到 ../admin/*.go —— 路径错了？本断言已空转")
	}
	clean := stripComments(src)
	// 禁止 resp["secret"] = … 与 map[string]any{"secret": …} 两种响应写法。
	re := regexp.MustCompile(`(?m)(\w+\["secret"\]\s*=|\{\s*"secret"\s*:)`)
	if hits := re.FindAllString(clean, -1); len(hits) != 0 {
		t.Errorf("管理响应体里写入完整 secret 的地方有 %d 处，应为 0", len(hits))
	}
}
