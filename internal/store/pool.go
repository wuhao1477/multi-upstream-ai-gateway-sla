package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DBTX 是"能执行一条语句的东西"。`*pgx.Conn` 与 `pgx.Tx` **都已满足**它
// （三个方法的签名在 pgx v5 里逐字相同），故本接口是纯加法：现有那些
// `conn *pgx.Conn` 的调用点一处都不用改。
//
// 为什么需要它（2026-08-29 二次评审查出）：`importOne` 要把建渠道 + 存探测 +
// 建账号 + 存凭证四处写入放进**同一个事务**，而其中两处是注入函数
// （`Server.SaveDetected` / `SaveCredential`），原先各自 `Pool.Acquire` 取
// **另一条连接**独立提交 —— 于是"给 conn 包一层 tx"根本盖不住它们，
// 中途失败会留下不可自愈的半成品（渠道行在、凭证没有，重导又按 base_url 判为
// 已存在而跳过）。让这些函数收 DBTX 而非 *pgx.Conn，事务才能一路传下去。
//
// ⚠️ 只放 Exec/Query/QueryRow 三个方法，**不放 Begin** —— 事务边界由调用方
// （importOne）决定，被调函数不该自己开事务，否则又回到"各自提交"的老问题。
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Pool 包一层 pgxpool，提供 admin.DB 所需的 Acquire 语义。
//
// 之所以不让 admin 直接依赖 *pgxpool.Pool：admin 需要的是"拿一个连接、用完还回"，
// 而 pgxpool.Conn 的释放语义（Release）会泄进业务代码。这里收敛成一个 closure。
type Pool struct{ p *pgxpool.Pool }

// NewPool 建连接池并验证可达。
func NewPool(ctx context.Context, dsn string) (*Pool, error) {
	return newPool(ctx, dsn, false)
}

// NewReadOnlyPool creates a pool whose every session rejects writes.
func NewReadOnlyPool(ctx context.Context, dsn string) (*Pool, error) {
	return newPool(ctx, dsn, true)
}

func newPool(ctx context.Context, dsn string, readOnly bool) (*Pool, error) {
	cfg, err := poolConfig(dsn, readOnly)
	if err != nil {
		return nil, fmt.Errorf("解析 DSN: %w", err)
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("建连接池: %w", err)
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Pool{p: p}, nil
}

// MinPoolConns 是 DSN 未显式给出 `pool_max_conns` 时的连接数下限。
//
// pgx 的默认值是 max(4, NumCPU)，而周期采集的每个 worker **峰值占两条连接**：
// 一条是整轮持有的渠道 advisory lock（TryLock 必须一直握着连接），另一条是
// 临时的凭证读取 / host 限速预留 / 采集结果写入。4 个 worker 就要 8 条。
// 在 ≤4 核的机器上默认值正好是 4：4 条全被锁占住，之后每次临时 Acquire 都要
// 阻塞到整渠道 120s 超时，整轮采集全军覆没。8 是刚好打满，没有余量给
// sla-core 的 admin 请求，故留 4 条余量。
// 上限不动：显式写了 pool_max_conns 的 DSN 说了算。
const MinPoolConns = 12

// maxConns 抬高 pgx 的默认连接数下限。单独一个函数是为了能脱离
// runtime.NumCPU() 断言 —— 直接测 poolConfig 在多核机器上会因为默认值
// 本来就够大而恒绿，测不出下限有没有生效。
func maxConns(dsn string, parsed int32) int32 {
	// ParseConfig 不区分"没写"和"写了个恰好等于默认值的数"，只能看原始 DSN。
	if strings.Contains(dsn, "pool_max_conns") || parsed >= MinPoolConns {
		return parsed
	}
	return MinPoolConns
}

func poolConfig(dsn string, readOnly bool) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = maxConns(dsn, cfg.MaxConns)
	if readOnly {
		if cfg.ConnConfig.RuntimeParams == nil {
			cfg.ConnConfig.RuntimeParams = map[string]string{}
		}
		cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	}
	return cfg, nil
}

// Acquire 取一个连接，返回的函数用于归还。
func (pl *Pool) Acquire(ctx context.Context) (*pgx.Conn, func(), error) {
	c, err := pl.p.Acquire(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("取连接: %w", err)
	}
	return c.Conn(), c.Release, nil
}

// Ping 供 /healthz 使用（health.Pinger 接口）。
func (pl *Pool) Ping(ctx context.Context) error { return pl.p.Ping(ctx) }

// Close 关闭连接池。
func (pl *Pool) Close() { pl.p.Close() }

// pgxConn 是"连接 + 归还"的组合，让 sink 各方法能用 defer 归还。
type pgxConn struct {
	Conn    *pgx.Conn
	release func()
}

// Close 归还连接。
func (c *pgxConn) Close() {
	if c.release != nil {
		c.release()
	}
}
