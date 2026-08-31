package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// SyncRunner 执行一次渠道采集（由 main 注入，避免 admin 依赖具体适配器）。
type SyncRunner func(ctx context.Context, ch store.Channel) (*collector.SyncResult, error)

// UpstreamRoutes 注册 P1 的上游资产管理端点（09 §5.0）。
func (s *Server) UpstreamRoutes(mux *http.ServeMux) {
	h := func(f func(http.ResponseWriter, *http.Request)) http.Handler {
		return s.requireToken(http.HandlerFunc(f))
	}
	// 渠道
	mux.Handle("GET /admin/channels", h(s.listChannels))
	mux.Handle("POST /admin/channels", h(s.createChannel))
	mux.Handle("PATCH /admin/channels/{id}", h(s.patchChannel))
	mux.Handle("GET /admin/channels/{id}/inventory", h(s.channelInventory))
	mux.Handle("POST /admin/channels/{id}/sync", h(s.syncChannel))
	mux.Handle("GET /admin/channels/{id}/catalog", h(s.channelCatalog))
	// 账号
	mux.Handle("GET /admin/accounts", h(s.listAccounts))
	mux.Handle("POST /admin/accounts", h(s.createAccount))
	// Key
	mux.Handle("GET /admin/keys", h(s.listKeys))
	mux.Handle("POST /admin/keys", h(s.createKey))
	mux.Handle("POST /admin/keys/{id}/rotate", h(s.rotateKey))
	mux.Handle("POST /admin/keys/{id}/disable", h(s.disableKey))
	mux.Handle("GET /admin/keys/{id}/usage", h(s.keyUsage))
	// 采集凭证（04 §5；一期明文 FR-113）
	mux.Handle("POST /admin/collector/credentials", h(s.saveCredential))
	mux.Handle("GET /admin/collector/credentials", h(s.listCredentials))
	// 分组
	mux.Handle("GET /admin/channel-groups", h(s.listGroups))
	mux.Handle("GET /admin/channel-groups/{id}/models", h(s.groupModels))
	// 站型注册表（04 §2/§7）
	mux.Handle("GET /admin/site-families", h(s.listSiteFamilies))
}

// listSiteFamilies 返回已注册的站型（collector 的注册表）。
//
// 存在的理由：界面的站型下拉此前是写死的四项。加一个站型时那份写死的列表
// 不会报任何错 —— 新站型就是**在界面上不存在**，运维只能靠自动探测碰上它。
// 读注册表之后，加一份 Registration 就同时在后端与界面生效。
//
// 不带库查询也不碰凭证，但仍走 requireToken：全部 /admin/* 一个口径，
// 例外会让"这条为什么不用鉴权"变成每次读代码都要重新判断的问题。
func (s *Server) listSiteFamilies(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Family      string   `json:"family"`
		DisplayName string   `json:"display_name"`
		Aliases     []string `json:"aliases"`
		CredType    string   `json:"cred_type"`
		RequiresUID bool     `json:"requires_external_user_id"`
		// AllowsPassword 为真表示可用账密登记（无 refresh 路径的站型）。
		AllowsPassword bool `json:"allows_password"`
	}
	out := []item{}
	for _, reg := range collector.All() {
		out = append(out, item{
			Family: string(reg.Family), DisplayName: reg.DisplayName,
			Aliases: reg.Aliases, CredType: reg.CredType,
			RequiresUID:    reg.RequiresUID,
			AllowsPassword: reg.PasswdCredType != "",
		})
	}
	s.ok(w, map[string]any{"count": len(out), "items": out})
}

// ── 渠道 ──

func (s *Server) listChannels(w http.ResponseWriter, r *http.Request) {
	s.withConn(w, r, func(conn *pgx.Conn) {
		cs, err := store.ListChannels(r.Context(), conn)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if cs == nil {
			cs = []store.Channel{}
		}
		s.ok(w, map[string]any{"count": len(cs), "items": cs})
	})
}

