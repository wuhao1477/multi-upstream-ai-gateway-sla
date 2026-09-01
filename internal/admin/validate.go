package admin

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/config"
)

// validateBaseURL 校验一个将被采集器请求的上游地址，**并返回它的规范形态**。
//
// **三个入口共用这一处**：`createChannel`、`patchChannel`、批量导入
// （`importAllAPIHub` 的探测前 + `importOne` 的落库前）。
// ⚠️ 2026-08-29 那版注释写的是"两个入口" —— 那时 `patchChannel` 一处校验都没有，
// 而写注释的人（我）以为有。**"已收敛到一处"这种话必须数路由表，不能数记忆。**
//
// **校验与规范化必须是同一个函数**（2026-09-01 合并，Codex [medium]）。
// 原先是分开的：校验一处、`strings.TrimRight(…, "/")` 散在三处，其中两处还漏了
// `TrimSpace` —— 三份必然分叉，而分叉的后果落在 019 的唯一约束上（它比字面值）。
// 规范化做三件事，各有理由：
//   - 去首尾空白：**不是**为了防"带空格的地址落库"（`url.Parse` 认不出 " https"
//     的 scheme，那种输入会被判 400）。防的是**同一个输入在两个阶段得到两种判定**：
//     导入侧原先探测那头 TrimSpace 过、放行并真的发出了请求，落库那头没有、判 400 →
//     条目变成 failed 且理由是"须以 http:// 开头"，而它刚刚被探测成功；
//   - **小写 host**：DNS 主机名大小写不敏感，`https://EXAMPLE.invalid` 与
//     `https://example.invalid` 是同一个上游，而字面唯一约束拦不住它们 ——
//     两条都能插进去、各带自己的账号与凭证（Codex 本轮 [medium]）；
//   - 去尾斜杠：同理，一个 "/" 就能绕过唯一约束。
//
// **只小写 host，不动 path**：path 大小写敏感，一刀切会把两个真实不同的地址
// 判成同一个。也不动默认端口 —— `:443` 与省略在字面上不同，但真站点不会两种都给，
// 而"规范化端口"要先知道 scheme 的默认端口表，收益不抵那份表。
//
// ⚠️ **这是格式校验，不是出站策略。** loopback 与私网地址**按设计放行** ——
// 把采集器指向内网地址正是本系统的用途（真库在 <internal-db-host>；真库里那行夹具渠道
// 就是 `http://127.0.0.1:18099`，35 项验收的 3 条 Key 断言全靠它）。
// 缺的那层（拒绝 loopback/私有/链路本地/云元数据网段、自定义 Transport 在每次
// 连接前复核 DNS、CheckRedirect 拦重定向）记在
// docs/acceptance/P1-release-readiness.md §3.7，连触发条件一起：当前管理面是
// **单一 admin 令牌、无角色分级**，能改 base_url 的人已经握有配置面本身，
// 故那层加固挂到引入 RBAC 或把管理面暴露到公网时做。
// Codex 2026-09-01 三次评审按 [high] 重提了这一条，判定未改，理由同上。
func validateBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("base_url 必填")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("base_url 解析失败: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("base_url 须以 http:// 或 https:// 开头")
	}
	if u.Host == "" {
		return "", fmt.Errorf("base_url 缺主机名：%q", raw)
	}
	u.Host = strings.ToLower(u.Host)
	return strings.TrimRight(u.String(), "/"), nil
}

