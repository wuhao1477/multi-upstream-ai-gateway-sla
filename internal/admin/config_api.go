// Package admin 是管理平面 /admin/*（09）。
//
// 边界（09 §1）：只在内网/本机可达 —— Caddy 不代理 /admin/* 与 /metrics
// （06 §1）。但网络边界之外**仍需管理令牌**：两层缺一不可，网络边界防外部，
// 令牌防同机其它进程（compose 里还跑着 collector）。
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/config"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// DB 是管理平面所需的库能力。用接口而非具体池类型，便于单测替换。
type DB interface {
	Acquire(ctx context.Context) (*pgx.Conn, func(), error)
}

// Server 承载 /admin/* 全部端点。
type Server struct {
	DB     DB
	Token  string // ADMIN_TOKEN；空则拒绝一切请求（不是"放行一切"）
	Logger *slog.Logger

	// Detect 探测站型（建渠道时可选自动探测，04 §2）。
	Detect func(ctx context.Context, baseURL string) (collector.DetectResult, error)
	// Sync 执行一次渠道采集（由 main 注入，避免 admin 依赖具体适配器）。
	Sync SyncRunner
	// Snapshot 读当前配置快照（sync 间隔、下架轮数等）。
	Snapshot func() *config.Snapshot
	// SaveCredential 持久化采集凭证。注入函数而非 *store.Pool ——
	// admin 只依赖 DB 接口，不该知道连接池的具体类型。
	//
	// ⚠️ 收 `store.DBTX` 而不自己取连接（2026-08-29 改）：导入要把四处写入放进
	// 同一个事务，而这个函数原先自己 `Pool.Acquire` 取**另一条连接**独立提交 ——
	// 调用方无论怎么包事务都盖不住它。传 `*pgx.Conn` 时行为与从前一致。
	SaveCredential func(ctx context.Context, db store.DBTX, cred collector.Credential) error
	// SaveDetected 持久化站型探测结果。
	// **必须落库**：quota_per_unit 是 NewAPI 系额度归一的必需输入，
	// 而 FetchAccount 缺它会直接报错（不猜，猜错差 50 万倍）——
	// 只在响应里回显给人看是不够的。
	//
	// ⚠️ 同上收 `store.DBTX`。
	SaveDetected func(ctx context.Context, db store.DBTX, channelID int64, d collector.DetectResult) error
	// ImportKeys 在渠道基础事务提交后读取并登记上游 Key。它是可选能力：
	// Key 读取失败不得影响渠道、账号与凭证导入。
	ImportKeys func(ctx context.Context, conn *pgx.Conn, channelID, accountID int64) (collector.KeyImportResult, error)

	guard  *syncGuard
	tokens *tokenStore
	// onConfigChange 在 apply 成功后触发内存快照重建（09 §2 末条：
	// 使新配置对决策路径生效；决策路径本身仍只读快照、不查库）。
	onConfigChange func()
}

// NewServer 构造管理平面。
func NewServer(db DB, token string, logger *slog.Logger, onConfigChange func()) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		DB: db, Token: token, Logger: logger,
		guard:          newSyncGuard(),
		tokens:         newTokenStore(),
		onConfigChange: onConfigChange,
	}
}

// Routes 注册全部管理端点。
func (s *Server) Routes(mux *http.ServeMux) {
	h := func(f func(http.ResponseWriter, *http.Request)) http.Handler {
		return s.requireToken(http.HandlerFunc(f))
	}
	mux.Handle("GET /admin/config", h(s.listConfig))
	mux.Handle("GET /admin/config/{scopeType}/{scopeID}/{paramKey}", h(s.getConfig))
	mux.Handle("POST /admin/config/preview", h(s.previewConfig))
	mux.Handle("POST /admin/config/apply", h(s.applyConfig))
	mux.Handle("GET /admin/config/history", h(s.configHistory))
}

