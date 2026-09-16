package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// NewAPI 系用 **HTTP 200 + `{"success":false,"message":"…"}`** 表达失败，
// 而不是用状态码。这一族测试盯的就是它。
//
// ⚠️ 形态**取自实测**，不是照文档或照想象写的（CLAUDE.md §1 对形态类夹具的要求）：
// 2026-09-16 用一把已失效的令牌打 api.lyjxka.top（午夜Free，new-api 系）的
// /api/user/self，七个候选用户 ID 头名**全部**返回
//
//	HTTP 200  {"message":"Unauthorized, invalid access token","success":false}
//
// 下面的 rejectionBody 就是那一行的逐字复制。
//
// 为什么用 httptest 而不是打真站点：被测的是"我方收到这个响应之后怎么办"——
// 真站点没法在你要的那一刻恰好回一个失效令牌的拒绝（而且要真做，就得拿一把
// 真失效的凭证反复打别人家站点）。属 CLAUDE.md §1 的行为类例外；形态那一半的
// 举证责任由上面那段实测记录承担。
const rejectionBody = `{"message":"Unauthorized, invalid access token","success":false}`

func rejectingNewAPI(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 关键：**200**，不是 401。整个缺陷的根源就在这里。
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(rejectionBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func rejectedSession(base string) Session {
	return Session{
		ChannelID: 1, Family: FamilyNewAPI, BaseURL: base,
		Token: "stale", UserIDHeader: "New-API-User", ExternalUserID: "1663",
		QuotaPerUnit: 500000,
	}
}

// 这是用户报的那个症状：「同步 key 的时候没有同步到某站的 key」。
//
// 旧行为：/api/token 回 200 + success:false → 没有 data → fetchAllKeyItems 直接
// return → **空列表且无错误**。上层于是记成 found=0 / status=ok，
// 界面上读起来就是"这个账号没有 Key"，而真相是"上游根本没让我们看"。
func TestNewAPIFetchKeysRejectionIsErrorNotEmptyList(t *testing.T) {
	srv := rejectingNewAPI(t)
	a := &NewAPIAdapter{C: NewClient(0)}

	keys, err := a.FetchKeys(context.Background(), rejectedSession(srv.URL))
	if err == nil {
		t.Fatalf("上游拒绝时必须报错，实际返回 %d 把 Key 且 err 为 nil —— "+
			"这正是「同步完了却一把 Key 都没有」的来源", len(keys))
	}
	if !strings.Contains(err.Error(), "Unauthorized, invalid access token") {
		t.Fatalf("错误里必须带上上游自己那句话，实际：%v", err)
	}
}

// 余额同理：取不到 quota 就当 0，会给出一个**看起来精确的错数**。
// FR-020 明说"未采集不是 0"，而这条路径正好绕过了那条纪律。
func TestNewAPIFetchAccountRejectionIsErrorNotZeroBalance(t *testing.T) {
	srv := rejectingNewAPI(t)
	a := &NewAPIAdapter{C: NewClient(0)}

	acc, err := a.FetchAccount(context.Background(), rejectedSession(srv.URL))
	if err == nil {
		t.Fatalf("上游拒绝时必须报错，实际回了一个余额 %v 的账号 —— "+
			"「有钱判没钱」就是这么来的", acc.BalanceUSD)
	}
}

// 诊断必须指向**令牌**，不能指向用户 ID 头名。
//
// 旧文案是"七个用户 ID 头名全部试探失败（04 §3.1 fan-out）: 头名 neo-api-user
// 通过但响应无预期字段"——两句都把矛头指向头名，而上游那句现成的答案
// （invalid access token）被丢掉了。运维照着它去查头名，查不出任何东西。
func TestNewAPIAuthenticateBlamesTokenNotHeaderWhenAllCandidatesRejected(t *testing.T) {
	srv := rejectingNewAPI(t)
	a := &NewAPIAdapter{C: NewClient(0)}

	_, err := a.Authenticate(context.Background(), Credential{
		ChannelID: 1, BaseURL: srv.URL, AccessToken: "stale",
		ExternalUserID: "1663", QuotaPerUnit: 500000,
	})
	if err == nil {
		t.Fatal("令牌被拒时 Authenticate 必须失败")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Unauthorized, invalid access token") {
		t.Fatalf("必须带上上游原话，否则运维没有任何线索。实际：%v", err)
	}
	if !strings.Contains(msg, "拒绝了这把采集凭证") {
		t.Fatalf("结论要指向凭证。实际：%v", err)
	}
	// 反向哨兵：不能再出现那句把人往头名上带的话
	if strings.Contains(msg, "头名") && !strings.Contains(msg, "无济于事") {
		t.Fatalf("不该再把矛头指向用户 ID 头名。实际：%v", err)
	}
}

// 头名 fan-out 本身要留着：**不是**每个 success:false 都等于"令牌废了"。
// 站点可能只拒某几个头名而认另一个 —— 那时换头名是有用的，不能因为看到一次
// 拒绝就放弃后面的候选。
func TestNewAPIAuthenticateStillFansOutWhenOnlySomeHeadersRejected(t *testing.T) {
	const good = "Veloera-User"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(good) == "" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(rejectionBody))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":1663,"quota":189502940}}`))
	}))
	t.Cleanup(srv.Close)

	a := &NewAPIAdapter{C: NewClient(0)}
	s, err := a.Authenticate(context.Background(), Credential{
		ChannelID: 1, BaseURL: srv.URL, AccessToken: "ok",
		ExternalUserID: "1663", QuotaPerUnit: 500000,
	})
	if err != nil {
		t.Fatalf("只有部分头名被拒时应继续试探并命中 %s，实际：%v", good, err)
	}
	if s.UserIDHeader != good {
		t.Fatalf("应命中 %s，实际 %s", good, s.UserIDHeader)
	}
}

