package store

import (
	"testing"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla-public/internal/config"
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

// 数组类值必须支持：balance_text_patterns 是余额不足文案关键词表
// （09 §4bis / 05 §5.3），是 P3 余额信号识别的输入。
// 首版把数组一并拒了，会让配置加载在这一个键上直接失败。
func TestJSONToScalarSupportsArray(t *testing.T) {
	got, err := jsonToScalar([]byte(`["余额","额度"]`))
	if err != nil {
		t.Fatalf("数组类配置应被支持: %v", err)
	}
	if got != `["余额","额度"]` {
		t.Fatalf("数组应原样紧凑回传，得到 %q", got)
	}
	// JSONB 回读可能带空白，须归一后与文档字面量可比对
	got2, err := jsonToScalar([]byte(`[ "a" , "b" ]`))
	if err != nil {
		t.Fatal(err)
	}
	if got2 != `["a","b"]` {
		t.Fatalf("空白未归一: %q", got2)
	}
}

// 对象与 null 一期确实不存在，出现即数据错误。
func TestJSONToScalarRejectsObjectAndNull(t *testing.T) {
	for _, raw := range []string{`{"a":1}`, `null`} {
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
