package store

import (
	"context"
	"fmt"

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

func poolConfig(dsn string, readOnly bool) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
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
