package config

import "testing"

// TestParamsMatchDoc 守住"生成物与文档同步"这条线。
// 数量断言是刻意的：文档加键忘了跑 make gen-params 时，这条会红。
func TestParamsMatchDoc(t *testing.T) {
	if len(Params) != 72 {
		t.Fatalf("配置键数 = %d，期望 72（09 §4bis）。改了文档请跑 make gen-params", len(Params))
	}
	seen := map[string]bool{}
	for _, p := range Params {
		if p.Key == "" {
			t.Error("存在空键名")
		}
		if seen[p.Key] {
			t.Errorf("键重复: %s", p.Key)
		}
		seen[p.Key] = true
		if p.Default == "" {
			t.Errorf("键 %s 无默认值——§4bis 要求每项都有出厂默认值（FR-115）", p.Key)
		}
	}
}

// TestP1KeysPresent 断言 P1 实际要读的键都在清单里。
//
// 这三个键是第 45 轮才补进文档的：AC-40 早就声称 catalog_missing_rounds
// "可配、默认 3"，但键从未登记 → 运行时会静默取 0 → 每轮都判模型下架。
func TestP1KeysPresent(t *testing.T) {
	want := map[string]string{
		"catalog_missing_rounds":          "3",
		"collector_catalog_interval_h":    "12",
		"sync_min_interval_s":             "60",
		"collector_request_interval_ms":   "200",
		"collector_price_interval_h":      "6",
		"collector_keyquota_interval_min": "30",
	}
	for k, def := range want {
		s, ok := Spec(k)
		if !ok {
			t.Errorf("P1 需要的键 %s 不在 §4bis 清单里", k)
			continue
		}
		if s.Default != def {
			t.Errorf("键 %s 默认值 = %q，期望 %q", k, s.Default, def)
		}
	}
}

// TestSnapshotFallsBackToDocDefault 缺键必须回落到文档默认值，不是零值。
func TestSnapshotFallsBackToDocDefault(t *testing.T) {
	snap := NewSnapshot(map[string]string{}) // 空库
	got, err := snap.Int("catalog_missing_rounds")
	if err != nil {
		t.Fatalf("读 catalog_missing_rounds: %v", err)
	}
	if got != 3 {
		t.Fatalf("空库时 catalog_missing_rounds = %d，期望回落到 3。"+
			"取 0 会让每轮采集都判模型下架（02 §1.3bis）", got)
	}
}

// TestSnapshotPrefersDBValue 库中有值时用库值。
func TestSnapshotPrefersDBValue(t *testing.T) {
	snap := NewSnapshot(map[string]string{"sync_min_interval_s": "120"})
	got, err := snap.Int("sync_min_interval_s")
	if err != nil {
		t.Fatal(err)
	}
	if got != 120 {
		t.Fatalf("sync_min_interval_s = %d，期望 120", got)
	}
}

// TestUnknownKeyIsError 清单外的键必须报错而不是给零值（09 §3 白名单语义）。
func TestUnknownKeyIsError(t *testing.T) {
	snap := NewSnapshot(nil)
	if _, err := snap.Int("no_such_key"); err == nil {
		t.Fatal("未知键应返回 error，否则 /admin/config 无从拒绝表外键")
	}
	if _, ok := Spec("no_such_key"); ok {
		t.Fatal("Spec 对未知键应返回 ok=false")
	}
}

// TestValidateCatchesBadValue 非法值必须在启动时被发现。
func TestValidateCatchesBadValue(t *testing.T) {
	if err := NewSnapshot(nil).Validate(); err != nil {
		t.Fatalf("全默认值的快照应通过校验: %v", err)
	}
	bad := NewSnapshot(map[string]string{"max_hops": "三跳"})
	if err := bad.Validate(); err == nil {
		t.Fatal("非法整数值应被 Validate 捕获——否则等到热路径才失败")
	}
}

// TestCriticalKeysFlagged 关键项数量守住 FR-115 的二次确认范围。
func TestCriticalKeysFlagged(t *testing.T) {
	n := 0
	for _, p := range Params {
		if p.Critical {
			n++
		}
	}
	if n != 12 {
		t.Errorf("关键项 = %d，期望 12（§4bis 的 ✅ 标记）", n)
	}
}
