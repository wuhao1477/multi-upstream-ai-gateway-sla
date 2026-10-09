package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// all-api-hub 的 WebDAV 定时同步。
//
// 一轮 = 取回远端备份 → 解密（若加密）→ 按 apply_mode 走一遍**既有的**导入管线。
// 复用 ImportHub 而不是另写一条落库路径：那条路里的"以探测为准、同 base_url 跳过、
// 明文读取预算、四处写入同生共死"每一条都是踩出来的。
//
// 为什么跑在 sla-core 而不是 collector 进程：导入要 Detect + ImportKeys，这两个
// 只在 sla-core 里装配（cmd/sla-core/main.go）。把它们再往 collector 里接一遍，
// 换来的只是"看起来更像后台任务"。

// hubSyncHTTPTimeout 单次取备份的超时。WebDAV 那头可能是别人家的网盘，
// 也可能是内网 NAS —— 给得比采集宽，但不能没有。
const hubSyncHTTPTimeout = 60 * time.Second

// hubSyncMinInterval 与迁移里的 CHECK 一致。两处都要有：CHECK 挡住直接写库，
// 这里挡住 API 传进来的值（报错文案比约束违反好读）。
const hubSyncMinInterval = 5

// hubSyncTick 是轮询那一行配置的节奏，**不是**同步间隔。
//
// 间隔存在库里、随时可能被界面改小，所以循环不能按 interval 睡死一觉 ——
// 那样改配置要等到下一觉醒来才生效。每分钟醒一次看看到点没有，代价是一条
// SELECT，换来的是"改完立刻按新节奏走"。
const hubSyncTick = time.Minute

// hubSyncClient 取 WebDAV 用的 HTTP 客户端。
//
// 不走 collector.Client：那个带按上游 host 的跨进程限速（collector_host_rate_limits），
// 而 WebDAV 不是上游站点，把它挤进同一套时隙只会让采集互相等。
func (s *Server) hubSyncClient() *http.Client {
	if s.HubHTTP != nil {
		return s.HubHTTP
	}
	return &http.Client{
		Timeout:       hubSyncHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// RunHubSync 跑一轮同步，并把这一轮记进 hub_sync_runs。
//
// forceImport 为 true 时忽略 apply_mode 直接落库 —— 界面上那个"立即导入"按钮用，
// 定时器永远传 false（定时器只按配置走，不自己加码）。
func (s *Server) RunHubSync(
	ctx context.Context, trigger string, forceImport bool,
) (*collector.HubImportResult, error) {
	startedAt := time.Now()
	conn, release, err := s.DB.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取连接失败: %w", err)
	}
	cfg, err := store.LoadHubSyncConfig(ctx, conn)
	release()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.WebDAVURL) == "" {
		return nil, errors.New("还没有配置 WebDAV 地址")
	}

	res, runErr := s.runHubSyncOnce(ctx, cfg, forceImport)

	// 成败都要记：失败那条是界面上唯一能看见"为什么一直没同步"的地方。
	run := store.HubSyncRun{
		StartedAt:  startedAt,
		FinishedAt: time.Now(),
		Trigger:    trigger,
		Applied:    forceImport || cfg.ApplyMode == store.HubSyncModeImport,
	}
	if runErr != nil {
		run.Error = runErr.Error()
		// 失败的那轮没有落库，不管配置是什么 —— 记成 applied=true 会让历史里
		// 那一行看起来"建过渠道了"，而其实一个都没建。
		run.Applied = false
	}
	if res != nil {
		if b, err := json.Marshal(res); err == nil {
			run.Result = b
		}
	}
	if conn, release, err := s.DB.Acquire(ctx); err == nil {
		if _, err := store.InsertHubSyncRun(ctx, conn, run); err != nil {
			s.Logger.Warn("记录 all-api-hub 同步结果失败", "err", err)
		}
		release()
	}
	return res, runErr
}

