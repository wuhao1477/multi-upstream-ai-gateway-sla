package store

import (
	"context"
	"fmt"
	"time"
)

// HostRequestLimiter coordinates collection request starts across processes.
type HostRequestLimiter struct {
	Pool *Pool
}

// NewHostRequestLimiter creates a PostgreSQL-backed host request limiter.
func NewHostRequestLimiter(pool *Pool) *HostRequestLimiter {
	return &HostRequestLimiter{Pool: pool}
}

// Wait blocks until this process owns the next request slot for host.
func (l *HostRequestLimiter) Wait(
	ctx context.Context, host string, minInterval time.Duration,
) error {
	if minInterval <= 0 {
		return nil
	}
	conn, release, err := l.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("获取 host 限速连接: %w", err)
	}
	var allowedAt, databaseNow time.Time
	err = conn.QueryRow(ctx, `
INSERT INTO collector_host_rate_limits (host, next_allowed_at, updated_at)
VALUES ($1, clock_timestamp() + $2::bigint * interval '1 millisecond', clock_timestamp())
ON CONFLICT (host) DO UPDATE
   SET next_allowed_at = GREATEST(collector_host_rate_limits.next_allowed_at, clock_timestamp())
                         + $2::bigint * interval '1 millisecond',
       updated_at = clock_timestamp()
RETURNING next_allowed_at - $2::bigint * interval '1 millisecond', clock_timestamp()`,
		host, minInterval.Milliseconds()).Scan(&allowedAt, &databaseNow)
	release()
	if err != nil {
		return fmt.Errorf("预留 host %q 请求时隙: %w", host, err)
	}
	delay := allowedAt.Sub(databaseNow)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
