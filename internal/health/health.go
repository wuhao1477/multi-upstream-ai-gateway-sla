// Package health 实现 /healthz —— **只反映实例自身**的就绪状态。
//
// ⚠️ 本包存在的唯一理由是把一条纪律固化成代码（06 §6 健康语义分层）：
//
//	若把"上游渠道可用性"计入 /healthz，则全部上游不可用时两个 core 都会被判
//	不健康 → Caddy 摘除全部实例 → 客户端收到的是 LB 层 502/503，而不是我们
//	设计的"明确不可用响应 + 事件编号 + 建议重试时间"（AC-15/AC-27）。
//	后果是：账本不记录、告警不触发、故障被掩盖。
//
// 三层语义严格分开，不可混用：
//
//	/healthz（本包）      → 实例自身：进程存活 + PG 可达 + 配置快照已加载
//	渠道健康（P3 selector）→ 各 binding 的健康度/冷却/余额/配额，不影响实例就绪
//	全候选不可用（P2）     → 请求路径按等级排队后返回明确错误，落账本 + P1 告警
//
// 渠道级健康另由 GET /admin/health 暴露（09），它**不参与 LB 判定**。
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Pinger 是 PG 可达性探测。用接口而非直连 *pgxpool.Pool，
// 使本包可单测（不需要真库），也避免 health 依赖 store。
type Pinger interface {
	Ping(ctx context.Context) error
}

// SnapshotProbe 报告配置快照是否已加载。
// 快照未加载时实例不应接流量——决策路径只读快照（01 §5），没快照就没法决策。
type SnapshotProbe func() bool

// Handler 是 /healthz 的处理器。
type Handler struct {
	DB          Pinger
	HasSnapshot SnapshotProbe
	// PingTimeout 限制单次 PG 探测耗时，避免探针把自己挂住。
	PingTimeout time.Duration
}

type response struct {
	Status   string `json:"status"`          // ok | unavailable
	DB       string `json:"db"`              // ok | 错误原因
	Snapshot string `json:"config_snapshot"` // loaded | missing
	Note     string `json:"note,omitempty"`
}

const upstreamNote = "本端点不反映上游渠道可用性（06 §6）；渠道健康见 GET /admin/health"

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	timeout := h.PingTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	res := response{Status: "ok", DB: "ok", Snapshot: "loaded", Note: upstreamNote}

	if h.DB == nil {
		res.Status, res.DB = "unavailable", "未配置数据库"
	} else if err := h.DB.Ping(ctx); err != nil {
		res.Status, res.DB = "unavailable", err.Error()
	}

	if h.HasSnapshot == nil || !h.HasSnapshot() {
		res.Status, res.Snapshot = "unavailable", "missing"
	}

	code := http.StatusOK
	if res.Status != "ok" {
		// 503 让 Caddy 摘除本实例；另一实例继续服务（AC-27）。
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(res)
}