// runHubSyncOnce 是取回 → 解密 → 导入这三步本身，不碰 last_* 那几列。
func (s *Server) runHubSyncOnce(
	ctx context.Context, cfg store.HubSyncConfig, forceImport bool,
) (*collector.HubImportResult, error) {
	raw, err := collector.FetchHubBackup(ctx, s.hubSyncClient(), collector.HubWebDAVConfig{
		URL:      cfg.WebDAVURL,
		Username: cfg.WebDAVUsername,
		Password: cfg.WebDAVPassword,
	})
	if err != nil {
		return nil, err
	}
	plain, err := collector.DecryptHubBackup(raw, cfg.BackupPassword)
	if err != nil {
		return nil, err
	}
	backup, err := collector.ParseHubBackup(strings.NewReader(string(plain)))
	if err != nil {
		return nil, err
	}
	dryRun := !forceImport && cfg.ApplyMode != store.HubSyncModeImport
	res, err := s.ImportHub(ctx, backup, dryRun)
	if err == nil {
		s.disableHubRemoved(ctx, res, dryRun)
	}
	return res, err
}

// disableHubRemoved 停用"本地还在、备份里已经没有"的 hub 渠道；dry_run 只报不改。
//
// 只挂在 WebDAV 同步上：那份备份由扩展维护、是全量；手工上传的文件可能只是
// 一部分，拿它判"已移除"会把其余渠道全停掉。
func (s *Server) disableHubRemoved(ctx context.Context, res *collector.HubImportResult, dryRun bool) {
	conn, release, err := s.DB.Acquire(ctx)
	if err != nil {
		s.Logger.Warn("停用备份里已移除的渠道失败", "err", err)
		return
	}
	defer release()
	keep := keepBaseURLs(res)
	var gone []store.Channel
	if dryRun {
		gone, err = store.HubRemovedChannels(ctx, conn, keep)
	} else {
		gone, err = store.DisableHubRemovedChannels(ctx, conn, keep,
			"all-api-hub 备份里已移除（定时同步自动停用，可手动重新启用）")
	}
	if err != nil {
		s.Logger.Warn("停用备份里已移除的渠道失败", "err", err)
		return
	}
	appendRemovedItems(res, gone)
}

// StartHubSync 起定时同步循环，直到 ctx 结束。
//
// 多实例安全靠 pg advisory 锁：两个 sla-core 副本同时到点时只有一个真的跑，
// 另一个这一轮跳过。没有它的话，两边会同时探测同一批站点、同时建同一批渠道。
func (s *Server) StartHubSync(ctx context.Context) {
	ticker := time.NewTicker(hubSyncTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.hubSyncTickOnce(ctx)
		}
	}
}

func (s *Server) hubSyncTickOnce(ctx context.Context) {
	conn, release, err := s.DB.Acquire(ctx)
	if err != nil {
		return
	}
	cfg, err := store.LoadHubSyncConfig(ctx, conn)
	release()
	if err != nil || !cfg.Enabled || strings.TrimSpace(cfg.WebDAVURL) == "" {
		return
	}
	if !hubSyncDue(cfg, time.Now()) {
		return
	}
	// 与手动触发共用进程内标记：界面才看得到"在跑"，手动点击拿到 409 而不是空跑。
	if !s.hubSyncBusy.CompareAndSwap(false, true) {
		return
	}
	defer s.hubSyncBusy.Store(false)

	locked, unlock, err := s.tryHubSyncLock(ctx)
	if err != nil || !locked {
		return
	}
	defer unlock()

	s.Logger.Info("all-api-hub 定时同步开始", "apply_mode", cfg.ApplyMode)
	res, err := s.RunHubSync(ctx, store.HubSyncTriggerSchedule, false)
	if err != nil {
		s.Logger.Warn("all-api-hub 定时同步失败", "err", err)
		return
	}
	s.Logger.Info("all-api-hub 定时同步完成",
		"total", res.Total, "imported", res.Imported, "skipped", res.Skipped,
		"failed", res.Failed, "apply_mode", cfg.ApplyMode)
}

// hubSyncDue 判断这一轮到点没有。从未跑过 = 立刻该跑。
func hubSyncDue(cfg store.HubSyncConfig, now time.Time) bool {
	if cfg.LastRunAt == nil {
		return true
	}
	interval := time.Duration(cfg.IntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = time.Duration(hubSyncMinInterval) * time.Minute
	}
	return !now.Before(cfg.LastRunAt.Add(interval))
}