func (s *Server) createChannel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name       string `json:"name"`
		BaseURL    string `json:"base_url"`
		SiteFamily string `json:"site_family"`
		// AutoDetect 为真时先探测站型（04 §2），失败不阻止建渠道 ——
		// 站型可以后补，而"先建上再探测"是运维的自然顺序。
		AutoDetect bool `json:"auto_detect"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.fail(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.BaseURL) == "" {
		s.fail(w, http.StatusBadRequest, "name 与 base_url 必填")
		return
	}
	if err := validateBaseURL(strings.TrimSpace(in.BaseURL)); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}

	var detected *collector.DetectResult
	if in.AutoDetect && s.Detect != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if res, err := s.Detect(ctx, in.BaseURL); err == nil {
			detected = &res
			if in.SiteFamily == "" {
				in.SiteFamily = string(res.Family)
			}
		} else {
			s.Logger.Warn("站型探测失败，仍继续建渠道", "base_url", in.BaseURL, "err", err)
		}
	}

	// ⚠️ 建渠道与探测结果落库**必须同生共死**（2026-09-01 修，Codex 三次评审 [high]）。
	//
	// 原先是：CreateChannel 先提交，SaveDetected 失败只 Logger.Warn + 往响应里塞
	// 一个 warning_persist，接口照样返 201 Created。于是一次瞬时库错误就留下
	// "看起来建成功、实际永远采不了"的渠道 —— quota_per_unit 缺失时 FetchAccount
	// 直接报错（04 §2：不猜，猜错差 50 万倍）。更糟的是重试 POST 会再建一条而不是
	// 修好它，而库里没有 DELETE 渠道的入口。
	//
	// 这与导入侧那条 [high] 是同一个缺陷的两半：上一轮只把导入侧收进了事务。
	s.withConn(w, r, func(conn *pgx.Conn) {
		tx, err := conn.Begin(r.Context())
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "开事务: "+err.Error())
			return
		}
		// 已 Commit 后 Rollback 返 ErrTxClosed，忽略即可；未提交时它才是真回滚。
		defer func() { _ = tx.Rollback(r.Context()) }()

		id, err := store.CreateChannel(r.Context(), tx, store.Channel{
			Name: in.Name, BaseURL: strings.TrimRight(in.BaseURL, "/"),
			SiteFamily: in.SiteFamily,
		})
		if err != nil {
			// 同地址已存在是**冲突**而不是"请求写错了"：调用方该去改那一条，
			// 不是改自己的请求。409 让重试逻辑能区分这两种。
			if errors.Is(err, store.ErrDuplicate) {
				s.fail(w, http.StatusConflict, err.Error())
				return
			}
			s.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		resp := map[string]any{"id": id, "name": in.Name, "site_family": in.SiteFamily}
		if detected != nil {
			// 探测结果必须落库：quota_per_unit 是后续额度归一的必需输入。
			// 落不进去就整体回滚 —— 半个渠道比没有渠道更难处理。
			if s.SaveDetected != nil {
				if err := s.SaveDetected(r.Context(), tx, id, *detected); err != nil {
					s.Logger.Error("探测结果落库失败，整笔回滚",
						"base_url", in.BaseURL, "err", err)
					s.fail(w, http.StatusInternalServerError,
						"站型探测结果未能落库，渠道未创建（避免留下采不到数据的半成品）："+
							err.Error())
					return
				}
			}
			resp["detected"] = map[string]any{
				"family": detected.Family, "version": detected.Version,
				"no_shield": detected.NoShield, "quota_per_unit": detected.QuotaPerUnit,
			}
			// 开盾站点服务端采集不可行，须转人工录入（04 §6）—— 明确告知
			if !detected.NoShield && detected.Family != collector.FamilyUnknown {
				resp["warning"] = "该站点疑似开启 turnstile 人机验证，" +
					"服务端自动采集可能不可行，需转人工录入（04 §6）"
			}
		}
		if err := tx.Commit(r.Context()); err != nil {
			s.fail(w, http.StatusInternalServerError, "提交建渠道: "+err.Error())
			return
		}
		s.Logger.Info("渠道已创建", "id", id, "name", in.Name, "family", in.SiteFamily)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	})
}

func (s *Server) patchChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Name           string     `json:"name"`
		BaseURL        string     `json:"base_url"`
		SiteFamily     string     `json:"site_family"`
		Status         string     `json:"status"`
		DisabledReason string     `json:"disabled_reason"`
		DisabledUntil  *time.Time `json:"disabled_until"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// status 只认两个值。不校验的话非法值会撞 CHECK 约束，变成一句 PG 错误 ——
	// 调用方读不出"哪个字段不对"。
	if in.Status != "" && in.Status != "enabled" && in.Status != "disabled" {
		s.fail(w, http.StatusBadRequest,
			"status 只能是 enabled 或 disabled，收到："+in.Status)
		return
	}
	// base_url 是**会被采集器请求的地址**，与 POST 走同一道校验。
	//
	// ⚠️ 2026-09-01 之前这里一处校验都没有：POST 拒 `file:///etc/passwd`（400），
	// PATCH 却返 200 并把它写进库（实测四种坏值 file:// / 非 URL / gopher:// /
	// 无 host 全部落库）。于是"建渠道那道校验"可以被一次 PATCH 完整绕过，
	// 而 syncChannel 之后每轮都会去请求那个地址。本轮自审与 Codex 三次评审
	// 各自独立查到同一条（后者判 [critical]）。
	//
	// 规范化也必须与 POST 一致（去尾斜杠）：否则 019 的唯一约束能被一个 "/" 绕过。
	if bu := strings.TrimSpace(in.BaseURL); bu != "" {
		if err := validateBaseURL(bu); err != nil {
			s.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		in.BaseURL = strings.TrimRight(bu, "/")
	}
	// FR-095：停用必须填原因。库里没有 CHECK 拦这个（原因列可空 —— 启用态本就
	// 该是空），所以这道闸只能在这里。少了它就会出现"已停用但没人知道为什么"
	// 的渠道，而停用是要人来解除的，没原因等于解不了。
	if in.Status == "disabled" && strings.TrimSpace(in.DisabledReason) == "" {
		s.fail(w, http.StatusBadRequest, "停用渠道必须填 disabled_reason（FR-095）")
		return
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		err := store.UpdateChannel(r.Context(), conn, store.Channel{
			ID: id, Name: in.Name, BaseURL: in.BaseURL, SiteFamily: in.SiteFamily,
			Status: in.Status, DisabledReason: in.DisabledReason,
			DisabledUntil: in.DisabledUntil,
		})
		if err != nil {
			// 改 base_url 会撞 019 的唯一约束 —— 那是冲突（去用已有那条），
			// 不是"没找到"，两者混在一起会让调用方按错的方式重试。
			if errors.Is(err, store.ErrDuplicate) {
				s.fail(w, http.StatusConflict, err.Error())
				return
			}
			s.mapNotFound(w, err)
			return
		}
		s.ok(w, map[string]any{"id": id, "updated": true})
	})
}