// validateValue 校验新值的形态与该键的默认值一致。
//
// 为什么按默认值推断类型而不在 §4bis 加"类型"列：默认值本身已表达类型，
// 加一列就多一处可能与默认值矛盾的信息（config.inferKind 同一理由）。
//
// 这道校验的实际价值：挡住 `sync_min_interval_s="三十"` 这类改动在**写入前**失败，
// 而不是等启动时 Validate 才发现 —— 那时配置已经在库里，
// 且关键项还占了一个版本号。
func validateValue(spec config.ParamSpec, newValue string) error {
	// 空值只在"该键的默认值就是空"时合法。
	// 默认值为空的字符串配置允许恢复为空；默认非空项不可清空。
	if newValue == "" {
		if spec.Default != "" {
			return fmt.Errorf("new_value 不可为空（键 %s，默认值 %q）",
				spec.Key, spec.Default)
		}
		return nil
	}
	// EnvSourced 项不落 config_params，改它没有任何效果，且 admin_token
	// 一旦可改就等于管理 API 能改自己的鉴权令牌（09 §4bis）。直接拒绝。
	if spec.EnvSourced {
		return fmt.Errorf("键 %s 由环境变量注入、不落 config_params，不可经本接口修改",
			spec.Key)
	}
	switch {
	case spec.Default == "true" || spec.Default == "false":
		if newValue != "true" && newValue != "false" {
			return fmt.Errorf("键 %s 是布尔项，值须为 true/false，收到 %q",
				spec.Key, newValue)
		}
	case isIntLiteral(spec.Default):
		n, err := strconv.Atoi(newValue)
		if err != nil {
			return fmt.Errorf("键 %s 是整型项，值 %q 不是整数", spec.Key, newValue)
		}
		if n < 0 {
			// 全部整型配置都是时长/次数/字节数，语义上不可能为负
			return fmt.Errorf("键 %s 不接受负值（收到 %d）", spec.Key, n)
		}
	case isFloatLiteral(spec.Default):
		f, err := strconv.ParseFloat(newValue, 64)
		if err != nil {
			return fmt.Errorf("键 %s 是浮点项，值 %q 不是数字", spec.Key, newValue)
		}
		if f < 0 {
			return fmt.Errorf("键 %s 不接受负值（收到 %v）", spec.Key, f)
		}
		// 比例类参数（默认值 <1）超过 1 极可能是把 5% 写成了 5
		if parseFloatOr(spec.Default, 0) < 1 && f > 1 {
			return fmt.Errorf(
				"键 %s 是比例项（默认 %s），值 %v 超过 1 —— 是否把百分数写成了整数？",
				spec.Key, spec.Default, f)
		}
	}
	return nil
}

// valueToJSON 按类型转成 JSONB 字面量，与 store.toJSONLiteral 同规则。
//
// ⚠️ 两处必须同规则：种子（store）与 API（本处）写的是同一列，
// 规则不一致会让"经 API 改过的值"与"种子写的值"在读取侧表现不同。
func valueToJSON(spec config.ParamSpec, v string) string {
	switch {
	case spec.Default == "true" || spec.Default == "false":
		return v
	case isIntLiteral(spec.Default), isFloatLiteral(spec.Default):
		return v
	}
	// 数组/对象字面量原样用，不再套一层引号；与 store.toJSONLiteral 保持一致。
	if len(v) >= 2 && (v[0] == '[' || v[0] == '{') && json.Valid([]byte(v)) {
		return v
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// impactHint 生成 FR-115 要求的"明示影响"。
//
// 一期给的是**方向性提示**而非精确预测：精确预测需要历史用量模型，
// 而那属 P3 经营闭环。方向性提示已能满足"让人知道自己在改什么"这个目的。
func impactHint(spec config.ParamSpec, cur, next string) string {
	base := fmt.Sprintf("将 %s 从 %s 改为 %s", spec.Key, cur, next)
	if !spec.Critical {
		return base + "。该项非关键，改动立即生效。"
	}
	suffix := "。**关键项**：改动需二次确认，生效后影响真实调度与花费。"

	c, cok := tryFloat(cur)
	n, nok := tryFloat(next)
	if cok && nok && c != 0 {
		pct := (n - c) / c * 100
		dir := "提高"
		if pct < 0 {
			dir = "降低"
			pct = -pct
		}
		return fmt.Sprintf("%s（%s %.0f%%）%s", base, dir, pct, suffix)
	}
	return base + suffix
}

func isIntLiteral(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

func isFloatLiteral(s string) bool {
	if isIntLiteral(s) {
		return false
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

func tryFloat(s string) (float64, bool) {
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

func parseFloatOr(s string, def float64) float64 {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return def
}
