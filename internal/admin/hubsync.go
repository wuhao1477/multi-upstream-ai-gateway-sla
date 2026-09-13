package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	return &http.Client{Timeout: hubSyncHTTPTimeout}
}

// RunHubSync 跑一轮同步，并把结果记回 hub_sync_config。
//
// forceImport 为 true 时忽略 apply_mode 直接落库 —— 界面上那个"立即导入"按钮用，
// 定时器永远传 false（定时器只按配置走，不自己加码）。
func (s *Server) RunHubSync(
	ctx context.Context, forceImport bool,
) (*collector.HubImportResult, error) {
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
	msg := ""
	if runErr != nil {
		msg = runErr.Error()
	}
	var payload json.RawMessage
	if res != nil {
		// 只留汇总计数，不留逐站明细：明细里有站点地址，而这一行会一直留在库里。
		summary := map[string]any{
			"total": res.Total, "imported": res.Imported, "skipped": res.Skipped,
			"failed": res.Failed, "family_mismatches": res.Mismatches,
			"shielded_sites": res.Shielded, "without_credential": res.NoCredential,
			"keys_found": res.KeysFound, "keys_imported": res.KeysImported,
			"applied": forceImport || cfg.ApplyMode == store.HubSyncModeImport,
		}
		if b, err := json.Marshal(summary); err == nil {
			payload = b
		}
	}
	if conn, release, err := s.DB.Acquire(ctx); err == nil {
		if err := store.RecordHubSyncRun(ctx, conn, time.Now(), msg, payload); err != nil {
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
	return s.ImportHub(ctx, backup, dryRun)
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

	locked, unlock, err := s.tryHubSyncLock(ctx)
	if err != nil || !locked {
		return
	}
	defer unlock()

	s.Logger.Info("all-api-hub 定时同步开始", "apply_mode", cfg.ApplyMode)
	res, err := s.RunHubSync(ctx, false)
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
}

// hubSyncView 是回给界面的形态。**两个密码只报有没有**（FR-094 同源纪律）。
type hubSyncView struct {
	WebDAVURL         string          `json:"webdav_url"`
	WebDAVUsername    string          `json:"webdav_username"`
	HasWebDAVPassword bool            `json:"has_webdav_password"`
	HasBackupPassword bool            `json:"has_backup_password"`
	Enabled           bool            `json:"enabled"`
	IntervalMinutes   int             `json:"interval_minutes"`
	ApplyMode         string          `json:"apply_mode"`
	LastRunAt         *time.Time      `json:"last_run_at,omitempty"`
	LastError         string          `json:"last_error,omitempty"`
	LastResult        json.RawMessage `json:"last_result,omitempty"`
}

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
		LastError:         c.LastError,
	}
	if len(c.LastResult) > 0 && string(c.LastResult) != "null" {
		v.LastResult = c.LastResult
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
		s.ok(w, hubSyncViewOf(cfg))
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
		s.ok(w, hubSyncViewOf(cfg))
	})
}

// runHubSyncNow 手动跑一轮。?apply=true 强制落库（界面上的"立即导入"）。
func (s *Server) runHubSyncNow(w http.ResponseWriter, r *http.Request) {
	res, err := s.RunHubSync(r.Context(), r.URL.Query().Get("apply") == "true")
	if err != nil {
		switch {
		case errors.Is(err, collector.ErrHubBackupNotFound):
			s.fail(w, http.StatusNotFound, err.Error())
		case errors.Is(err, collector.ErrHubBackupEncrypted),
			errors.Is(err, collector.ErrHubBackupDecrypt):
			// 4xx：远端好好地把文件给了我们，解不开是**我方配置**的问题。
			s.fail(w, http.StatusBadRequest, err.Error())
		default:
			s.fail(w, http.StatusBadGateway, err.Error())
		}
		return
	}
	s.ok(w, res)
}