// ── sync（09 §5.0bis）──

// syncGuard 保证同渠道互斥且遵守最小间隔。
//
// 用进程内状态而非 PG 咨询锁：管理请求只到一个实例（Caddy 不代理 /admin/*，
// 06 §1），跨实例互斥无意义；而进程内 map 免掉一次库往返。
type syncGuard struct {
	mu      sync.Mutex
	running map[int64]bool
	last    map[int64]time.Time
}

func newSyncGuard() *syncGuard {
	return &syncGuard{running: map[int64]bool{}, last: map[int64]time.Time{}}
}

var errSyncTooSoon = errors.New("同渠道 sync 间隔未到")
var errSyncRunning = errors.New("该渠道已有 sync 在执行")

// acquire 尝试取得该渠道的 sync 许可。
func (g *syncGuard) acquire(channelID int64, minInterval time.Duration) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running[channelID] {
		return errSyncRunning
	}
	if t, ok := g.last[channelID]; ok && time.Since(t) < minInterval {
		return fmt.Errorf("%w：还需等 %.0f 秒",
			errSyncTooSoon, (minInterval - time.Since(t)).Seconds())
	}
	g.running[channelID] = true
	return nil
}

// release 解除互斥。armWindow 决定是否顺带起算最小间隔。
//
// 互斥无条件解除；窗口**只在这次尝试真的触达了上游时**才起算 ——
// 最小间隔是给上游挡请求的，本地前置失败（缺凭证、站型未知、取不到连接）
// 一个字节都没发出去，凭什么让下一次真采集等 60 秒。
func (g *syncGuard) release(channelID int64, armWindow bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.running, channelID)
	if armWindow {
		g.last[channelID] = time.Now()
	}
}

