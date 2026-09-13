package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestHubSyncRunsSurviveResultlessRow 是这套东西的真库回归点：
// **一轮没有结果的同步（取回或解密就失败了）不能让整个历史列表打不开。**
//
// 这条是实测撞出来的（2026-09-13）。当时 InsertHubSyncRun 把"没有结果"写成
// JSON 标量 `null`，而列表查询要 `result - 'items'` 剥掉逐站明细 —— jsonb 的
// 减号只接受对象/数组，碰上标量直接 `cannot delete from scalar`（SQLSTATE 22023）。
// 于是密码填错那一轮写进去之后，界面上的同步历史就一直 500，
// **而失败恰恰是最需要看历史的时候**。
//
// 为什么必须打真库：这是 PostgreSQL 对 jsonb 减号的运算规则，Go 这侧怎么写
// 单测都测不到它 —— 写个假 DB 只会回"我让它回的东西"（CLAUDE.md §1）。
func TestHubSyncRunsSurviveResultlessRow(t *testing.T) {
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库（由 verify/test-migrate.sh 提供）")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	wipe := func() {
		if _, err := conn.Exec(context.Background(), `DELETE FROM hub_sync_runs`); err != nil {
			t.Fatalf("清 hub_sync_runs: %v", err)
		}
	}
	wipe()
	defer wipe()

	now := time.Now()
	// ① 失败轮：没有结果。这一行就是当初把列表打爆的那种。
	failedID, err := InsertHubSyncRun(ctx, conn, HubSyncRun{
		StartedAt: now.Add(-2 * time.Minute), FinishedAt: now.Add(-2 * time.Minute),
		Trigger: HubSyncTriggerManual, Applied: false,
		Error: "解密失败：密码不对，或备份文件已被篡改",
	})
	if err != nil {
		t.Fatalf("写失败轮: %v", err)
	}
	// ② 成功轮：带逐站明细。
	okID, err := InsertHubSyncRun(ctx, conn, HubSyncRun{
		StartedAt: now, FinishedAt: now.Add(3 * time.Second),
		Trigger: HubSyncTriggerSchedule, Applied: true,
		Result: json.RawMessage(
			`{"total":2,"imported":1,"items":[{"site_url":"https://a.example"},` +
				`{"site_url":"https://b.example"}]}`),
	})
	if err != nil {
		t.Fatalf("写成功轮: %v", err)
	}

	runs, err := ListHubSyncRuns(ctx, conn, 0)
	if err != nil {
		t.Fatalf("列历史失败（失败轮把列表打爆了）: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("历史条数 = %d，want 2", len(runs))
	}
	// 倒序：最新的成功轮在前。
	if runs[0].ID != okID || runs[1].ID != failedID {
		t.Fatalf("顺序不对：%d,%d（want %d,%d）", runs[0].ID, runs[1].ID, okID, failedID)
	}
	if runs[1].Error == "" {
		t.Fatal("失败轮的 error 丢了 —— 那是界面上唯一能看见失败原因的地方")
	}

	// 列表必须剥掉 items（几十行明细一起回等于把上兆 JSON 塞进列表响应），
	// 但汇总计数要留着 —— 剥过头的话列表就只剩时间戳了。
	var listed map[string]any
	if err := json.Unmarshal(runs[0].Result, &listed); err != nil {
		t.Fatalf("列表里的 result 不是对象: %v", err)
	}
	if _, ok := listed["items"]; ok {
		t.Fatal("列表里的 result 仍然带着 items")
	}
	if listed["total"] == nil {
		t.Fatal("列表里的 result 把汇总计数也剥掉了")
	}

	// 详情要带全明细 —— 点开一条历史要回答的正是"哪个站点失败了"。
	detail, err := GetHubSyncRun(ctx, conn, okID)
	if err != nil {
		t.Fatalf("读详情: %v", err)
	}
	var full struct {
		Items []struct {
			SiteURL string `json:"site_url"`
		} `json:"items"`
	}
	if err := json.Unmarshal(detail.Result, &full); err != nil {
		t.Fatalf("详情里的 result 解析失败: %v", err)
	}
	if len(full.Items) != 2 {
		t.Fatalf("详情里的明细 = %d 条，want 2", len(full.Items))
	}

	// 失败轮的详情也要能打开：它的 result 是 SQL NULL，读出来该是 JSON null
	// 而不是报错或空字节串（空字节串会让前端 JSON.parse 炸）。
	failedDetail, err := GetHubSyncRun(ctx, conn, failedID)
	if err != nil {
		t.Fatalf("读失败轮详情: %v", err)
	}
	if string(failedDetail.Result) != "null" {
		t.Fatalf("失败轮的 result = %q，want null", failedDetail.Result)
	}
}

// TestHubSyncRunsPruneToKeepLimit 写入时裁剪必须真的在裁。
//
// 不裁的后果不是立刻可见的：每轮的 result 含逐站明细，6 小时一轮要几个月
// 才涨到碍事的大小 —— 等发现时已经积了几千行。
func TestHubSyncRunsPruneToKeepLimit(t *testing.T) {
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库（由 verify/test-migrate.sh 提供）")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	wipe := func() {
		if _, err := conn.Exec(context.Background(), `DELETE FROM hub_sync_runs`); err != nil {
			t.Fatalf("清 hub_sync_runs: %v", err)
		}
	}
	wipe()
	defer wipe()

	base := time.Now().Add(-time.Hour)
	var lastID int64
	for i := range hubSyncRunsKeep + 5 {
		id, err := InsertHubSyncRun(ctx, conn, HubSyncRun{
			StartedAt:  base.Add(time.Duration(i) * time.Minute),
			FinishedAt: base.Add(time.Duration(i) * time.Minute),
			Trigger:    HubSyncTriggerSchedule,
			Result:     json.RawMessage(`{"total":0}`),
		})
		if err != nil {
			t.Fatalf("第 %d 轮写入失败: %v", i, err)
		}
		lastID = id
	}

	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM hub_sync_runs`).Scan(&n); err != nil {
		t.Fatalf("数行: %v", err)
	}
	if n != hubSyncRunsKeep {
		t.Fatalf("裁剪后剩 %d 行，want %d", n, hubSyncRunsKeep)
	}
	// 留下的必须是**最近**那批，不是最早那批。
	if _, err := GetHubSyncRun(ctx, conn, lastID); err != nil {
		t.Fatalf("最新一轮被裁掉了: %v", err)
	}
}
