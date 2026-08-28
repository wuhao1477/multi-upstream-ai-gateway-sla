package admin

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/config"
)

// validateValue 校验新值的形态与该键的默认值一致。
//
// 为什么按默认值推断类型而不在 §4bis 加"类型"列：默认值本身已表达类型，
// 加一列就多一处可能与默认值矛盾的信息（config.inferKind 同一理由）。
//
// 这道校验的实际价值：挡住 `max_hops="三跳"` 这类改动在**写入前**失败，
// 而不是等启动时 Validate 才发现 —— 那时配置已经在库里，
// 且关键项还占了一个版本号。
func validateValue(spec config.ParamSpec, newValue string) error {
	// 空值只在"该键的默认值就是空"时合法。
	// 这不是形式主义：alert_webhook_url 为空即"不外发"（06 §5bis），
	// 运维必须能把它**改回空**来关掉外发，否则一旦配了就再也关不掉。
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
	// 数组/对象字面量原样用，不再套一层引号（balance_text_patterns 是关键词表）。
	// 与 store.toJSONLiteral 的同一分支对应。
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