// success:true 不受影响 —— 这道闸只认"存在且为 false"那一位。
// Sub2API 系用的是 {code,message,data}，没有 success，行为必须完全不变。
func TestUpstreamRejectionGateOnlyFiresOnExplicitFalse(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantErr    bool
	}{
		{name: "success:false 拦下", body: rejectionBody, wantErr: true},
		{name: "success:true 放行", body: `{"success":true,"data":{"id":1}}`, wantErr: false},
		{name: "没有 success 字段（Sub2API 形态）放行",
			body: `{"code":0,"message":"ok","data":{"id":1}}`, wantErr: false},
		{name: "success 不是布尔值就不管", body: `{"success":"yes","data":{"id":1}}`, wantErr: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := NewClient(0)
			_, _, err := c.getJSONAuth(context.Background(), rejectedSession(srv.URL), "/x")
			if tc.wantErr && err == nil {
				t.Fatal("应报错")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("不应报错，实际：%v", err)
			}
		})
	}
}

// NewAPI 系至少两种变体：列表里给脱敏串（要另打专用端点取明文）、
// 以及**列表里直接给完整明文**（且没有那个专用端点）。
//
// 形态实测（2026-09-17，四个真站点各打一次 /api/token）：
//
//	钱多多 / VVCode / JustDoWork  →  key 是 18 字符含 `*`；POST /api/token/{id}/key 回 200 + 48 字符明文
//	Agent Router（agentrouter.org）→  key 就是 48 字符完整明文；POST /api/token/{id}/key **404**
//
// 判据只能是"含不含 `*`"，不能按长度：48 是这四个站今天的长度。
func TestUsableSecretAcceptsPlainRejectsMasked(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{name: "完整明文（Agent Router 列表实测形态）",
			in: "eFYlJ7pCabcdefghijklmnopqrstuvwxyz0123456789ABCD", want: "eFYlJ7pCabcdefghijklmnopqrstuvwxyz0123456789ABCD"},
		{name: "脱敏串（钱多多列表实测形态）", in: "4lF9lpMc********", want: ""},
		{name: "空串", in: "", want: ""},
		{name: "只有空白", in: "   ", want: ""},
		{name: "首尾空白要去掉", in: "  abcdef  ", want: "abcdef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := usableSecret(tc.in); got != tc.want {
				t.Fatalf("usableSecret(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

// 列表里已经给了明文时，FetchKeys 要把它带出来 —— 带不出来的话，
// 上层只能去打那个专用端点，而 Agent Router 那种站上它是 404。
func TestNewAPIFetchKeysCarriesInlinePlaintextWhenUpstreamGivesIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":{"page":1,"page_size":100,"total":2,"items":[
			{"id":887318,"key":"PLAINTEXT-FROM-LIST-0001","group":"core","unlimited_quota":true},
			{"id":481908,"key":"masked-0001******","group":"core","unlimited_quota":true}
		]}}`))
	}))
	t.Cleanup(srv.Close)

	a := &NewAPIAdapter{C: NewClient(0)}
	keys, err := a.FetchKeys(context.Background(), rejectedSession(srv.URL))
	if err != nil {
		t.Fatalf("取 Key 列表失败: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("应取到 2 把，实际 %d", len(keys))
	}
	if keys[0].Secret != "PLAINTEXT-FROM-LIST-0001" {
		t.Fatalf("列表里给了完整明文就该带出来，实际 %q", keys[0].Secret)
	}
	if keys[1].Secret != "" {
		t.Fatal("脱敏串不能当明文带出去 —— 那会把一串带 * 的东西存进库当 Key 用")
	}
}
