package store

import (
	"encoding/json"
	"testing"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla-public/internal/config"
)

func TestToJSONLiteral(t *testing.T) {
	cases := []struct{ in, want string }{
		{"true", "true"},
		{"false", "false"},
		{"3", "3"},
		{"0.05", "0.05"},
		{"-1", "-1"},
		{"262144", "262144"},
		{"sla-gold", `"sla-gold"`},
		{`say "hi"`, `"say \"hi\""`},
		{"1h", `"1h"`}, // 带单位的不是数值，必须加引号否则 JSONB 解析失败
	}
	for _, c := range cases {
		if got := toJSONLiteral(c.in); got != c.want {
			t.Errorf("toJSONLiteral(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestAllDefaultsProduceValidJSON 全部可种子化的默认值都必须是合法 JSON，
// 否则种子会在那一行 INSERT 失败 —— 而那是启动路径。
func TestAllDefaultsProduceValidJSON(t *testing.T) {
	for _, p := range config.Params {
		if p.EnvSourced {
			continue // 不落表，其"默认值"是一句说明而非真值
		}
		lit := toJSONLiteral(p.Default)
		if lit == "" {
			t.Errorf("键 %s 的默认值 %q 转出空字面量", p.Key, p.Default)
			continue
		}
		if !json.Valid([]byte(lit)) {
			t.Errorf("键 %s 的字面量 %q 不是合法 JSON，JSONB 会拒绝入库",
				p.Key, lit)
		}
	}
}

// EnvSourced 项必须被排除在种子之外。
// admin_token 若落进 config_params，管理 API 就能改自己的鉴权令牌 = 提权
// （09 §4bis：不落 config_params，避免自己改自己）。
func TestEnvSourcedExcluded(t *testing.T) {
	var found bool
	for _, p := range config.Params {
		if p.Key == "admin_token" {
			found = true
			if !p.EnvSourced {
				t.Error("admin_token 必须标 EnvSourced —— 否则会被种子写进 config_params，" +
					"而管理 API 能改该表 = 能改自己的鉴权令牌")
			}
		}
	}
	if !found {
		t.Fatal("清单里找不到 admin_token")
	}
}

func TestIsNumeric(t *testing.T) {
	for _, s := range []string{"0", "12", "-3", "0.5", "262144"} {
		if !isNumeric(s) {
			t.Errorf("isNumeric(%q) 应为 true", s)
		}
	}
	for _, s := range []string{"", "-", ".", "1h", "1.2.3", "abc", "1e5"} {
		if isNumeric(s) {
			t.Errorf("isNumeric(%q) 应为 false", s)
		}
	}
}
