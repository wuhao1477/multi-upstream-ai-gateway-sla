package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool 包一层 pgxpool，提供 admin.DB 所需的 Acquire 语义。
//
// 之所以不让 admin 直接依赖 *pgxpool.Pool：admin 需要的是"拿一个连接、用完还回"，
// 而 pgxpool.Conn 的释放语义（Release）会泄进业务代码。这里收敛成一个 closure。
type Pool struct{ p *pgxpool.Pool }

// NewPool 建连接池并验证可达。
func NewPool(ctx context.Context, dsn string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
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
