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

// detectStep 是一次探测尝试。顺序即 04 §2 的表，**命中即停**。
type detectStep struct {
	family Family
	path   string
	// match 判定该响应是否属本家族。传入已解析的 JSON map 与原始字节 ——
	// ASXS 的判据是 JWT iss，需要看原文。
	match func(m map[string]any, raw []byte) bool
	// extract 从命中的响应里取家族特有信息（版本、无盾、额度基数）。
	extract func(m map[string]any, r *DetectResult)
}

// detectSteps 按 04 §2 的顺序定义。
//
// 顺序不可随意调整：NewAPI 的 /api/status 最通用，放前面能最快分桶；
// ASXS 的判据最特殊（JWT iss=ampmanager），放最后。
var detectSteps = []detectStep{
	{
		family: FamilyNewAPI,
		path:   "/api/status",
		// 命中特征：含 quota_per_unit / turnstile_check / checkin_enabled。
		// 用"任一存在"而非"全部存在"：二开站点会删字段，但不会全删。
		match: func(m map[string]any, _ []byte) bool {
			d := unwrapData(m)
			for _, k := range []string{"quota_per_unit", "turnstile_check", "checkin_enabled"} {
				if _, ok := d[k]; ok {
					return true
				}
			}
			return false
		},
		extract: func(m map[string]any, r *DetectResult) {
			d := unwrapData(m)
			r.Version = str(d["version"])
			// turnstile_check=true 表示开盾 → 服务端采集不可行（04 §6）
			r.NoShield = !boolOf(d["turnstile_check"])
			// ⚠️ 逐站读取，**不写死**：upstream-d.invalid 是 500000，别家不一定（04 §2）
			r.QuotaPerUnit = floatOf(d["quota_per_unit"])
		},
	},
	{
		family: FamilySub2API,
		path:   "/api/v1/settings/public",
		// 命中特征：含 site_name / turnstile_enabled，且是 {code,message,data} envelope
		match: func(m map[string]any, _ []byte) bool {
			d := unwrapData(m)
			_, hasSite := d["site_name"]
			_, hasTurnstile := d["turnstile_enabled"]
			return hasSite || hasTurnstile
		},
		extract: func(m map[string]any, r *DetectResult) {
			d := unwrapData(m)
			r.Version = str(d["version"])
			r.NoShield = !boolOf(d["turnstile_enabled"])
		},
	},
	{
		family: FamilyASXS,
		path:   "/api/public/site-config",
		// 命中特征：200 且 JWT iss 为 ampmanager（04 §2/§3.3）。
		// 站点可能不在 JSON 里直接写 iss，故也扫原文 —— 这是闭源平台的
		// 唯一稳定指纹（其命名空间与 NewAPI/Sub2API 零重叠）。
		match: func(m map[string]any, raw []byte) bool {
			if strings.Contains(string(raw), "ampmanager") {
				return true
			}
			d := unwrapData(m)
			return str(d["iss"]) == "ampmanager"
		},
		extract: func(m map[string]any, r *DetectResult) {
			r.Version = str(unwrapData(m)["version"])
			// ASXS 无 turnstile 概念；不声称无盾，留 false 由运维确认
		},
	},
}

// Detect 按 04 §2 的顺序探测站型，命中即停。
//
// 全部公开端点、零成本，可对 ~20 个上游批量跑一次自动分桶（AC-28）。
// 全未命中返回 FamilyUnknown —— 按 04 §7 走未知家族接入流程，
// **不猜、不 fallback 到某个家族**：猜错会让后续所有字段映射都错。
func Detect(ctx context.Context, hc *http.Client, baseURL string) (DetectResult, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	base := strings.TrimRight(baseURL, "/")
	res := DetectResult{Family: FamilyUnknown}

	var lastErr error
	for _, step := range detectSteps {
		url := base + step.path
		m, raw, err := getJSON(ctx, hc, url)
		if err != nil {
			// 单步失败不中断：探测的本意就是"试试是不是这一族"，
			// 404/超时都是正常的否定答案。只在全部失败时报最后一个错。
			lastErr = err
			continue
		}
		if !step.match(m, raw) {
			continue
		}
		res.Family = step.family
		res.Meta = NewAPIMeta(url, time.Now())
		if step.extract != nil {
			step.extract(m, &res)
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
func getJSON(ctx context.Context, hc *http.Client, url string) (map[string]any, []byte, error) {
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
		return nil, nil, fmt.Errorf("%s 返回 %d", url, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, probeMaxBytes))
	if err != nil {
		return nil, nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		// 非 JSON（如 HTML 首页）不是错误，只是"不属这一族"的一种表现。
		// 仍把原文回传 —— ASXS 的 ampmanager 指纹可能出现在非 JSON 响应里。
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
