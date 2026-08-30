package collector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxBodyBytes 限制单次响应读取量。上游站点异构，一个坏掉的端点可能
// 返回巨大 HTML；不限量会让采集器把内存吃掉。
const maxBodyBytes = 8 << 20

// Client 是采集侧的 HTTP 客户端：带站内最小间隔限速。
//
// **限速是硬要求**（04 §6）：同站点请求强制最小间隔，避免触发风控。
// 一个渠道被风控意味着整站采不到，代价远高于慢几百毫秒。
type Client struct {
	HC *http.Client
	// MinInterval 是同一 host 两次请求的最小间隔
	// （config_params 的 collector_request_interval_ms，默认 200ms）。
	MinInterval time.Duration

	mu   sync.Mutex
	last map[string]time.Time // host → 上次请求时刻
}

// NewClient 构造采集客户端。
func NewClient(minInterval time.Duration) *Client {
	return &Client{
		HC:          &http.Client{Timeout: 30 * time.Second},
		MinInterval: minInterval,
		last:        map[string]time.Time{},
	}
}

// wait 按 host 限速。
//
// 用 host 而非 channelID 作键：同一站点可能配了多个渠道（不同账号），
// 风控是按站点来的，按渠道限速等于没限。
func (c *Client) wait(ctx context.Context, host string) error {
	if c.MinInterval <= 0 {
		return nil
	}
	c.mu.Lock()
	if c.last == nil {
		c.last = map[string]time.Time{}
	}
	prev, ok := c.last[host]
	now := time.Now()
	var sleep time.Duration
	if ok {
		if d := c.MinInterval - now.Sub(prev); d > 0 {
			sleep = d
		}
	}
	// 提前占位，避免并发请求都读到同一个 prev 而一起放行
	c.last[host] = now.Add(sleep)
	// 顺手清掉已无意义的条目：占位时刻早于 now-MinInterval 的条目，
	// 再取出来算 sleep 也必然 ≤0，留着只是让 map 随"见过多少 host"单调增长。
	// 扫描代价与**清理后**的规模同阶：清理把 map 压到"最近一个间隔内活跃的 host"
	// （单渠道同步时通常 1~2 个），所以这个 O(n) 是自限的，不需要按大小设阈值。
	cutoff := now.Add(-c.MinInterval)
	for h, t := range c.last {
		if t.Before(cutoff) {
			delete(c.last, h)
		}
	}
	c.mu.Unlock()

	if sleep <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(sleep):
		return nil
	}
}

// authHeaders 构造鉴权头。
//
// **这里没有逐家族分流，是刻意的。** 三家族的差别只有"要不要带用户 ID 头"，
// 而那由会话里有没有头名决定 —— 头名只有 NewAPI 的 Authenticate 会 fan-out
// 试探出来并写进凭证（04 §3.1：只带 Authorization 会 401，且二开站改了头名）。
// 原先这里是一个 switch，它表达的是**零个变化点**：三个 case 里两个逐字相同，
// 第三个多的那两行本身已经被 if 守住了。
//
// 这一处曾是"加站型要改九处"里最坏的两处之一：漏加 case 的后果是新站型
// 一个头都不带 → 401，而编译、单测、Capabilities 声明全是绿的。
// 删掉分流之后，新站型默认就拿到 Bearer + 有则带用户 ID 头 —— 漏不掉。
func authHeaders(s Session) http.Header {
	h := http.Header{}
	// 实测 `Authorization: <token>` 与 `Bearer <token>` 均可，用后者更通用
	h.Set("Authorization", "Bearer "+s.Token)
	if s.UserIDHeader != "" && s.ExternalUserID != "" {
		h.Set(s.UserIDHeader, s.ExternalUserID)
	}
	h.Set("Accept", "application/json")
	return h
}