// disabledSyncMessage 判定"已停用的渠道不采集"，返回给运维看的原因。
//
// 这条守卫此前不存在，症状是**"停用"只改了台账上的一个字段**：2026-08-31 放弃
// 那 20 个采不到的站（PATCH status=disabled）之后，对已停用的渠道 30 发采集
// 照样打了上游并返 502。于是每轮覆盖率报告仍会去打 20 个死站 —— 20 次无谓的
// 上游请求、20 条注定的 fatal、人工清单永远停在 20 条，而"放弃"这个动作在
// 报告上看不出任何效果。停用必须在采集侧真的生效。
//
// 归入 ErrPrecondition 那一类（调用方返 422 而非 502，且**不起算 60s 窗口**）：
// 它在发第一个上游请求之前就失败，且是**配置态**而不是上游故障 —— 返 502 会让
// 运维去查别人家站点为什么挂了，而真相是我们自己停用了它。
//
// 想重新采集就先 PATCH status=enabled，**不给"绕过停用"的旁路**：
// 有旁路的话"已停用"就不再是一个可依赖的事实。
//
// 抽成纯函数是为了能单测：syncChannel 本体要真库连接（withConn），
// 而这条判定的语义（哪个状态挡、消息里带不带原因、非 disabled 一律放行）
// 不需要库就能钉住。真库那端的覆盖由 test-api / ui-stack 打。
func disabledSyncMessage(status, disabledReason string) (string, bool) {
	if status != "disabled" {
		return "", false
	}
	msg := fmt.Sprintf("%v：渠道已停用", collector.ErrPrecondition)
	if disabledReason != "" {
		msg += "（" + disabledReason + "）"
	}
	return msg + "。要重新采集请先启用该渠道。", true
}

func (s *Server) syncChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if s.Sync == nil {
		s.fail(w, http.StatusServiceUnavailable, "采集器未配置")
		return
	}

	minInterval := 60 * time.Second
	if s.Snapshot != nil {
		if v, err := s.Snapshot().Int("sync_min_interval_s"); err == nil && v > 0 {
			minInterval = time.Duration(v) * time.Second
		}
	}
	if err := s.guard.acquire(id, minInterval); err != nil {
		// 限流与互斥分别返 429 / 409（09 §5.0bis）：
		// **不排队** —— 手动刷新重复点击应立即得到反馈而非静默堆积
		code := http.StatusTooManyRequests
		if errors.Is(err, errSyncRunning) {
			code = http.StatusConflict
		}
		s.failWith(w, code, err.Error(), map[string]any{
			"channel_id": id,
			"items":      []map[string]any{{"status": "skipped"}},
		})
		return
	}
	// 默认不起算窗口：只有确认打过上游的分支才置 true，
	// 这样新增的提前 return 分支不会悄悄开始限流（默认值取安全的一侧）。
	reachedUpstream := false
	defer func() { s.guard.release(id, reachedUpstream) }()

	s.withConn(w, r, func(conn *pgx.Conn) {
		ch, err := store.GetChannel(r.Context(), conn, id)
		if err != nil {
			s.mapNotFound(w, err)
			return
		}
		if msg, blocked := disabledSyncMessage(ch.Status, ch.DisabledReason); blocked {
			s.failWith(w, http.StatusUnprocessableEntity, msg,
				map[string]any{"channel_id": id, "site_family": ch.SiteFamily,
					"items": []map[string]any{{"status": "skipped"}}})
			return
		}
		// sync 会打多个上游端点，给足超时（限速本身就要花时间）
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()

		res, err := s.Sync(ctx, ch)
		if err != nil {
			if errors.Is(err, collector.ErrPrecondition) {
				// 没打上游 → 不起算窗口，且这是**配置问题**而非上游故障：
				// 返 422 而不是 502，否则运维会去查上游为什么挂了。
				s.failWith(w, http.StatusUnprocessableEntity, err.Error(),
					map[string]any{"channel_id": id, "site_family": ch.SiteFamily,
						"items": []map[string]any{{"status": "skipped"}}})
				return
			}
			reachedUpstream = true
			// 鉴权/连接失败 → 整体中止（09 §5.0bis ①）
			s.failWith(w, http.StatusBadGateway, err.Error(),
				map[string]any{"channel_id": id, "site_family": ch.SiteFamily})
			return
		}
		reachedUpstream = true
		s.Logger.Info("渠道采集完成", "channel_id", id,
			"elapsed_ms", res.ElapsedMs, "has_failure", res.HasFailure())
		s.ok(w, res)
	})
}

