package store

import (
	"testing"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/config"
)

func TestJSONToScalar(t *testing.T) {
	cases := []struct {
		raw, want string
	}{
		{`3`, "3"},
		{`0.05`, "0.05"},
		{`262144`, "262144"},
		{`true`, "true"},
		{`false`, "false"},
		{`"sla-gold"`, "sla-gold"},
		{`"1h"`, "1h"},
	}
	for _, c := range cases {
		got, err := jsonToScalar([]byte(c.raw))
		if err != nil {
			t.Errorf("jsonToScalar(%s): %v", c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("jsonToScalar(%s) = %q，期望 %q", c.raw, got, c.want)
		}
	}
}

// 小数必须原样保留字面量，不能过一遍 float64 —— 那会引入表示误差
// （0.05 可能变成 0.05000000000000001，而它是计费容差阈值）。
func TestJSONToScalarPreservesDecimalLiteral(t *testing.T) {
	got, err := jsonToScalar([]byte(`0.05`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "0.05" {
		t.Fatalf("小数字面量被改写为 %q —— 应原样保留 0.05", got)
	}
}

// 对象/数组类值一期不存在；出现即数据错误，必须报错而非静默 Sprint。
func TestJSONToScalarRejectsComposite(t *testing.T) {
	for _, raw := range []string{`{"a":1}`, `[1,2]`, `null`} {
		if _, err := jsonToScalar([]byte(raw)); err == nil {
			t.Errorf("jsonToScalar(%s) 应报错", raw)
		}
	}
}

// 往返一致性：种子写进去的字面量，读出来必须与文档默认值相同。
// 这条把 seed 与 config_read 两侧的转换绑在一起 —— 任一侧改了都会红。
func TestSeedReadRoundTrip(t *testing.T) {
	for _, p := range config.Params {
		lit := toJSONLiteral(p.Default)
		got, err := jsonToScalar([]byte(lit))
		if err != nil {
			t.Errorf("键 %s: 种子写 %q，读回失败: %v", p.Key, lit, err)
			continue
		}
		if got != p.Default {
			t.Errorf("键 %s 往返不一致: 默认 %q → 写 %q → 读 %q",
				p.Key, p.Default, lit, got)
		}
	}
}