// tryHubSyncLock 拿一把会话级 advisory 锁。拿不到说明别的实例正在跑这一轮。
//
// 锁必须与取它的那条连接同生共死，所以 release 要在解锁之后 —— 返回的闭包
// 把这个顺序封住，调用方 defer 一下就不会写反。
func (s *Server) tryHubSyncLock(ctx context.Context) (bool, func(), error) {
	conn, release, err := s.DB.Acquire(ctx)
	if err != nil {
		return false, func() {}, err
	}
	var locked bool
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtext('hub-sync'))`).Scan(&locked); err != nil {
		release()
		return false, func() {}, err
	}
	if !locked {
		release()
		return false, func() {}, nil
	}
	return true, func() {
		// 解锁用同一条连接。用 context.WithoutCancel：ctx 已经取消时（进程退出）
		// 这条 UNLOCK 仍要发出去，否则锁要等连接被回收才释放。
		if _, err := conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock(hashtext('hub-sync'))`); err != nil {
			s.Logger.Warn("释放 all-api-hub 同步锁失败", "err", err)
		}
		release()
	}, nil
}

// HubSyncRoutes 注册配置读写与手动触发。
func (s *Server) HubSyncRoutes(mux *http.ServeMux) {
	mux.Handle("GET /admin/hub-sync", s.requireToken(http.HandlerFunc(s.getHubSync)))
	mux.Handle("PUT /admin/hub-sync", s.requireToken(http.HandlerFunc(s.putHubSync)))
	mux.Handle("POST /admin/hub-sync/run", s.requireToken(http.HandlerFunc(s.runHubSyncNow)))
	mux.Handle("GET /admin/hub-sync/runs", s.requireToken(http.HandlerFunc(s.listHubSyncRuns)))
	mux.Handle("GET /admin/hub-sync/runs/{id}",
		s.requireToken(http.HandlerFunc(s.getHubSyncRun)))
}

// listHubSyncRuns 历史列表（倒序，不含逐站明细）。
func (s *Server) listHubSyncRuns(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			s.fail(w, http.StatusBadRequest, "limit 必须是正整数")
			return
		}
		limit = n
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		runs, err := store.ListHubSyncRuns(r.Context(), conn, limit)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		items := make([]hubSyncRunView, 0, len(runs))
		for _, run := range runs {
			items = append(items, hubSyncRunViewOf(run))
		}
		s.ok(w, map[string]any{"count": len(items), "items": items})
	})
}

// getHubSyncRun 单条记录，含逐站明细 —— 界面上点开一行看的就是它。
func (s *Server) getHubSyncRun(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		run, err := store.GetHubSyncRun(r.Context(), conn, id)
		if err != nil {
			s.mapNotFound(w, err)
			return
		}
		s.ok(w, hubSyncRunViewOf(run))
	})
}

// hubSyncView 是回给界面的形态。**两个密码只报有没有**（FR-094 同源纪律）。
type hubSyncView struct {
	WebDAVURL         string `json:"webdav_url"`
	WebDAVUsername    string `json:"webdav_username"`
	HasWebDAVPassword bool   `json:"has_webdav_password"`
	HasBackupPassword bool   `json:"has_backup_password"`
	Enabled           bool   `json:"enabled"`
	IntervalMinutes   int    `json:"interval_minutes"`
	ApplyMode         string `json:"apply_mode"`
	// 上次同步的时刻。取自 hub_sync_runs 最新一行，配置表上不存第二份。
	// 详情与历史走 /admin/hub-sync/runs。
	LastRunAt *time.Time `json:"last_run_at,omitempty"`
	// Running = 本进程此刻正在跑一轮。
	//
	// 界面靠它显示"正在同步"并轮询到结束 —— 一轮要探测上百个站点（实测 110 站
	// 108 秒），没有这一位的话人只能对着一个变灰的按钮干等，不知道是在跑还是卡了。
	// **进程级**：另一个副本在跑时这里是 false，那种情况由 advisory 锁挡住，
	// 界面会从历史里看到那一轮。
	Running bool `json:"running"`
}

// hubSyncViewNow 是"配置 + 此刻在不在跑"。
func (s *Server) hubSyncViewNow(c store.HubSyncConfig) hubSyncView {
	v := hubSyncViewOf(c)
	v.Running = s.hubSyncBusy.Load()
	return v
}

// hubSyncViewOf 只管把配置转成视图；Running 由调用方补 —— 它是进程状态，
// 不属于配置，硬塞进这个纯函数会逼它收一个 *Server。
func hubSyncViewOf(c store.HubSyncConfig) hubSyncView {
	v := hubSyncView{
		WebDAVURL:         c.WebDAVURL,
		WebDAVUsername:    c.WebDAVUsername,
		HasWebDAVPassword: c.WebDAVPassword != "",
		HasBackupPassword: c.BackupPassword != "",
		Enabled:           c.Enabled,
		IntervalMinutes:   c.IntervalMinutes,
		ApplyMode:         c.ApplyMode,
		LastRunAt:         c.LastRunAt,
	}
	return v
}

