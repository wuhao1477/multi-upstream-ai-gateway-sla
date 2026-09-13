package admin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// TestHubSyncDue 定时判据。
//
// 单独测它是因为循环那一层在验收里**跑不到**：真等一个周期意味着验收至少多
// 一分钟，而把周期调到秒级又等于不测真实配置。所以这里测判据、验收测"手动
// 触发跑通整条路"，两边合起来才是"定时同步能用"——**循环本身只有冒烟覆盖**，
// 这一点如实写在这里，不要让它看起来像全覆盖。
func TestHubSyncDue(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }

	cases := []struct {
		name string
		cfg  store.HubSyncConfig
		want bool
	}{
		{"从未跑过就该立刻跑",
			store.HubSyncConfig{IntervalMinutes: 360}, true},
		{"刚跑完不该再跑",
			store.HubSyncConfig{IntervalMinutes: 360, LastRunAt: ago(time.Minute)}, false},
		{"差一分钟到点也不跑",
			store.HubSyncConfig{IntervalMinutes: 60, LastRunAt: ago(59 * time.Minute)}, false},
		{"整点到了就跑",
			store.HubSyncConfig{IntervalMinutes: 60, LastRunAt: ago(60 * time.Minute)}, true},
		{"超期了当然跑",
			store.HubSyncConfig{IntervalMinutes: 60, LastRunAt: ago(10 * time.Hour)}, true},
		// 间隔为 0 只可能来自被直接改过的库（迁移的 CHECK 挡住 <5）。
		// 兜成最小间隔，而不是"每分钟都跑"——后者会对着别人家的 WebDAV 猛打。
		{"间隔为 0 时兜到最小间隔而不是每轮都跑",
			store.HubSyncConfig{IntervalMinutes: 0, LastRunAt: ago(time.Minute)}, false},
		{"间隔为 0 时超过最小间隔仍会跑",
			store.HubSyncConfig{IntervalMinutes: 0, LastRunAt: ago(6 * time.Minute)}, true},
	}
	for _, c := range cases {
		if got := hubSyncDue(c.cfg, now); got != c.want {
			t.Errorf("%s: hubSyncDue = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestHubSyncViewHidesSecrets 两个密码一律只报有没有。
//
// 这条守的是 FR-094 同源纪律：界面上那两个框每次打开都是空的，靠的就是这里
// 从不把内容放进响应。加一个字段时很容易顺手把 store 结构整个塞进去，
// 那一刻密码就进了 JSON，而界面看起来完全正常。
func TestHubSyncViewHidesSecrets(t *testing.T) {
	v := hubSyncViewOf(store.HubSyncConfig{
		WebDAVURL:      "https://dav.example/dav",
		WebDAVUsername: "someone",
		WebDAVPassword: "dav-secret-should-never-leave",
		BackupPassword: "enc-secret-should-never-leave",
	})
	if !v.HasWebDAVPassword || !v.HasBackupPassword {
		t.Fatal("有密码却报成没有")
	}
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"dav-secret-should-never-leave", "enc-secret-should-never-leave"} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("响应里出现了密码原文: %s", blob)
		}
	}

	empty := hubSyncViewOf(store.HubSyncConfig{})
	if empty.HasWebDAVPassword || empty.HasBackupPassword {
		t.Fatal("没密码却报成有 —— 界面会显示「已配置」而其实是空的")
	}
}
