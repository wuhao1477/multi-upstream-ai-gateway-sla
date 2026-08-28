package admin

import (
	"strings"
	"testing"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/config"
)

func spec(key string) config.ParamSpec {
	s, ok := config.Spec(key)
	if !ok {
		panic("测试引用了不存在的键: " + key)
	}
	return s
}

func TestValidateInt(t *testing.T) {
	s := spec("max_hops") // 默认 3
	if err := validateValue(s, "5"); err != nil {
		t.Errorf("合法整数被拒: %v", err)
	}
	if err := validateValue(s, "三跳"); err == nil {
		t.Error("非整数应被拒")
	}
	if err := validateValue(s, "-1"); err == nil {
		t.Error("负值应被拒——时长/次数类配置语义上不可能为负")
	}
	if err := validateValue(s, ""); err == nil {
		t.Error("空值应被拒")
	}
}

func TestValidateBool(t *testing.T) {
	s := spec("data_policy_enabled") // 默认 false
	for _, v := range []string{"true", "false"} {
		if err := validateValue(s, v); err != nil {
			t.Errorf("validateValue(%q): %v", v, err)
		}
	}
	for _, v := range []string{"1", "yes", "TRUE"} {
		if err := validateValue(s, v); err == nil {
			t.Errorf("布尔项应只接受 true/false，%q 被放行了", v)
		}
	}
}

// 比例项防手滑：把 5% 写成 5 会让测活预算变成月费用的 500%。
func TestValidateRatioCatchesPercentMistake(t *testing.T) {
	s := spec("probe_global_cost_cap_ratio") // 默认 0.02
	if err := validateValue(s, "0.05"); err != nil {
		t.Errorf("合法比例被拒: %v", err)
	}
	err := validateValue(s, "5")
	if err == nil {
		t.Fatal("比例项收到 5 应报错——极可能是把 5% 写成了 5")
	}
	if !strings.Contains(err.Error(), "百分数") {
		t.Errorf("错误信息应提示可能的百分数误写，实际: %v", err)
	}
}

func TestValueToJSON(t *testing.T) {
	cases := []struct{ key, in, want string }{
		{"max_hops", "5", "5"},
		{"probe_global_cost_cap_ratio", "0.05", "0.05"},
		{"data_policy_enabled", "true", "true"},
	}
	for _, c := range cases {
		if got := valueToJSON(spec(c.key), c.in); got != c.want {
			t.Errorf("valueToJSON(%s, %q) = %q，期望 %q", c.key, c.in, got, c.want)
		}
	}
}

// FR-115 要求"明示影响"。关键项的提示必须点明它是关键项。
func TestImpactHintFlagsCritical(t *testing.T) {
	crit := spec("probe_global_cost_cap_ratio")
	h := impactHint(crit, "0.02", "0.01")
	if !strings.Contains(h, "关键项") {
		t.Errorf("关键项提示应含'关键项'字样: %s", h)
	}
	if !strings.Contains(h, "降低") || !strings.Contains(h, "50%") {
		t.Errorf("应给出方向与幅度（0.02→0.01 即降低 50%%）: %s", h)
	}

	normal := spec("max_hops")
	if strings.Contains(impactHint(normal, "3", "4"), "关键项") {
		t.Error("非关键项不该标成关键项")
	}
}

// 可修改键的默认值都必须能通过自身校验 —— 否则种子写进去的值
// 反而无法经 API 再次提交，形成"默认值非法"的荒谬状态。
// EnvSourced 项排除在外：它们本就不可经 API 修改（见下一条测试）。
func TestAllDefaultsPassOwnValidation(t *testing.T) {
	for _, p := range config.Params {
		if p.EnvSourced {
			continue
		}
		if err := validateValue(p, p.Default); err != nil {
			t.Errorf("键 %s 的默认值 %q 通不过自身校验: %v", p.Key, p.Default, err)
		}
	}
}

// EnvSourced 键不可经 API 修改 —— admin_token 可改就等于能改自己的鉴权令牌。
func TestValidateRejectsEnvSourced(t *testing.T) {
	s := spec("admin_token")
	if !s.EnvSourced {
		t.Fatal("admin_token 应标 EnvSourced")
	}
	err := validateValue(s, "hunter2")
	if err == nil {
		t.Fatal("EnvSourced 键应拒绝修改（否则管理 API 能改自己的鉴权令牌）")
	}
	if !strings.Contains(err.Error(), "环境变量") {
		t.Errorf("错误应说明原因，实际: %v", err)
	}
}

// 种子与 API 写同一列，转换规则必须一致 —— 否则"经 API 改过的值"与
// "种子写的值"在读取侧表现不同。用数组类键验最容易出分歧的那个分支。
func TestValueToJSONMatchesSeedRuleForArray(t *testing.T) {
	s := spec("balance_text_patterns")
	arr := `["余额","欠费"]`
	if got := valueToJSON(s, arr); got != arr {
		t.Fatalf("数组应原样用作 JSONB 字面量，得到 %q —— "+
			"再套一层引号会让读取侧拿到字符串而非数组", got)
	}
}

// 空值规则：默认为空的键可被设回空（否则 webhook 配了就关不掉），
// 默认非空的键不接受空值。
func TestEmptyValueRules(t *testing.T) {
	webhook := spec("alert_webhook_url") // 默认 ""
	if err := validateValue(webhook, ""); err != nil {
		t.Errorf("alert_webhook_url 应可设回空以关闭外发（06 §5bis）: %v", err)
	}
	if err := validateValue(webhook, "https://hook.example/x"); err != nil {
		t.Errorf("合法 URL 被拒: %v", err)
	}

	hops := spec("max_hops") // 默认 3
	if err := validateValue(hops, ""); err == nil {
		t.Error("默认非空的键不应接受空值")
	}
}