// hubSyncRunView 是一条同步记录的形态。
//
// Result 在列表里已被剥掉 items（store.ListHubSyncRuns 里做的），详情接口才带全 ——
// 几十行明细一起回等于把上兆 JSON 塞进一个列表响应。
type hubSyncRunView struct {
	ID         int64     `json:"id"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// 毫秒。前端要显示"这轮跑了多久"，算一次比让每个调用方各减一遍稳妥。
	ElapsedMS int64           `json:"elapsed_ms"`
	Trigger   string          `json:"trigger"`
	Applied   bool            `json:"applied"`
	Error     string          `json:"error,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
}

func hubSyncRunViewOf(r store.HubSyncRun) hubSyncRunView {
	v := hubSyncRunView{
		ID:         r.ID,
		StartedAt:  r.StartedAt,
		FinishedAt: r.FinishedAt,
		ElapsedMS:  r.FinishedAt.Sub(r.StartedAt).Milliseconds(),
		Trigger:    r.Trigger,
		Applied:    r.Applied,
		Error:      r.Error,
	}
	if len(r.Result) > 0 && string(r.Result) != "null" {
		v.Result = r.Result
	}
	return v
}

func (s *Server) getHubSync(w http.ResponseWriter, r *http.Request) {
	s.withConn(w, r, func(conn *pgx.Conn) {
		cfg, err := store.LoadHubSyncConfig(r.Context(), conn)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.ok(w, s.hubSyncViewNow(cfg))
	})
}

