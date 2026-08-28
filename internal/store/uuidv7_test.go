package store

import (
	"regexp"
	"sort"
	"testing"
	"time"
)

var uuidRe = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestUUIDv7Format(t *testing.T) {
	for i := 0; i < 100; i++ {
		u := NewUUIDv7()
		if !uuidRe.MatchString(u) {
			t.Fatalf("格式不符 UUIDv7: %s（版本位应为 7，variant 应为 8/9/a/b）", u)
		}
	}
}

func TestUUIDv7Unique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 10000; i++ {
		u := NewUUIDv7()
		if seen[u] {
			t.Fatalf("重复: %s", u)
		}
		seen[u] = true
	}
}

// 时间有序是选 v7 的**唯一理由**（02 §0.2）：顺序插入让 B-tree 尾部追加
// 而非随机分裂。若排序性丢失，就该改回 v4 而不是假装有序。
//
// ⚠️ v7 保证的是**跨毫秒**有序，同一毫秒内由随机位决定、无序是符合规范的。
// 首版测试连续生成 200 个（耗时远小于 1ms、全在同毫秒），于是随机位主导、
// 逆序对近半 —— 那是测试的前提错了，不是实现错了。故按毫秒间隔取样。
func TestUUIDv7IsTimeOrderedAcrossMillis(t *testing.T) {
	const n = 12
	ids := make([]string, n)
	for i := range ids {
		ids[i] = NewUUIDv7()
		time.Sleep(2 * time.Millisecond) // 确保跨毫秒
	}
	sorted := make([]string, n)
	copy(sorted, ids)
	sort.Strings(sorted)

	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("跨毫秒生成的 ID 未按时间有序（位置 %d）：\n生成序 %v\n排序后 %v\n"+
				"时间有序性不成立时选 v7 就没有意义（02 §0.2）", i, ids, sorted)
		}
	}
}

// 同毫秒内无序是**规范允许**的，不该被当成缺陷"修掉"。
// 这条测试的作用是把这个预期写下来，避免有人看到同毫秒无序就去加锁排序
// （那会引入无谓的争用，而账本主键并不需要毫秒内单调）。
func TestUUIDv7SameMillisNeedNotBeOrdered(t *testing.T) {
	prefix := func(u string) string { return u[:13] } // 48bit 时间戳部分
	a, b := NewUUIDv7(), NewUUIDv7()
	if prefix(a) == prefix(b) {
		// 同毫秒：不断言顺序，只断言两者不同
		if a == b {
			t.Fatal("同毫秒生成的两个 ID 不应相同")
		}
	}
}