// requireToken 校验管理令牌。
//
// **Token 为空时拒绝一切请求**，而不是放行 —— 配置缺失应导致"不可用"
// 而非"无鉴权可用"。这是与 /healthz 相反方向的取舍：那里缺配置报不健康，
// 这里缺配置报拒绝，两者都指向"不要在配置缺失时提供服务"。
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" {
			s.fail(w, http.StatusServiceUnavailable,
				"ADMIN_TOKEN 未配置，管理平面不可用")
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got != s.Token {
			// 不回显收到的令牌（FR-094 同源纪律：凭证不进日志与响应）
			s.fail(w, http.StatusUnauthorized, "管理令牌无效")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type configItem struct {
	Key            string `json:"key"`
	Value          string `json:"value"`
	Default        string `json:"default"`
	IsCritical     bool   `json:"is_critical"`
	Group          string `json:"group"`
	Version        int    `json:"version,omitempty"`
	ConfirmedTwice bool   `json:"confirmed_twice,omitempty"`
	// EnvSourced 项在清单里可见但不可改（09 §4bis"此处仅登记其存在"），
	// 且**不回显其值** —— admin_token 的值就是当前鉴权令牌本身。
	EnvSourced bool   `json:"env_sourced,omitempty"`
	Note       string `json:"note,omitempty"`
}

func (s *Server) listConfig(w http.ResponseWriter, r *http.Request) {
	conn, release, err := s.DB.Acquire(r.Context())
	if err != nil {
		s.fail(w, http.StatusServiceUnavailable, "获取连接失败: "+err.Error())
		return
	}
	defer release()

	values, err := store.LoadConfigValues(r.Context(), conn)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err.Error())
		return
	}

	items := make([]configItem, 0, len(config.Params))
	for _, p := range config.Params {
		if p.EnvSourced {
			// 登记其存在但**不回显值也不回显默认值** ——
			// admin_token 的"值"就是当前鉴权令牌，回显等于泄露（FR-094）。
			items = append(items, configItem{
				Key: p.Key, IsCritical: p.Critical, Group: p.Group,
				EnvSourced: true,
				Note:       "由环境变量注入，不落 config_params、不可经本接口修改",
			})
			continue
		}
		v, ok := values[p.Key]
		if !ok {
			// 未生效（关键项未确认）或库中缺失 → 展示默认值，与快照行为一致
			v = p.Default
		}
		items = append(items, configItem{
			Key: p.Key, Value: v, Default: p.Default,
			IsCritical: p.Critical, Group: p.Group,
		})
	}
	s.ok(w, map[string]any{"count": len(items), "items": items})
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("paramKey")
	spec, ok := config.Spec(key)
	if !ok {
		s.fail(w, http.StatusBadRequest,
			fmt.Sprintf("未知配置键 %q（不在 09 §4bis 清单）", key))
		return
	}
	conn, release, err := s.DB.Acquire(r.Context())
	if err != nil {
		s.fail(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer release()

	ver, val, err := store.CurrentConfigVersion(r.Context(), conn, key)
	if err != nil {
		s.mapStoreErr(w, err)
		return
	}
	hist, err := store.ConfigHistory(r.Context(), conn, key)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.ok(w, map[string]any{
		"key": key, "value": val, "default": spec.Default,
		"is_critical": spec.Critical, "group": spec.Group,
		"current_version": ver, "history": hist,
	})
}

type previewReq struct {
	ParamKey     string `json:"param_key"`
	NewValue     string `json:"new_value"`
	ChangedBy    string `json:"changed_by"`
	ChangeReason string `json:"change_reason"`
}

func (s *Server) previewConfig(w http.ResponseWriter, r *http.Request) {
	var req previewReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	spec, ok := config.Spec(req.ParamKey)
	if !ok {
		s.fail(w, http.StatusBadRequest,
			fmt.Sprintf("未知配置键 %q（不在 09 §4bis 清单）", req.ParamKey))
		return
	}
	if req.ChangedBy == "" || req.ChangeReason == "" {
		// FR-099 要求记录责任人与原因，缺了就没有审计价值
		s.fail(w, http.StatusBadRequest, "changed_by 与 change_reason 必填（FR-099）")
		return
	}
	if err := validateValue(spec, req.NewValue); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}

	conn, release, err := s.DB.Acquire(r.Context())
	if err != nil {
		s.fail(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer release()

	ver, cur, err := store.CurrentConfigVersion(r.Context(), conn, req.ParamKey)
	if err != nil {
		s.mapStoreErr(w, err)
		return
	}

	resp := map[string]any{
		"valid": true, "is_critical": spec.Critical,
		"current_value": cur, "new_value": req.NewValue,
		"current_version": ver,
		"impact":          impactHint(spec, cur, req.NewValue),
	}
	// 非关键项可跳过 preview 直接 apply（09 §3 末条），故不签发令牌。
	if spec.Critical {
		tok, err := s.tokens.issue(req.ParamKey, req.NewValue, ver)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp["confirm_token"] = tok
		resp["confirm_token_ttl_seconds"] = int(confirmTTL.Seconds())
	}
	s.ok(w, resp)
}

type applyReq struct {
	ParamKey        string `json:"param_key"`
	NewValue        string `json:"new_value"`
	ChangedBy       string `json:"changed_by"`
	ChangeReason    string `json:"change_reason"`
	ConfirmToken    string `json:"confirm_token"`
	ExpectedVersion int    `json:"expected_current_version"`
	IdempotencyKey  string `json:"idempotency_key"`
}

func (s *Server) applyConfig(w http.ResponseWriter, r *http.Request) {
	var req applyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	spec, ok := config.Spec(req.ParamKey)
	if !ok {
		s.fail(w, http.StatusBadRequest,
			fmt.Sprintf("未知配置键 %q（不在 09 §4bis 清单）", req.ParamKey))
		return
	}
	if req.ChangedBy == "" || req.ChangeReason == "" {
		s.fail(w, http.StatusBadRequest, "changed_by 与 change_reason 必填（FR-099）")
		return
	}
	if err := validateValue(spec, req.NewValue); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// 关键项强制两步提交（FR-115）：令牌绑定具体 diff 且一次性消耗。
	confirmed := false
	if spec.Critical {
		if req.ConfirmToken == "" {
			s.fail(w, http.StatusBadRequest,
				"关键项必须先 POST /admin/config/preview 取 confirm_token（FR-115）")
			return
		}
		if !s.tokens.consume(req.ConfirmToken, req.ParamKey,
			req.NewValue, req.ExpectedVersion) {
			s.fail(w, http.StatusBadRequest,
				"confirm_token 无效、已过期或与本次改动不符，请重新预览")
			return
		}
		confirmed = true
	}

	conn, release, err := s.DB.Acquire(r.Context())
	if err != nil {
		s.fail(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer release()

	res, err := store.ApplyConfig(r.Context(), conn,
		req.ParamKey, valueToJSON(spec, req.NewValue),
		req.ChangedBy, req.ChangeReason,
		req.ExpectedVersion, spec.Critical, confirmed, req.IdempotencyKey)
	if err != nil {
		s.mapStoreErr(w, err)
		return
	}

	// 触发快照重建，使新配置对决策路径生效（09 §2）。
	if s.onConfigChange != nil {
		s.onConfigChange()
	}
	s.Logger.Info("配置已变更",
		"key", req.ParamKey, "version", res.NewVersion,
		"by", req.ChangedBy, "reason", req.ChangeReason,
		"idempotent", res.Idempotent)

	s.ok(w, map[string]any{
		"key": req.ParamKey, "new_version": res.NewVersion,
		"prev_value": res.PrevValue, "value": req.NewValue,
		"idempotent_replay": res.Idempotent,
	})
}

func (s *Server) configHistory(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("param_key")
	if key == "" {
		s.fail(w, http.StatusBadRequest, "缺少 param_key 查询参数")
		return
	}
	if _, ok := config.Spec(key); !ok {
		s.fail(w, http.StatusBadRequest, fmt.Sprintf("未知配置键 %q", key))
		return
	}
	conn, release, err := s.DB.Acquire(r.Context())
	if err != nil {
		s.fail(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer release()

	hist, err := store.ConfigHistory(r.Context(), conn, key)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.ok(w, map[string]any{"key": key, "versions": hist})
}

// mapStoreErr 把 store 的三类错误映射成 09 §3 规定的状态码。
func (s *Server) mapStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrVersionConflict):
		s.fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrUnknownParam):
		s.fail(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrNeedsConfirm):
		s.fail(w, http.StatusBadRequest, err.Error())
	default:
		s.fail(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) ok(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
