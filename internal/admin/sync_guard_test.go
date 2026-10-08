package admin

import (
	"errors"
	"testing"
	"time"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
)

// 最小间隔是给上游挡请求的。本地前置失败没打上游，不该消耗窗口 ——
// 否则「建渠道 → 采集 → 提示缺凭证 → 登记凭证 → 再采集」这条首跑路径
// 会被自己上一次的失败挡住（P1-evidence §4 第 15 项）。
func TestSyncGuardLocalFailureKeepsWindowOpen(t *testing.T) {
	g := newSyncGuard()
	const ch = int64(7)

	if err := g.acquire(ch, time.Minute); err != nil {
		t.Fatalf("首次 acquire 应成功，得到 %v", err)
	}
	g.release(ch, false) // 模拟：缺凭证，未触达上游

	if err := g.acquire(ch, time.Minute); err != nil {
		t.Fatalf("本地失败后应能立刻重试，却被挡：%v", err)
	}
	g.release(ch, true) // 这次真打了上游

	err := g.acquire(ch, time.Minute)
	if !errors.Is(err, errSyncTooSoon) {
		t.Fatalf("触达上游后应起算窗口，期望 errSyncTooSoon，得到 %v", err)
	}
}

// 窗口一旦起算，剩余秒数要出现在错误里 —— UI 直接把 error 字段贴给用户，
// 没有秒数就只能干等（不排队，见 09 §5.0bis）。
func TestSyncGuardTooSoonReportsRemaining(t *testing.T) {
	g := newSyncGuard()
	const ch = int64(1)

	_ = g.acquire(ch, time.Minute)
	g.release(ch, true)

	err := g.acquire(ch, time.Minute)
	if err == nil {
		t.Fatal("期望被限流")
	}
	if !containsDigit(err.Error()) {
		t.Fatalf("错误信息应含剩余秒数，得到 %q", err.Error())
	}
}

// 互斥与限流是两件事：release 传 false 也必须解锁，
// 否则一次本地失败会把渠道永久锁死（比多等 60 秒严重得多）。
func TestSyncGuardReleaseAlwaysUnlocks(t *testing.T) {
	g := newSyncGuard()
	const ch = int64(3)

	_ = g.acquire(ch, time.Minute)
	if err := g.acquire(ch, time.Minute); !errors.Is(err, errSyncRunning) {
		t.Fatalf("在执行中应返回 errSyncRunning，得到 %v", err)
	}
	g.release(ch, false)

	if err := g.acquire(ch, time.Minute); err != nil {
		t.Fatalf("release(false) 后仍应能 acquire，得到 %v", err)
	}
}

// 间隔为 0（关闭限流）时不应因为 last 有值而拒绝。
func TestSyncGuardZeroIntervalNeverThrottles(t *testing.T) {
	g := newSyncGuard()
	const ch = int64(9)

	_ = g.acquire(ch, 0)
	g.release(ch, true)

	if err := g.acquire(ch, 0); err != nil {
		t.Fatalf("间隔 0 应不限流，得到 %v", err)
	}
}

// 补齐用的槽位是负数，与渠道 sync 用的渠道 id（正数）不可能撞上 ——
// 撞上的表现是"采过一次渠道之后补齐预览被挡 60 秒"，而两件事毫无关系。
//
// 「同步已有 Key」原先也占一个槽位（keyImportGuardSlot），2026-09-16 改成后台
// 批次之后它不再走 guard：要防的是两批并发去花同一份上游限流预算，而那由
// keyImportQueue.begin 的"同时只跑一个"保证，比"两次点击间隔 60 秒"贴切。
func TestKeyProvisionGuardSlotDoesNotCollideWithChannels(t *testing.T) {
	g := newSyncGuard()
	if err := g.acquire(keyProvisionGuardSlot, time.Minute); err != nil {
		t.Fatalf("补齐槽位首次 acquire 应成功，得到 %v", err)
	}
	g.release(keyProvisionGuardSlot, true)

	// 任意渠道都不该被补齐的窗口挡住
	if err := g.acquire(int64(1), time.Minute); err != nil {
		t.Fatalf("渠道 sync 不应被补齐窗口阻塞，得到 %v", err)
	}
	g.release(int64(1), false)

	if err := g.acquire(keyProvisionGuardSlot, time.Minute); !errors.Is(err, errSyncTooSoon) {
		t.Fatalf("补齐槽位自身仍应遵守间隔，得到 %v", err)
	}
}

func TestMergeKeyProvisionBatchResultCountsFailureOnce(t *testing.T) {
	var batch keyProvisionBatchResult
	mergeKeyProvisionBatchResult(&batch, collector.KeyProvisionResult{
		Found: 1, Failed: 1, Created: 0,
	})
	if batch.Found != 1 || batch.Failed != 1 {
		t.Fatalf("批量失败计数 = %+v，期望 found=1/failed=1", batch)
	}
}

func containsDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}
