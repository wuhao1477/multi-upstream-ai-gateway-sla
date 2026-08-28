package store

import (
	"testing"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/config"
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

// TestAllDefaultsProduceValidJSON 全部 72 个默认值都必须能转成合法 JSON，
// 否则种子会在那一行 INSERT 失败 —— 而那是启动路径。
func TestAllDefaultsProduceValidJSON(t *testing.T) {
	for _, p := range config.Params {
		lit := toJSONLiteral(p.Default)
		if lit == "" {
			t.Errorf("键 %s 的默认值 %q 转出空字面量", p.Key, p.Default)
		}
		// 粗校验：非数值/布尔的必须被引号包住
		switch lit {
		case "true", "false":
		default:
			if !isNumeric(lit) && (lit[0] != '"' || lit[len(lit)-1] != '"') {
				t.Errorf("键 %s 的字面量 %q 既非数值也未加引号，JSONB 会解析失败",
					p.Key, lit)
			}
		}
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
