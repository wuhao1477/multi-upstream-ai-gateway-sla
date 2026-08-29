package admin

import (
	"fmt"
	"testing"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

func entry(name string, unit *string) store.CatalogEntry {
	return store.CatalogEntry{ModelName: name, BillingUnit: unit}
}

func unitPtr(s string) *string { return &s }

// TestFilterCatalogMakesEveryUnitReachable 锁住「按次那一段够得着」这件事。
//
// 为什么值得一个专门的测试：ListCatalog 是 `ORDER BY billing_unit` 分段排的，
// 而 per_1m_token 字母序在 per_call 之前。真实渠道实测 1161 条倍率 + 208 条按次，
// 界面一页 50 行 —— 按次的第一条落在第 1162 位，要翻到第 24 页才见得到。
// 于是脚注上「跨段不可直接比大小」的告警对着一个**永远只有一段**的表格说，
// 运维照着倍率段的数字去比按次段的价格，选型必然选错。
// 这个缺陷不报错、不进日志：表格渲染得好好的，只是少了一段。
func TestFilterCatalogMakesEveryUnitReachable(t *testing.T) {
	// 复刻真库 channel 3 的分布形状：倍率远多于按次，且有存量空值
	var all []store.CatalogEntry
	for i := 0; i < 1161; i++ {
		all = append(all, entry(fmt.Sprintf("mult-%d", i), unitPtr("per_1m_token")))
	}
	for i := 0; i < 208; i++ {
		all = append(all, entry(fmt.Sprintf("call-%d", i), unitPtr("per_call")))
	}
	for i := 0; i < 12; i++ {
		all = append(all, entry(fmt.Sprintf("bare-%d", i), nil))
	}

	const page = 50

	// 不筛分段时，第一页确实**只有**倍率 —— 这正是缺陷本身，先把它钉住，
	// 免得将来有人改了排序就以为分段筛选可以删了。
	noFilter, units := filterCatalog(all, "", "")
	for _, e := range noFilter[:page] {
		if unitKey(e.BillingUnit) != "per_1m_token" {
			t.Fatalf("前提变了：不筛分段的第一页出现了 %s，"+
				"说明排序或数据形状已改，请重新评估分段筛选是否还必要",
				unitKey(e.BillingUnit))
		}
	}

	// units 必须是**全量分布**，不随分段筛选缩水：界面靠它画分段按钮，
	// 缩水了就会切进某一段之后再也切不回来。
	want := map[string]int{"per_1m_token": 1161, "per_call": 208, "unknown": 12}
	for k, v := range want {
		if units[k] != v {
			t.Fatalf("units[%s] = %d，期望 %d", k, units[k], v)
		}
	}

	// 核心断言：每一段都能在第一页内看到，且看到的行口径纯净
	for unit, count := range want {
		got, u2 := filterCatalog(all, "", unit)
		if len(got) != count {
			t.Fatalf("unit=%s 筛出 %d 行，期望 %d", unit, len(got), count)
		}
		if len(got) == 0 {
			t.Fatalf("unit=%s 一行都没筛出来，该段在界面上不可达", unit)
		}
		for _, e := range got[:min(page, len(got))] {
			if unitKey(e.BillingUnit) != unit {
				t.Fatalf("unit=%s 的结果里混进了 %s 口径的行",
					unit, unitKey(e.BillingUnit))
			}
		}
		// 筛某一段时 units 仍须是全量分布
		for k, v := range want {
			if u2[k] != v {
				t.Fatalf("unit=%s 下 units[%s] = %d，期望 %d（分段规模不得随筛选缩水）",
					unit, k, u2[k], v)
			}
		}
	}
}

// TestFilterCatalogUnknownIsNotEmptyString 空值段必须用显式的 "unknown" 选。
//
// 若拿空串表示"口径为 NULL 的那一段"，`?unit=` 就同时意味着"不筛"和"筛空值段"，
// 两种意图在 query string 里无从区分 —— 想看存量未标注的行反而会看到全部。
func TestFilterCatalogUnknownIsNotEmptyString(t *testing.T) {
	all := []store.CatalogEntry{
		entry("a", unitPtr("per_call")),
		entry("b", nil),
		entry("c", unitPtr("per_1m_token")),
	}
	if got, _ := filterCatalog(all, "", ""); len(got) != 3 {
		t.Fatalf("unit 为空应视作不筛，得到 %d 行，期望 3", len(got))
	}
	got, _ := filterCatalog(all, "", "unknown")
	if len(got) != 1 || got[0].ModelName != "b" {
		t.Fatalf("unit=unknown 应只筛出 billing_unit 为 NULL 的行，得到 %+v", got)
	}
}

// TestFilterCatalogNameAndUnitCompose 名称与分段两个筛选要能叠加，
// 且叠加后的 units 反映的是"名称筛过、分段未筛"的分布 ——
// 搜了个词之后分段按钮上的数字必须是该词在各段的命中数，否则按钮会指向空页。
func TestFilterCatalogNameAndUnitCompose(t *testing.T) {
	all := []store.CatalogEntry{
		entry("gpt-4o", unitPtr("per_1m_token")),
		entry("gpt-4o-audio", unitPtr("per_call")),
		entry("claude-sonnet", unitPtr("per_1m_token")),
		entry("GPT-4O-Mini", unitPtr("per_1m_token")), // 大小写不敏感
		entry("sora-video", unitPtr("per_call")),
	}
	got, units := filterCatalog(all, "gpt-4o", "")
	if len(got) != 3 {
		t.Fatalf("名称筛选应命中 3 行（含大小写不同的一行），得到 %d", len(got))
	}
	if units["per_1m_token"] != 2 || units["per_call"] != 1 {
		t.Fatalf("名称筛过后的分段分布错了：%v", units)
	}
	if _, ok := units["unknown"]; ok {
		t.Fatalf("没有空值口径的行时不应出现 unknown 段：%v", units)
	}
	both, _ := filterCatalog(all, "gpt-4o", "per_call")
	if len(both) != 1 || both[0].ModelName != "gpt-4o-audio" {
		t.Fatalf("名称+分段叠加结果错了：%+v", both)
	}
}

// TestFilterCatalogDoesNotMutateInput 筛选不得就地改写入参。
//
// 原来的写法是 `filtered := all; filtered = filtered[:0]` 再往回 append，
// 复用的是 all 的底层数组。那版能跑对，只因为写指针恒不快于读指针 ——
// 这个前提没有任何东西守着，一旦有人在中间插一个"先按价格重排"就静默错位。
// 缓存 ListCatalog 的结果是很自然的下一步，届时被改写的是缓存本身。
func TestFilterCatalogDoesNotMutateInput(t *testing.T) {
	all := []store.CatalogEntry{
		entry("keep-a", unitPtr("per_1m_token")),
		entry("drop-b", unitPtr("per_call")),
		entry("keep-c", unitPtr("per_1m_token")),
		entry("drop-d", unitPtr("per_call")),
	}
	before := make([]string, len(all))
	for i, e := range all {
		before[i] = e.ModelName
	}
	if got, _ := filterCatalog(all, "", "per_1m_token"); len(got) != 2 {
		t.Fatalf("期望筛出 2 行，得到 %d", len(got))
	}
	for i, e := range all {
		if e.ModelName != before[i] {
			t.Fatalf("入参被就地改写了：all[%d] 从 %s 变成 %s",
				i, before[i], e.ModelName)
		}
	}
}
