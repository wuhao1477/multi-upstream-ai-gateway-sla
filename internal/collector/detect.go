package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// probeMaxBytes 限制探测响应读取量。
// 探测打的是陌生站点的公开端点，可能返回整个 HTML 首页（甚至更大）；
// 不限量会让一次探测把内存吃掉。判定只需要头部若干字段，1MB 足够。
const probeMaxBytes = 1 << 20

// Detect 按注册表顺序探测站型，命中即停（04 §2）。
//
// 探测步骤本身在 registry.go 的每份 Registration 里（ProbePath/Match/Extract）——
// 原先这里有一张与之平行的 detectSteps 表，是"加一个站型要改九处"里的第 2 处。
//
// 全部公开端点、零成本，可对 ~20 个上游批量跑一次自动分桶（AC-28）。
// 全未命中返回 FamilyUnknown —— 按 04 §7 走未知家族接入流程，
// **不猜、不 fallback 到某个家族**：猜错会让后续所有字段映射都错。
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func Detect(ctx context.Context, hc httpDoer, baseURL string) (DetectResult, error) {
	return detectWith(ctx, hc, baseURL, All())
}

// detectWith 是 Detect 的本体，注册表由参数传入。
//
// 分出这一层只为一件事：让"探测循环把原始字节交给 Match"可被单测
// （TestDetectPassesRawBodyToMatch）。现役两族的指纹都在顶层 JSON 字段里、
// 都不读 raw，若只能经 All() 探测，这条通路就没有任何断言看着它 ——
// 而它正是自研站接入要靠的那条（registry.go 的 Match 注释）。
// 用参数而不是在测试里改全局 registrations：改全局会泄漏给同包其它测试，
// 而那些测试正是靠遍历 All() 来断言"每族都合规"的。
func detectWith(ctx context.Context, hc httpDoer, baseURL string, regs []*Registration) (DetectResult, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	base := strings.TrimRight(baseURL, "/")
	res := DetectResult{Family: FamilyUnknown}

	var lastErr error
	for _, reg := range regs {
		url := base + reg.ProbePath
		m, raw, err := getJSON(ctx, hc, url)
		if err != nil {
			// 单步失败不中断：探测的本意就是"试试是不是这一族"，
			// 404/超时都是正常的否定答案。只在全部失败时报最后一个错。
			lastErr = err
			continue
		}
		if !reg.Match(m, raw) {
			continue
		}
		res.Family = reg.Family
		res.Meta = NewAPIMeta(url, time.Now())
		if reg.Extract != nil {
			reg.Extract(m, &res)
		}
		return res, nil
	}

	if lastErr != nil {
		// 全未命中且有网络错误：区分"站点不可达"与"站点可达但不属已知家族"。
		// 前者是运维问题（URL 错/站点挂了），后者要写新适配器（04 §7）。
		return res, fmt.Errorf("未匹配任何已知站型，且探测请求有失败: %w", lastErr)
	}
	return res, nil
}

// getJSON 取一个 JSON 响应。
func getJSON(ctx context.Context, hc httpDoer, url string) (map[string]any, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, newHTTPError(resp,
			fmt.Sprintf("%s 返回 %d", url, resp.StatusCode), nil)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, probeMaxBytes))
	if err != nil {
		return nil, nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		// 非 JSON（如 HTML 首页）不是错误，只是"不属这一族"的一种表现。
		// **仍把原文回传**：自研站的指纹可能压根不在 JSON 里（实测过一例，
		// 指纹在 JWT 的 iss 声明里）。这里 return nil, nil 会让那种站永远
		// 探测不到，而现役两族一切正常 —— 故有 TestDetectPassesRawBodyToMatch。
		return nil, raw, nil
	}
	return m, raw, nil
}

// unwrapData 剥掉 {code,message,data} envelope。
//
// Sub2API 系用 envelope（04 §2），NewAPI 系的 /api/status 也是
// {success,message,data} 形态。两者都要看 data 里面。
func unwrapData(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	if d, ok := m["data"].(map[string]any); ok {
		return d
	}
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

func floatOf(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	}
	return 0
}