// ── inventory（09 §5.0 六类异常项）──

type inventoryResp struct {
	ChannelID     int64          `json:"channel_id"`
	Name          string         `json:"name"`
	SiteFamily    string         `json:"site_family"`
	Accounts      int            `json:"accounts"`
	Keys          int            `json:"keys"`
	Groups        int            `json:"groups"`
	CatalogModels int            `json:"catalog_models"`
	QuotaTotalUSD float64        `json:"quota_total_usd"`
	LastSyncedAt  *time.Time     `json:"last_synced_at,omitempty"`
	Anomalies     []anomalyGroup `json:"anomalies"`
}

// anomalyGroup 是一类异常项。
//
// **逐类计数并可下钻**（09 §5.0）：不合并成一个总数 ——
// 否则运维看到"异常 7"却不知道该修什么。
type anomalyGroup struct {
	Kind  string   `json:"kind"`
	Count int      `json:"count"`
	Hint  string   `json:"hint"`
	Items []string `json:"items,omitempty"`
}

func (s *Server) channelInventory(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		ctx := r.Context()
		ch, err := store.GetChannel(ctx, conn, id)
		if err != nil {
			s.mapNotFound(w, err)
			return
		}
		inv, err := store.BuildInventory(ctx, conn, ch)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp := inventoryResp{
			ChannelID: ch.ID, Name: ch.Name, SiteFamily: ch.SiteFamily,
			Accounts: inv.Accounts, Keys: inv.Keys, Groups: inv.Groups,
			CatalogModels: inv.CatalogModels, QuotaTotalUSD: inv.QuotaTotalUSD,
			LastSyncedAt: inv.LastSyncedAt,
			Anomalies:    []anomalyGroup{},
		}
		for _, a := range inv.Anomalies {
			resp.Anomalies = append(resp.Anomalies, anomalyGroup{
				Kind: a.Kind, Count: a.Count, Hint: a.Hint, Items: a.Items,
			})
		}
		s.ok(w, resp)
	})
}

// ── 账号 ──

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	chID, _ := strconv.ParseInt(r.URL.Query().Get("channel_id"), 10, 64)
	s.withConn(w, r, func(conn *pgx.Conn) {
		as, err := store.ListAccounts(r.Context(), conn, chID)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if as == nil {
			as = []store.Account{}
		}
		s.ok(w, map[string]any{"count": len(as), "items": as})
	})
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ChannelID       int64  `json:"channel_id"`
		ExternalUserID  string `json:"external_user_id"`
		BalanceGroupKey string `json:"balance_group_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.ChannelID <= 0 {
		s.fail(w, http.StatusBadRequest, "channel_id 必填")
		return
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		id, err := store.CreateAccount(r.Context(), conn, store.Account{
			ChannelID: in.ChannelID, ExternalUserID: in.ExternalUserID,
			BalanceGroupKey: in.BalanceGroupKey,
		})
		if err != nil {
			s.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
	})
}

// ── Key（FR-122，脱敏是硬要求）──

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	chID, _ := strconv.ParseInt(r.URL.Query().Get("channel_id"), 10, 64)
	s.withConn(w, r, func(conn *pgx.Conn) {
		ks, err := store.ListKeys(r.Context(), conn, chID)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if ks == nil {
			ks = []store.Key{}
		}
		s.ok(w, map[string]any{"count": len(ks), "items": ks})
	})
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AccountID   int64  `json:"account_id"`
		Secret      string `json:"secret"`
		ExternalRef string `json:"external_ref"`
		GroupRef    string `json:"group_ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.AccountID <= 0 || in.Secret == "" {
		s.fail(w, http.StatusBadRequest, "account_id 与 secret 必填")
		return
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		var gid *int64
		if in.GroupRef != "" {
			// 需要 channel_id 才能定位分组，先查账号
			as, err := store.ListAccounts(r.Context(), conn, 0)
			if err == nil {
				for _, a := range as {
					if a.ID == in.AccountID {
						if id, found, _ := store.GroupIDByRef(
							r.Context(), conn, a.ChannelID, in.GroupRef); found {
							gid = &id
						}
						break
					}
				}
			}
		}
		id, err := store.CreateKey(r.Context(), conn, in.AccountID,
			in.Secret, in.ExternalRef, gid)
		if err != nil {
			s.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		s.Logger.Info("Key 已登记", "id", id, "account_id", in.AccountID)
		w.WriteHeader(http.StatusCreated)
		// **不回显明文**（FR-094）：只确认已收到
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "secret_stored": true,
			"note": "明文不回显；列表只显示前缀（FR-094）",
		})
	})
}

