package admin

import (
	"errors"
	"strings"
	"testing"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
)

// 「已停用的渠道不采集」的语义守卫。
//
// 背景：2026-08-31 放弃 20 个采不到的站时发现，`PATCH status=disabled` 之后
// 对该渠道发采集**照样打上游**（实测渠道 30 返 502 + 上游 401）。停用当时
// 只是台账上的一个字段，采集侧完全不看它。
//
// 这组用例钉住三件事，每一件都对应一个"改坏了但别处不会红"的改法：
//   - 停用必须挡住（否则放弃纳管等于没做）
//   - 消息必须带 ErrPrecondition（否则调用方返 502，运维被指去查别人家站点）
//   - 非 disabled 一律放行（否则一次手误就让全部渠道采不了，而正向断言仍绿）
func TestDisabledChannelBlocksSync(t *testing.T) {
	msg, blocked := disabledSyncMessage("disabled", "2026-08-31 放弃纳管：凭证失效")
	if !blocked {
		t.Fatal("已停用的渠道必须挡住采集 —— 不挡的话「放弃纳管」只改了台账字段，" +
			"每轮覆盖率报告仍会去打这些死站")
	}
	if !strings.Contains(msg, collector.ErrPrecondition.Error()) {
		t.Errorf("消息里必须带 ErrPrecondition，否则调用方按上游故障返 502，"+
			"运维会去查别人家站点为什么挂了；实际 %q", msg)
	}
	if !strings.Contains(msg, "凭证失效") {
		t.Errorf("停用原因必须出现在消息里，否则运维只知道「停用了」不知道为什么；实际 %q", msg)
	}
	if !strings.Contains(msg, "启用") {
		t.Errorf("消息必须说清怎么恢复（先启用），否则只能去读代码；实际 %q", msg)
	}
}

// 反向哨兵。只有正向断言的话，把判定写成 `status != "enabled"` 甚至恒 true
// 都能让上面那条继续绿 —— 而那会让**所有**渠道都采不了。
func TestNonDisabledStatusesAllSync(t *testing.T) {
	for _, st := range []string{"enabled", "", "unknown", "Disabled", "DISABLED"} {
		if _, blocked := disabledSyncMessage(st, "some reason"); blocked {
			t.Errorf("status=%q 被挡住了 —— 只有精确的 %q 才该挡。"+
				"挡多了的后果是渠道整体采不了，而正向用例照样绿", st, "disabled")
		}
	}
}

// 没写停用原因时消息不能出现空括号 —— 那种串会被运维读成"原因丢了"。
func TestDisabledWithoutReasonHasNoEmptyParens(t *testing.T) {
	msg, blocked := disabledSyncMessage("disabled", "")
	if !blocked {
		t.Fatal("没写原因也必须挡住")
	}
	if strings.Contains(msg, "（）") {
		t.Errorf("空原因不该留下空括号；实际 %q", msg)
	}
	if !strings.Contains(msg, collector.ErrPrecondition.Error()) {
		t.Errorf("仍须带 ErrPrecondition；实际 %q", msg)
	}
}

// ErrPrecondition 的语义契约：调用方靠 errors.Is 区分"没打上游"与"打了但失败"。
// 这条断言防的是有人把 ErrPrecondition 改成 fmt.Errorf 新建的错误值。
func TestErrPreconditionStillMatchesItself(t *testing.T) {
	if !errors.Is(collector.ErrPrecondition, collector.ErrPrecondition) {
		t.Fatal("ErrPrecondition 必须可被 errors.Is 认出 —— " +
			"syncChannel 与 sync.go 都靠它决定返 422 还是 502、起不起算限流窗口")
	}
}
