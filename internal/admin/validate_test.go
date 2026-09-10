package admin

import (
	"strings"
	"testing"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla-public/internal/config"
)

func spec(key string) config.ParamSpec {
	s, ok := config.Spec(key)
	if !ok {
		panic("测试引用了不存在的键: " + key)
	}
	return s
}

func TestValidateInt(t *testing.T) {
	s := spec("sync_min_interval_s")
	if err := validateValue(s, "5"); err != nil {
		t.Errorf("合法整数被拒: %v", err)
	}
	if err := validateValue(s, "三十"); err == nil {
		t.Error("非整数应被拒")
	}
	if err := validateValue(s, "-1"); err == nil {
		t.Error("负值应被拒——时长/次数类配置语义上不可能为负")
	}
	if err := validateValue(s, ""); err == nil {
		t.Error("空值应被拒")
	}
}

func TestValueToJSON(t *testing.T) {
	if got := valueToJSON(spec("sync_min_interval_s"), "90"); got != "90" {
		t.Errorf("valueToJSON(sync_min_interval_s, 90) = %q，期望 90", got)
	}
}

func TestImpactHintForP1Config(t *testing.T) {
	normal := spec("sync_min_interval_s")
	if strings.Contains(impactHint(normal, "60", "90"), "关键项") {
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

func TestEmptyValueRules(t *testing.T) {
	interval := spec("sync_min_interval_s")
	if err := validateValue(interval, ""); err == nil {
		t.Error("默认非空的键不应接受空值")
	}
}
