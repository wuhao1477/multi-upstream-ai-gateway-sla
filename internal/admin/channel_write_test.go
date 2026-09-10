package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla-public/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla-public/internal/store"
)

// 渠道写路径的守卫 —— 打**真 PG**，由 test-migrate.sh 第 8 步驱动。
//
// 为什么必须有这几条（2026-09-01 自审 + Codex 二次评审各自查到）：
// `POST /admin/channels` 的 base_url 校验从一开始就在，但 **PATCH 那条路径
// 一处校验都没有** —— 实测 `file:///etc/passwd` / 云元数据网段 / 非 URL /
// `gopher://` 四种坏值全部返 200 并落库，等于建渠道那道闸可以被一次 PATCH
// 完整绕过。107 项验收跑过 PATCH 的改名/停用/启用，**从没 PATCH 过 base_url**，
// 所以全绿。
//
// ⚠️ 关于 CLAUDE.md §1（禁 mock）：下面注入了 `Detect` 与一个必然失败的
// `SaveDetected`。判据是那条原文——**真依赖能不能按需产出这个输入**：
// 被测的是**回滚**与**校验顺序**，而真 PG 不肯只在"写探测快照"那一步报错，
// 真站点也不肯按需变成一个坏 URL。库是真的、约束是真的、事务是真的、
// HTTP 路由是真的 —— 造的只有那一个错误和一个不出网的探测结果。
func testPool(t *testing.T) *store.Pool {
	t.Helper()
	p, err := store.NewPool(context.Background(), testDSN(t))
	if err != nil {
		t.Fatalf("建连接池: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// httpServer 起一个带真库的管理平面，返回 mux 与鉴权头。
func httpServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	const tok = "chan-write-test-token"
	s := NewServer(testPool(t), tok, testServer().Logger, nil)
	mux := http.NewServeMux()
	s.UpstreamRoutes(mux)
	s.ImportRoutes(mux)
	return s, mux, tok
}

// do 发一条带令牌的请求，返回状态码与响应体。
func do(t *testing.T, h http.Handler, tok, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// baseURLOf 从库里读回该渠道当前的 base_url。
func baseURLOf(ctx context.Context, t *testing.T, conn *pgx.Conn, id int64) string {
	t.Helper()
	var got string
	if err := conn.QueryRow(ctx, `SELECT base_url FROM channels WHERE id=$1`, id).
		Scan(&got); err != nil {
		t.Fatalf("读渠道 %d 的 base_url: %v", id, err)
	}
	return got
}

// newChannel 经 HTTP 建一个渠道并返回 id（不自动探测，避免出网）。
func newChannel(t *testing.T, h http.Handler, tok, name, base string) int64 {
	t.Helper()
	code, body := do(t, h, tok, "POST", "/admin/channels",
		fmt.Sprintf(`{"name":%q,"base_url":%q,"auto_detect":false}`, name, base))
	if code != http.StatusCreated {
		t.Fatalf("建渠道应 201，得 %d：%s", code, body)
	}
	var out struct{ ID int64 }
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析建渠道响应: %v（%s）", err, body)
	}
	return out.ID
}

// TestPatchChannelRejectsBadBaseURL PATCH 必须与 POST 共用同一道地址校验。
//
// 这是 Codex 二次评审那条 [critical] 的守卫。实测过的绕过路径：POST
// file:///etc/passwd 返 400，而 PATCH 同一个值返 200 且写进库 —— 之后每轮
// sync 都会去请求它。
func TestPatchChannelRejectsBadBaseURL(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	_, h, tok := httpServer(t)
	good := "https://patch-guard.example.invalid"
	wipe(ctx, t, conn, good)
	defer wipe(context.Background(), t, conn, good)

	id := newChannel(t, h, tok, "PATCH 校验靶子", good)

	// ⚠️ 刻意**不**把 `http://169.254.169.254/…` 放进来：它是合法 http URL，
	// 拒绝云元数据/私有网段属 §3.7 明确延后的那一层。本条只断言 scheme/host
	// 这一层在 PATCH 上生效 —— 把延后的那层混进来会让这条断言在"网段校验没做"
	// 时也红，红的理由就不再是它守的那件事了。
	for _, bad := range []string{
		"file:///etc/passwd",
		"not-a-url-at-all",
		"gopher://x",
		"https://",
	} {
		code, body := do(t, h, tok, "PATCH", fmt.Sprintf("/admin/channels/%d", id),
			fmt.Sprintf(`{"base_url":%q}`, bad))
		if code != http.StatusBadRequest {
			t.Errorf("PATCH base_url=%q 应 400，得 %d：%s", bad, code, body)
		}
		if got := baseURLOf(ctx, t, conn, id); got != good {
			t.Errorf("PATCH base_url=%q 被拒了，库里却已变成 %q（应仍是 %q）——\n"+
				"   后果：syncChannel 之后每轮都会向这个地址出站。", bad, got, good)
		}
	}
}

// TestChannelBaseURLNormalizedAndUnique 尾斜杠要在写入前去掉，同地址必须冲突。
//
// 两件事必须一起验：019 的唯一约束认**字面值**，所以少了"写入前去尾斜杠"，
// 一个 "/" 就能绕过它 —— 约束与规范化是同一道防线的两半，分开验会各自绿。
func TestChannelBaseURLNormalizedAndUnique(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	_, h, tok := httpServer(t)
	base := "https://uniq-guard.example.invalid"
	// 本条故意用带尾斜杠、以及大写 host 的地址发请求，一旦规范化失效（反向自验
	// 就是这么破坏的）库里会留下那几种拼法之一。**清理由 wipe 按"大小写与尾斜杠
	// 都不敏感"匹配**，所以这里只传一次 —— 原先是在这里列拼法，而每漏一种就换来
	// 一轮"红在落了 2 行上、而被测代码是好的"（两种拼法都实测踩过）。
	wipe(ctx, t, conn, base)
	defer wipe(context.Background(), t, conn, base)

	id := newChannel(t, h, tok, "唯一约束靶子", base+"/")
	if got := baseURLOf(ctx, t, conn, id); got != base {
		t.Errorf("POST 落库前未去尾斜杠：库里是 %q，应是 %q", got, base)
	}

	// 同地址再建一次 —— 要 409（冲突：去用已有那条），不是 400（改自己的请求）
	code, body := do(t, h, tok, "POST", "/admin/channels",
		fmt.Sprintf(`{"name":"重复地址","base_url":%q,"auto_detect":false}`, base))
	if code != http.StatusConflict {
		t.Errorf("同 base_url 再建应 409，得 %d：%s", code, body)
	}

	// 带尾斜杠再建一次：若写入侧漏了规范化，唯一约束拦不住它
	code, body = do(t, h, tok, "POST", "/admin/channels",
		fmt.Sprintf(`{"name":"重复地址带斜杠","base_url":%q,"auto_detect":false}`, base+"/"))
	if code != http.StatusConflict {
		t.Errorf("同 base_url（带尾斜杠）再建应 409，得 %d：%s\n"+
			"   → 规范化漏了，一个 \"/\" 就绕过 019 的唯一约束", code, body)
	}
	// 大写 host 再建一次：DNS 主机名大小写不敏感，这是同一个上游。
	// Codex 2026-09-01 [medium]：019 的约束比**字面值**，所以少了"写入前小写 host"
	// 这两个字符串都能插进去，各带自己的账号与凭证 —— 台账里一个上游两行渠道，
	// 而库里没有 DELETE 渠道的入口。
	code, body = do(t, h, tok, "POST", "/admin/channels",
		fmt.Sprintf(`{"name":"重复地址大写host","base_url":%q,"auto_detect":false}`,
			strings.ToUpper(base)))
	if code != http.StatusConflict {
		t.Errorf("同 base_url（大写 host）再建应 409，得 %d：%s\n"+
			"   → 写入前没小写 host，一个大写字母就绕过 019 的唯一约束", code, body)
	}
	// 首尾带空白再建一次 —— 也要 409。守的是规范化的第三件事（去空白）。
	// 原先它没有任何断言：反向自验里把 TrimSpace 去掉，8 条全绿 —— 那是"三份
	// 规范化各不相同"里唯一没被测到的那半。
	// ⚠️ 去掉 TrimSpace 的后果**不是**带空格的地址落库（`url.Parse` 认不出
	// " https" 的 scheme，会被判 400），而是**同一个输入在两个阶段得到两种判定**：
	// 导入侧探测那头 TrimSpace 过、放行并真的发出了请求，落库那头没 TrimSpace、
	// 判 400 → 条目变成 failed 且理由是"须以 http:// 开头"，而它刚被探测成功。
	code, body = do(t, h, tok, "POST", "/admin/channels",
		fmt.Sprintf(`{"name":"重复地址带空白","base_url":%q,"auto_detect":false}`,
			"  "+base+"  "))
	if code != http.StatusConflict {
		t.Errorf("同 base_url（首尾带空白）再建应 409，得 %d：%s\n"+
			"   → 规范化没去首尾空白：同一个地址在校验与落库两处会得到不同判定",
			code, body)
	}
	var n int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM channels WHERE lower(base_url) IN ($1,$2)`, base, base+"/").
		Scan(&n); err != nil {
		t.Fatalf("数渠道: %v", err)
	}
	if n != 1 {
		t.Errorf("同一个地址落了 %d 行，应恰好 1 行", n)
	}
}

// TestCreateChannelRollsBackWhenSaveDetectedFails 探测结果落不进库就不该建出渠道。
//
// Codex 二次评审那条 [high] 的守卫：原先 CreateChannel 先提交、SaveDetected
// 失败只 Warn + 塞一个 warning_persist，接口照样 201 —— 于是一次瞬时库错误
// 就留下"看起来建成功、实际永远采不了"的渠道（缺 quota_per_unit 时
// FetchAccount 直接报错），而重试 POST 会再建一条而不是修好它。
func TestCreateChannelRollsBackWhenSaveDetectedFails(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	s, h, tok := httpServer(t)
	base := "https://atomic-create.example.invalid"
	wipe(ctx, t, conn, base)
	defer wipe(context.Background(), t, conn, base)

	// 注入不出网的探测：被测的是回滚，探测本身不是被测对象（CLAUDE.md §1 例外判据）
	s.Detect = func(context.Context, string) (collector.DetectResult, error) {
		return detected(), nil
	}
	boom := errors.New("刻意失败：模拟探测快照写入报错")
	s.SaveDetected = func(context.Context, store.DBTX, int64, collector.DetectResult) error {
		return boom
	}

	code, body := do(t, h, tok, "POST", "/admin/channels",
		fmt.Sprintf(`{"name":"原子性靶子","base_url":%q,"auto_detect":true}`, base))
	if code == http.StatusCreated {
		t.Errorf("探测快照落库失败，接口却返 201：%s", body)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM channels WHERE base_url=$1`, base).
		Scan(&n); err != nil {
		t.Fatalf("数渠道: %v", err)
	}
	if n != 0 {
		t.Errorf("回滚不彻底：库里留了 %d 行渠道，应为 0\n"+
			"   半成品的后果：缺 quota_per_unit → 每轮采集报错；重试 POST 会再建一条；"+
			"而库里没有 DELETE 渠道的入口。", n)
	}
}

// TestImportValidatesBeforeProbing 坏 site_url 不该被探测 —— 校验必须在出站之前。
//
// 断言的是 **Detect 一次都没被调用**，而不是"报告里标了 skipped"：
// 后者在"先探测、探完再拒"的实现下**也会绿**，而 SSRF 关心的恰是那次出站请求。
// 实测过原实现：喂 http://127.0.0.1:9/probe，报告回来的 reason 是
// "connection refused" —— 请求真的发出去了。dry_run=true 更是完全走不到落库校验。
func TestImportValidatesBeforeProbing(t *testing.T) {
	testDSN(t) // 与其余几条同批跑；本条不写库，但要求同样的运行条件
	s, h, tok := httpServer(t)

	var probed []string
	s.Detect = func(_ context.Context, u string) (collector.DetectResult, error) {
		probed = append(probed, u)
		return detected(), nil
	}

	body := `{"accounts":{"accounts":[
      {"site_name":"坏-file","site_url":"file:///etc/passwd","site_type":"newapi"},
      {"site_name":"坏-非URL","site_url":"not-a-url-at-all","site_type":"newapi"},
      {"site_name":"坏-无host","site_url":"https://","site_type":"newapi"}]}}`
	code, resp := do(t, h, tok, "POST", "/admin/import/all-api-hub?dry_run=true", body)
	if code != http.StatusOK {
		t.Fatalf("dry_run 应 200，得 %d：%s", code, resp)
	}
	if len(probed) != 0 {
		t.Errorf("坏地址被探测了 %d 次：%v\n"+
			"   → 校验跑在 Detect 之后，出站请求已经发出去了（拒绝落库拦不住它）。",
			len(probed), probed)
	}
	var out struct {
		Items []struct{ Status, Reason string }
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("解析导入报告: %v（%s）", err, resp)
	}
	if len(out.Items) != 3 {
		t.Fatalf("报告应有 3 条，得 %d", len(out.Items))
	}
	for i, it := range out.Items {
		if it.Status != "skipped" {
			t.Errorf("第 %d 条状态 %q，应为 skipped", i+1, it.Status)
		}
		if !strings.Contains(it.Reason, "不能用于采集") {
			t.Errorf("第 %d 条 reason 未说明是地址被拒：%q", i+1, it.Reason)
		}
	}
}