func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Secret string `json:"secret"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)

	newSecret := in.Secret
	generated := false
	if newSecret == "" {
		// 未提供则生成一个 —— 便于"我就想换一个"的场景
		buf := make([]byte, 24)
		if _, err := rand.Read(buf); err != nil {
			s.fail(w, http.StatusInternalServerError, "生成随机 secret 失败")
			return
		}
		newSecret = "sk-" + hex.EncodeToString(buf)
		generated = true
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		if err := store.RotateKey(r.Context(), conn, id, newSecret); err != nil {
			s.mapNotFound(w, err)
			return
		}
		s.Logger.Info("Key 已轮换", "id", id, "generated", generated)
		resp := map[string]any{"id": id, "rotated": true}
		if generated {
			// **明文只在此处返回一次**（09 §5.0 / FR-094）
			resp["secret"] = newSecret
			resp["note"] = "明文只返回这一次，请立即保存"
		}
		s.ok(w, resp)
	})
}

func (s *Server) disableKey(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		if err := store.DisableKey(r.Context(), conn, id); err != nil {
			s.mapNotFound(w, err)
			return
		}
		s.ok(w, map[string]any{"id": id, "status": "revoked"})
	})
}

func (s *Server) keyUsage(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	from := time.Now().Add(-30 * 24 * time.Hour)
	to := time.Now()
	if v := r.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			from = t
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			to = t
		}
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		// 需要 channel_id 定位快照
		ks, err := store.ListKeys(r.Context(), conn, 0)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		var chID int64
		for _, k := range ks {
			if k.ID == id {
				chID = k.ChannelID
				break
			}
		}
		if chID == 0 {
			s.fail(w, http.StatusNotFound, fmt.Sprintf("Key %d 不存在", id))
			return
		}
		pts, err := store.KeyUsageHistory(r.Context(), conn, chID, id, from, to)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if pts == nil {
			pts = []store.KeyUsagePoint{}
		}
		s.ok(w, map[string]any{"key_id": id, "count": len(pts), "points": pts})
	})
}

// ── 分组与目录 ──

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	chID, _ := strconv.ParseInt(r.URL.Query().Get("channel_id"), 10, 64)
	s.withConn(w, r, func(conn *pgx.Conn) {
		gs, err := store.ListChannelGroups(r.Context(), conn, chID)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if gs == nil {
			gs = []store.ChannelGroup{}
		}
		s.ok(w, map[string]any{"count": len(gs), "items": gs})
	})
}

func (s *Server) groupModels(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		ms, err := store.ListGroupModels(r.Context(), conn, id)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if ms == nil {
			ms = []string{}
		}
		s.ok(w, map[string]any{"group_id": id, "count": len(ms), "models": ms})
	})
}

func (s *Server) channelCatalog(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	staleOnly := r.URL.Query().Get("stale") == "true"
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	// unit 按计价口径筛选。没有它的话按次计价那一段实际**不可达**：
	// ListCatalog 按 billing_unit 字母序分段（per_1m_token < per_call），
	// 真实渠道实测 1161 条倍率 + 208 条按次，翻页翻到第 24 页才见到第一条按次。
	// 而"跨段不可直接比大小"的告警要有意义，前提是两段都看得到。
	// 空值段用 unit=unknown 选（billing_unit IS NULL，015 迁移前的存量）。
	unit := strings.TrimSpace(r.URL.Query().Get("unit"))
	limit, offset := parsePaging(r.URL.Query())

	rounds, interval := 3, 12
	if s.Snapshot != nil {
		snap := s.Snapshot()
		if v, err := snap.Int("catalog_missing_rounds"); err == nil && v > 0 {
			rounds = v
		}
		if v, err := snap.Int("collector_catalog_interval_h"); err == nil && v > 0 {
			interval = v
		}
	}

	s.withConn(w, r, func(conn *pgx.Conn) {
		all, err := store.ListCatalog(r.Context(), conn, id, staleOnly, rounds, interval)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		filtered, units := filterCatalog(all, q, unit)
		total := len(filtered)
		if offset > total {
			offset = total
		}
		end := offset + limit
		if end > total {
			end = total
		}
		page := filtered[offset:end]
		if page == nil {
			page = []store.CatalogEntry{}
		}
		s.ok(w, map[string]any{
			"channel_id": id, "total": total, "limit": limit, "offset": offset,
			"unit": unit, "units": units,
			"items": page,
		})
	})
}

// ── 辅助 ──

// filterCatalog 按模型名与计价口径筛目录，并返回**筛分段之前**的各口径行数。
//
// units 要在分段筛选前算：界面靠它显示"倍率 1161 / 按次 208"这样的分段规模，
// 筛完再数就只剩当前段，切到某段后其余段的按钮会自己消失，出不去。
//
// 不改原切片：调用方（以及未来的缓存层）可能还要用 all，
// 就地压缩虽然省一次分配，但会把 ListCatalog 的返回值改成筛后结果。
func filterCatalog(all []store.CatalogEntry, q, unit string) (
	[]store.CatalogEntry, map[string]int,
) {
	// 名称筛选在应用层做：目录规模是数百到一千多行，不值得为它建 trigram 索引
	filtered := make([]store.CatalogEntry, 0, len(all))
	for _, e := range all {
		if q == "" || strings.Contains(strings.ToLower(e.ModelName), q) {
			filtered = append(filtered, e)
		}
	}
	units := map[string]int{}
	for _, e := range filtered {
		units[unitKey(e.BillingUnit)]++
	}
	if unit != "" {
		kept := filtered[:0]
		for _, e := range filtered {
			if unitKey(e.BillingUnit) == unit {
				kept = append(kept, e)
			}
		}
		filtered = kept
	}
	return filtered, units
}

// unitKey 把计价口径归一成筛选用的键。NULL 归到 "unknown" 而不是空串：
// 空串在 query 里与"未筛选"无从区分，会让 ?unit= 既像选空值段又像不筛。
func unitKey(u *string) string {
	if u == nil {
		return "unknown"
	}
	return *u
}

// 分页上限。真实渠道目录实测最大 1369 个模型，故上限不能太小。
const (
	pagingDefault = 100
	pagingMax     = 1000
)

// parsePaging 解析 limit/offset。
//
// 超上限**截到上限**而非退回默认值：`?limit=1369` 若静默返回 100 行，
// 运维会据此认为"这渠道只有 100 个模型"——**静默截断比报错更坏**，
// 因为它给出的是一个看起来完整的错答案。截到 1000 后配合响应里的 total
// 能看出还有下一页；退回 100 则连"被截了"都无从察觉。
// 非法值（负数、非数字）走默认值，不报错：分页参数不是业务语义。
func parsePaging(q url.Values) (limit, offset int) {
	limit, offset = pagingDefault, 0
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = min(v, pagingMax)
	}
	if v, err := strconv.Atoi(q.Get("offset")); err == nil && v >= 0 {
		offset = v
	}
	return limit, offset
}

func (s *Server) withConn(w http.ResponseWriter, r *http.Request, fn func(*pgx.Conn)) {
	conn, release, err := s.DB.Acquire(r.Context())
	if err != nil {
		s.fail(w, http.StatusServiceUnavailable, "获取连接失败: "+err.Error())
		return
	}
	defer release()
	fn(conn)
}

func (s *Server) pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.fail(w, http.StatusBadRequest, "路径 id 非法")
		return 0, false
	}
	return id, true
}

func (s *Server) mapNotFound(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.fail(w, http.StatusNotFound, err.Error())
		return
	}
	s.fail(w, http.StatusInternalServerError, err.Error())
}

func (s *Server) failWith(w http.ResponseWriter, code int, msg string, extra map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	body := map[string]any{"error": msg}
	for k, v := range extra {
		body[k] = v
	}
	_ = json.NewEncoder(w).Encode(body)
}

// ── 采集凭证（04 §5，一期明文 FR-113）──

func (s *Server) saveCredential(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ChannelID      int64  `json:"channel_id"`
		AccessToken    string `json:"access_token"`
		RefreshToken   string `json:"refresh_token"`
		Username       string `json:"username"`
		Password       string `json:"password"`
		ExternalUserID string `json:"external_user_id"`
		UserIDHeader   string `json:"user_id_header_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.ChannelID <= 0 {
		s.fail(w, http.StatusBadRequest, "channel_id 必填")
		return
	}

	s.withConn(w, r, func(conn *pgx.Conn) {
		ch, err := store.GetChannel(r.Context(), conn, in.ChannelID)
		if err != nil {
			s.mapNotFound(w, err)
			return
		}
		fam := collector.Family(ch.SiteFamily)
		reg, ok := collector.Lookup(fam)
		if !ok {
			s.fail(w, http.StatusBadRequest,
				fmt.Sprintf("渠道站型为 %q，无法确定凭证形态：请先探测站型", ch.SiteFamily))
			return
		}
		// 必需字段校验与 cred_type 选型**同一次调用**（collector 的注册表）。
		// 原先它们是两个各自按家族分流的结构（一个 switch + 一张 map），
		// 而漏改选型那张 map 的后果是 cred_type 空串进库 —— 校验绿、采集时才炸。
		// 缺字段提前拒绝：缺了必然在采集时 401，事后从日志里查比现在报贵得多。
		credType, err := reg.CredTypeFor(
			in.AccessToken != "",
			in.ExternalUserID != "",
			in.Username != "" && in.Password != "",
		)
		if err != nil {
			s.fail(w, http.StatusBadRequest, err.Error())
			return
		}

		if s.SaveCredential == nil {
			s.fail(w, http.StatusServiceUnavailable, "凭证存储未配置")
			return
		}
		if err := s.SaveCredential(r.Context(), conn, collector.Credential{
			ChannelID: in.ChannelID, Family: fam, CredType: credType,
			BaseURL:     ch.BaseURL,
			AccessToken: in.AccessToken, RefreshToken: in.RefreshToken,
			Username: in.Username, Password: in.Password,
			ExternalUserID: in.ExternalUserID, UserIDHeaderName: in.UserIDHeader,
		}); err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Logger.Info("采集凭证已登记", "channel_id", in.ChannelID, "cred_type", credType)
		// **不回显任何凭证内容**（FR-094 同源纪律）
		s.ok(w, map[string]any{
			"channel_id": in.ChannelID, "cred_type": credType, "stored": true,
			"note": "凭证内容不回显；可点“立即采集”验证是否可用",
		})
	})
}

