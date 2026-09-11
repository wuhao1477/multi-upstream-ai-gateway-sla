// Command sla-core 是主服务入口（数据平面 + 管理平面同进程）。
//
// 当前交付阶段 P1（PRD §2.1.0）：**只有管理平面**。
// 数据平面 /v1/* 属 P2，本进程此阶段不监听它 —— 这是范围而非缺失
// （ISSUE-005 §2 的"不做"清单）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/admin"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/bootstrap"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collection"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/config"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/health"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// version 由构建时注入（-ldflags "-X main.version=..."）。
var version = "dev"

func main() {
	var (
		addr     = flag.String("addr", envOr("SLA_ADDR", ":8080"), "监听地址（管理平面 + /healthz）")
		dsn      = flag.String("dsn", os.Getenv("DATABASE_URL"), "PG 连接串")
		readOnly = flag.Bool("read-only", false, "只读启动：跳过迁移和种子，并让 PG 会话拒绝写入")
		collect  = flag.Bool("collector", os.Getenv("SLA_COLLECTOR") != "",
			"在本进程内跑周期采集（单容器部署用，省掉独立 collector 容器）")
		probe   = flag.Bool("healthcheck", false, "探测本进程 /healthz 后按结果退出（容器 healthcheck 用）")
		showVer = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Println(version)
		return
	}

	if *probe {
		if err := healthcheck(*addr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if err := run(*addr, *dsn, *readOnly, *collect, logger); err != nil {
		logger.Error("退出", "err", err)
		os.Exit(1)
	}
}

func run(addr, dsn string, readOnly, collect bool, logger *slog.Logger) error {
	if dsn == "" {
		return errors.New("缺少 DSN：设 DATABASE_URL 或用 -dsn")
	}

	startCtx, cancelStart := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelStart()

	newPool := store.NewPool
	if readOnly {
		newPool = store.NewReadOnlyPool
	}
	pool, err := newPool(startCtx, dsn)
	if err != nil {
		return fmt.Errorf("连接 PG: %w", err)
	}
	defer pool.Close()

	// bootstrap 与 cmd/migrate 走同一段代码：手动迁移能过、启动却失败的
	// 情形因此不存在（cmd/migrate 的注释）。
	conn, release, err := pool.Acquire(startCtx)
	if err != nil {
		return err
	}
	var res *bootstrap.Result
	if readOnly {
		res, err = bootstrap.LoadReadOnly(startCtx, conn, logger)
	} else {
		res, err = bootstrap.Run(startCtx, conn, logger)
	}
	release()
	if err != nil {
		return fmt.Errorf("初始化: %w", err)
	}

	// 快照用 atomic 持有：apply 配置后需原地替换，而决策路径只读它（01 §5）。
	var snap atomic.Pointer[config.Snapshot]
	snap.Store(res.Snapshot)

	// 配置变更后重建快照，使新值对决策路径生效（09 §2 末条）。
	// 失败只记日志不中断服务：旧快照仍然可用，比"因为读不到新配置而挂掉"好。
	rebuild := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, rel, err := pool.Acquire(ctx)
		if err != nil {
			logger.Error("快照重建取连接失败", "err", err)
			return
		}
		defer rel()
		values, err := store.LoadConfigValues(ctx, c)
		if err != nil {
			logger.Error("快照重建读配置失败", "err", err)
			return
		}
		ns := config.NewSnapshot(values)
		if err := ns.Validate(); err != nil {
			logger.Error("新快照校验失败，保留旧快照", "err", err)
			return
		}
		snap.Store(ns)
		logger.Info("配置快照已重建")
	}

	adminToken := os.Getenv("ADMIN_TOKEN")
	if adminToken == "" {
		// 不 fatal：/healthz 仍应可服务（LB 需要它）。管理平面自己会返 503。
		logger.Warn("ADMIN_TOKEN 未设置，管理平面将拒绝全部请求")
	}

	// 采集器：三家族适配器 + 落库 sink（09 §5.0bis 的编排在 collector.Syncer）
	interval := 200 * time.Millisecond
	if v, err := snap.Load().Int("collector_request_interval_ms"); err == nil && v > 0 {
		interval = time.Duration(v) * time.Millisecond
	}
	hc := collector.NewClient(interval)
	if !readOnly {
		hc.WaitHost = store.NewHostRequestLimiter(pool).Wait
	}
	runner := collection.NewRunner(pool, hc)

	mux := http.NewServeMux()
	mux.Handle("/healthz", &health.Handler{
		DB:          pool,
		HasSnapshot: func() bool { return snap.Load() != nil },
	})

	srv := admin.NewServer(pool, adminToken, logger, rebuild)
	srv.Snapshot = func() *config.Snapshot { return snap.Load() }
	srv.Detect = func(ctx context.Context, baseURL string) (collector.DetectResult, error) {
		return collector.Detect(ctx, hc, baseURL)
	}
	srv.Sync = func(ctx context.Context, ch store.Channel) (*collector.SyncResult, error) {
		return runner.Sync(ctx, ch, nil)
	}
	credStore := store.NewCredentialStore(pool)
	loadKeyAccount := func(
		ctx context.Context, conn *pgx.Conn, channelID, accountID int64,
	) (store.Channel, collector.Credential, error) {
		ch, err := store.GetChannel(ctx, conn, channelID)
		if err != nil {
			return store.Channel{}, collector.Credential{}, err
		}
		creds, err := credStore.ListByChannel(ctx, conn, ch)
		if err != nil {
			return store.Channel{}, collector.Credential{}, err
		}
		for _, cred := range creds {
			if cred.AccountID != accountID {
				continue
			}
			cred.QuotaPerUnit = store.QuotaPerUnit(ctx, conn, channelID)
			return ch, cred, nil
		}
		return store.Channel{}, collector.Credential{}, fmt.Errorf("账号 %d 没有可用采集凭证", accountID)
	}
	srv.ImportKeys = func(
		ctx context.Context, conn *pgx.Conn, channelID, accountID int64,
	) (collector.KeyImportResult, error) {
		ch, cred, err := loadKeyAccount(ctx, conn, channelID, accountID)
		if err != nil {
			return collector.KeyImportResult{}, err
		}
		return runner.ImportKeys(ctx, conn, ch, []collector.Credential{cred})
	}
	srv.ProvisionKeys = func(
		ctx context.Context, conn *pgx.Conn, channelID, accountID int64,
		request collector.KeyProvisionRequest,
	) (collector.KeyProvisionResult, error) {
		ch, cred, err := loadKeyAccount(ctx, conn, channelID, accountID)
		if err != nil {
			return collector.KeyProvisionResult{}, err
		}
		return runner.ProvisionKeys(ctx, conn, ch, cred, request)
	}
	// 注入收 DBTX 的那一版（SaveTx，不是 Save）：管理面的写入要能被调用方
	// 收进事务。Save 那个自取连接的变体留给采集侧的续期路径（不变式 S-1）。
	srv.SaveCredential = credStore.SaveTx
	srv.SaveDetected = credStore.SaveDetected
	srv.Routes(mux)
	srv.UpstreamRoutes(mux)
	srv.ImportRoutes(mux)
	srv.WebRoutes(mux)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 优雅关闭：SIGTERM 后先让 Caddy 摘除本实例，再排空在途请求。
	// 这对 AC-27（停一个实例、请求全成功）是必要的 —— 硬杀会让在途请求失败。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 单容器部署：周期采集跑在本进程，省掉独立 collector 容器
	// （06 §8 开放点 3 已定"一期同二进制子命令"，这里只是少起一个容器）。
	//
	// 多实例同时开着也安全：每个渠道由 store.TryChannelSyncLock 的 PG 咨询锁
	// 排他，落败者跳过该渠道 —— 与独立 collector 容器并存时同理。
	//
	// 连接预算已经含这一路：MinPoolConns=12 = 4 worker × 2 + 给 admin 请求
	// 留的 4 条（store.MinPoolConns 注释），本来就是按同进程共池算的。
	//
	// read-only 实例不启动：采集是写路径，会被 default_transaction_read_only 拒掉。
	if collect && !readOnly {
		periods, err := collection.IntervalsFromSnapshot(snap.Load())
		if err != nil {
			return err
		}
		svc := collection.NewService(pool, runner, collection.NewSchedule(periods), logger)
		go func() {
			logger.Info("内置采集器启动",
				"balance_interval", periods.Balance,
				"keyquota_interval", periods.KeyQuota,
				"price_interval", periods.Price,
				"catalog_interval", periods.Catalog)
			if err := svc.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("内置采集器退出", "err", err)
			}
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("sla-core 启动", "addr", addr, "version", version,
			"phase", "P1", "read_only", readOnly)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("收到关闭信号，开始优雅关闭")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

// healthcheck 让二进制探自己的 /healthz —— distroless 镜像里没有 shell 也没有
// curl/wget，容器 healthcheck 只能 exec 一个二进制，所以这件事得由它自己做。
//
// 为什么不继续用 `-version`：那只证明二进制能执行。进程卡死、PG 连接断掉、
// 配置快照没加载，`-version` 照样退 0，于是容器一直报 healthy 而服务早已不可用。
// /healthz 反映的是实例自身（进程 + PG + 快照），**不含上游渠道可用性**
// （06 §6 健康语义分层）—— 上游全挂时本实例仍应判健康。
//
// 不需要 ADMIN_TOKEN：/healthz 是匿名端点，LB 就绪探针本来就要免鉴权。
func healthcheck(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("解析监听地址 %q: %w", addr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// 固定打回环：探的是"本进程活着吗"，不是"这个地址可达吗"。
	// 监听 0.0.0.0 时用 addr 原样拼会得到 http://0.0.0.0:8080，不可移植。
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://127.0.0.1:"+port+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/healthz 返回 %d", resp.StatusCode)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