// getJSONAuth 带鉴权取 JSON，返回解析后的 map 与原始字节。
func (c *Client) getJSONAuth(ctx context.Context, s Session, path string) (map[string]any, []byte, error) {
	url := strings.TrimRight(s.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header = authHeaders(s)

	if err := c.wait(ctx, req.URL.Host); err != nil {
		return nil, nil, err
	}
	hc := c.HC
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("读 %s 响应: %w", path, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// 401 单列：调用方据此触发续期或人工重登（04 §5 第 2/4 层）
		return nil, raw, fmt.Errorf("%w: GET %s", ErrUnauthorized, path)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, raw, fmt.Errorf("GET %s 返回 %d: %s",
			path, resp.StatusCode, snippet(raw))
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, raw, fmt.Errorf("解析 %s 响应失败（非 JSON）: %s", path, snippet(raw))
	}
	return m, raw, nil
}

// ErrUnauthorized 是 401，供调用方区分"凭证问题"与"其它错误"。
var ErrUnauthorized = errors.New("collector: 上游返回 401")

// snippet 截取响应片段用于错误信息。
// 限长是刻意的：上游可能返回整页 HTML，全塞进 error 会污染日志。
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// ── 取值助手：上游 JSON 字段类型不稳定（数字可能是字符串），统一容错 ──

// asFloat 从任意 JSON 值取浮点。
//
// **必须容忍字符串数字**：上游 JSON 的数字类型不稳定是实测过的事
// （NewAPI 的 quota 是数字，而有的站把余额返回成 `"90.50"`）。
// 用 float64 断言会静默得到 0 —— 那意味着"余额 0"，
// 会让 selector 把一个有钱的渠道判成耗尽。
// TestAsFloatToleratesStringNumbers 钉住这条容错。
func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%g", &f); err == nil {
			return f, true
		}
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

// asString 从任意 JSON 值取字符串（数字也转成字符串，用于 ID 类字段）。
func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// ID 常以数字返回，但我们按字符串存（scope_id 是 TEXT）
		return strings.TrimSuffix(fmt.Sprintf("%.0f", t), ".0")
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}

// asBool 从任意 JSON 值取布尔。
func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	case float64:
		return t != 0
	}
	return false
}

// asSlice 取数组。
func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// asMap 取对象。
func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// jwtExpiry 读 JWT 的 exp 声明，读不出返回 false。
//
// **不验签，只读 claim。** 读的是我方自己库里已存的令牌，用途仅是决定何时
// 续期 —— 签名的意义是"上游能不能确认这是它签的"，而我方伪造自己的令牌来
// 骗自己提早续期没有任何收益。真正的判定权在上游：令牌不对它会 401。
// 反过来说这里**不能**用来做任何授权决定。
//
// 读不出就返回 false 让调用方退回"不主动续期"，**不猜默认到期时间**：
// 真库里就有两条 cred_type 写 sub2api_jwt 而内容不是三段 JWT 的凭证
// （渠道 30/31），给它们编一个到期时间会让系统按凭空的节奏刷令牌。
//
// exp 是 Unix 秒（RFC 7519 §4.1.4 的 NumericDate）。JSON 解出来是 float64，
// 大整数在 float64 里到 2^53 都是精确的，Unix 秒离那儿还很远。
func jwtExpiry(accessToken string) (time.Time, bool) {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	// JWT 用的是无填充的 base64url（RFC 7515 §2）。有些实现仍带 '='，
	// 两种都吃：只认一种会让"看着像 JWT 的令牌"静默读不出 exp。
	seg := parts[1]
	dec, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(seg, "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp *float64 `json:"exp"`
	}
	if err := json.Unmarshal(dec, &claims); err != nil || claims.Exp == nil {
		return time.Time{}, false
	}
	// exp<=0 当读不出：0 会被 time.Unix 解成 1970，于是"早已过期"，
	// 让每次采集都刷一遍。
	if *claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(*claims.Exp), 0), true
}

// dig 按路径逐层取值，任一层缺失即返回 nil。
// 上游响应嵌套深且形态不一，逐层判空会让适配器代码不可读。
func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = mm[k]
		if !ok {
			return nil
		}
	}
	return cur
}