func (s *Server) listCredentials(w http.ResponseWriter, r *http.Request) {
	s.withConn(w, r, func(conn *pgx.Conn) {
		rows, err := conn.Query(r.Context(), `
SELECT channel_id, site_family, cred_type, status,
       token_expires_at, updated_at,
       (access_token IS NOT NULL) AS has_token,
       (password IS NOT NULL) AS has_password
  FROM collector_credentials ORDER BY channel_id`)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()
		type item struct {
			ChannelID   int64      `json:"channel_id"`
			SiteFamily  string     `json:"site_family"`
			CredType    string     `json:"cred_type"`
			Status      string     `json:"status"`
			ExpiresAt   *time.Time `json:"token_expires_at,omitempty"`
			UpdatedAt   time.Time  `json:"updated_at"`
			HasToken    bool       `json:"has_token"`
			HasPassword bool       `json:"has_password"`
		}
		out := []item{}
		for rows.Next() {
			var i item
			if err := rows.Scan(&i.ChannelID, &i.SiteFamily, &i.CredType, &i.Status,
				&i.ExpiresAt, &i.UpdatedAt, &i.HasToken, &i.HasPassword); err != nil {
				s.fail(w, http.StatusInternalServerError, err.Error())
				return
			}
			out = append(out, i)
		}
		// 遍历中途出错（连接断了、服务端流式报错）时 rows.Next() 只是返回 false，
		// 与"正常读完"无从区分。不查这一下就会返回 200 + 一个**被截断**的列表，
		// 而 count 看起来还很权威 —— 运维照着少掉的条数排查，查不出任何异常。
		if err := rows.Err(); err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		// 只报"有没有"，不报内容
		s.ok(w, map[string]any{"count": len(out), "items": out})
	})
}