func (s *Server) putHubSync(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WebDAVURL       string `json:"webdav_url"`
		WebDAVUsername  string `json:"webdav_username"`
		WebDAVPassword  string `json:"webdav_password"`
		BackupPassword  string `json:"backup_password"`
		Enabled         bool   `json:"enabled"`
		IntervalMinutes int    `json:"interval_minutes"`
		ApplyMode       string `json:"apply_mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	in.WebDAVURL = strings.TrimSpace(in.WebDAVURL)
	// 地址在这里就要校验，而不是等定时器跑起来才在日志里报错 ——
	// 填错的人正站在界面前，此刻告诉他最便宜。
	//
	// ⚠️ 刻意**不**走 validateBaseURL 那套 SSRF 校验：它拦私网地址，而自建
	// WebDAV 十有八九就在内网（NAS、群晖、局域网里的 nginx）。这个地址是管理员
	// 在鉴权之后自己填的，与导入文件里那一百个陌生站点不是同一类输入。
	if in.WebDAVURL != "" {
		if _, err := collector.ResolveHubBackupURL(in.WebDAVURL); err != nil {
			s.fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if in.Enabled && in.WebDAVURL == "" {
		s.fail(w, http.StatusBadRequest, "要开启定时同步得先填 WebDAV 地址")
		return
	}
	if in.IntervalMinutes < hubSyncMinInterval {
		s.fail(w, http.StatusBadRequest,
			fmt.Sprintf("同步间隔不能小于 %d 分钟", hubSyncMinInterval))
		return
	}
	if in.ApplyMode != store.HubSyncModeReport && in.ApplyMode != store.HubSyncModeImport {
		s.fail(w, http.StatusBadRequest, "apply_mode 只能是 report 或 import")
		return
	}

	s.withConn(w, r, func(conn *pgx.Conn) {
		if err := store.SaveHubSyncConfig(r.Context(), conn, store.HubSyncConfig{
			WebDAVURL:       in.WebDAVURL,
			WebDAVUsername:  in.WebDAVUsername,
			WebDAVPassword:  in.WebDAVPassword,
			BackupPassword:  in.BackupPassword,
			Enabled:         in.Enabled,
			IntervalMinutes: in.IntervalMinutes,
			ApplyMode:       in.ApplyMode,
		}); err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		cfg, err := store.LoadHubSyncConfig(r.Context(), conn)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Logger.Info("all-api-hub 同步配置已更新",
			"enabled", cfg.Enabled, "interval_min", cfg.IntervalMinutes,
			"apply_mode", cfg.ApplyMode)
		s.ok(w, s.hubSyncViewNow(cfg))
	})
}

// runHubSyncNow 手动跑一轮。?apply=true 强制落库（界面上的"立即导入"）。
// runHubSyncNow 起一轮同步并**立刻返回 202**，结果去 /admin/hub-sync/runs 看。
//
// 从"同步等结果"改成"起了就走"（2026-09-16），修三个缺陷：
//
//  1. **整轮跑在 `r.Context()` 上** —— 它在浏览器切走页面、关标签页、或反向代理
//     超时那一刻就被取消。而这一轮要探测备份里的上百个站点（实测 110 站 108 秒），
//     被取消时已经建了一部分渠道：留下**半拉子导入**。更糟的是 RunHubSync 末尾
//     写 hub_sync_runs 那一步用的也是这个 ctx，于是连"这轮失败了"都记不下来 ——
//     半个状态 + 零条历史，事后完全无从追查。
//  2. **手动触发没有任何互斥**。定时那条路有 advisory 锁（hubSyncTickOnce），
//     手动这条直接调 RunHubSync。开两个标签页各点一次，就是两轮并发的百站导入
//     互相抢同一批渠道 —— 界面上的 `busy` 只管得住它自己那个标签页。
//  3. 前端因此只能干等两分钟，而且不知道自己在等什么（见 HubSyncCard）。
//
// 结果不在响应里，因为它本来就不在响应里才对：每一轮都已经完整落进 hub_sync_runs
// （含逐站明细），界面读那张表即可 —— 让同一份结果有两个来源才是分叉。
func (s *Server) runHubSyncNow(w http.ResponseWriter, r *http.Request) {
	apply := r.URL.Query().Get("apply") == "true"
	// **配置问题要当场报**，别让它变成一句 202 + 一条只在日志里的错误。
	// 改成后台跑之后，RunHubSync 里"还没有配置 WebDAV 地址"那条早退发生在
	// goroutine 里 —— 界面会先收到 202、再发现历史里什么都没多出来，
	// 而那正是最难排查的一种反馈。这一条 SELECT 很快，放在同步路径上不亏。
	conn, release, err := s.DB.Acquire(r.Context())
	if err != nil {
		s.fail(w, http.StatusServiceUnavailable, "获取连接失败: "+err.Error())
		return
	}
	cfg, err := store.LoadHubSyncConfig(r.Context(), conn)
	release()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if strings.TrimSpace(cfg.WebDAVURL) == "" {
		s.fail(w, http.StatusBadRequest, "还没有配置 WebDAV 地址")
		return
	}
	if !s.hubSyncBusy.CompareAndSwap(false, true) {
		s.fail(w, http.StatusConflict, "已有一轮 all-api-hub 同步在执行，等它跑完再点")
		return
	}
	// 跨实例互斥仍走 advisory 锁 —— 上面那个布尔只管得住本进程。
	// **在请求里同步取**：拿不到就当场 409。放进 goroutine 的话，先回 202、
	// 再在后台静默跳过，界面轮询到"不在跑"后读到的是上一轮的历史。
	// 取锁用请求的 ctx（连接池耗尽时随请求取消，不让 handler 无限挂着）；
	// 锁绑在连接上、unlock 用 WithoutCancel，所以请求结束不影响后台那一轮。
	locked, unlock, err := s.tryHubSyncLock(r.Context())
	if err != nil || !locked {
		s.hubSyncBusy.Store(false)
		if err != nil {
			s.fail(w, http.StatusServiceUnavailable, "取 all-api-hub 同步锁失败: "+err.Error())
			return
		}
		s.fail(w, http.StatusConflict, "另一实例正在跑 all-api-hub 同步，等它跑完再点")
		return
	}
	go func() {
		defer s.hubSyncBusy.Store(false)
		defer unlock()
		// 后台 ctx：这一轮要活得比触发它的请求久（见上面第 1 条）。
		if _, err := s.RunHubSync(s.backgroundCtx(), store.HubSyncTriggerManual, apply); err != nil {
			// 失败照样进了 hub_sync_runs（RunHubSync 成败都记），界面从那儿读。
			s.Logger.Warn("all-api-hub 手动同步失败", "err", err)
		}
	}()
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"started": true, "apply": apply,
		"note": "已在后台开始；进度看 GET /admin/hub-sync 的 running，结果看 /admin/hub-sync/runs",
	})
}
