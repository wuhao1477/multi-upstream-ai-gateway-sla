// Package store 是 PG 访问层：迁移、连接池、查询。
//
// 纪律（01 §5）：决策路径只读内存快照、不查库。本包承担写路径与后台快照重建，
// 不在同步决策的关键路径上。
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	slagateway "github.com/wuhao1477/multi-upstream-ai-gateway-sla"
)

// migrationFS 是仓库根 migrations/ 的嵌入快照。
// 嵌入声明在根包（embed.go）——go:embed 不能引用包目录之外的路径，
// 而 10 §1 把 migrations/ 定在仓库根。
var migrationFS = slagateway.MigrationFS

// advisoryLockKey 是 bootstrap 选主用的固定常量（06 §2.2）。
// 两个 core 实例并存时，取到锁的执行迁移、其余跳过并轮询就绪。
// 用咨询锁而非新建表：无额外依赖，连接断开自动释放、不留死锁。
const advisoryLockKey int64 = 0x5F1A_C0DE

// Migration 是一个迁移文件。
type Migration struct {
	Name     string
	SQL      string
	Checksum string // sha256 前 16 位，用于检测已应用文件被篡改
}

// LoadMigrations 读取嵌入的迁移，按文件名排序。
//
// 排序即执行顺序：文件名前缀 001~012 编码了依赖链
// （被引用的表先建，02 §9.1bis 建表顺序原则）。
func LoadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("读取嵌入迁移: %w", err)
	}
	var ms []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("读取 %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(b)
		ms = append(ms, Migration{
			Name:     e.Name(),
			SQL:      string(b),
			Checksum: hex.EncodeToString(sum[:])[:16],
		})
	}
	if len(ms) == 0 {
		return nil, fmt.Errorf("migrations/ 为空——嵌入是否失败？")
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
	return ms, nil
}

const createLedgerTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  name        TEXT PRIMARY KEY,
  checksum    TEXT NOT NULL,
  applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// Migrate 应用尚未应用的迁移，幂等。
//
// 三条保证：
//  1. **选主**：先取 advisory lock，双实例并发冷启动只有一个真正执行（06 §2.2）
//  2. **幂等**：已应用的按 schema_migrations 跳过，重复调用不报错
//  3. **防篡改**：已应用文件的 checksum 变了即报错 —— 已应用的迁移不可修改，
//     改了会让已部署环境与新环境 schema 不一致（split_migrations.py 的告示）
func Migrate(ctx context.Context, conn *pgx.Conn, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	// 选主：取不到锁说明另一实例正在迁移，直接返回让调用方轮询就绪。
	var got bool
	if err := conn.QueryRow(ctx,
		"SELECT pg_try_advisory_lock($1)", advisoryLockKey).Scan(&got); err != nil {
		return fmt.Errorf("取咨询锁: %w", err)
	}
	if !got {
		logger.Info("另一实例正在执行迁移，跳过（选主，06 §2.2）")
		return nil
	}
	defer func() {
		if _, err := conn.Exec(ctx,
			"SELECT pg_advisory_unlock($1)", advisoryLockKey); err != nil {
			logger.Warn("释放咨询锁失败（连接断开时会自动释放）", "err", err)
		}
	}()

	if _, err := conn.Exec(ctx, createLedgerTable); err != nil {
		return fmt.Errorf("建 schema_migrations: %w", err)
	}

	applied := map[string]string{}
	rows, err := conn.Query(ctx, "SELECT name, checksum FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("读已应用迁移: %w", err)
	}
	for rows.Next() {
		var n, c string
		if err := rows.Scan(&n, &c); err != nil {
			rows.Close()
			return fmt.Errorf("扫描已应用迁移: %w", err)
		}
		applied[n] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("遍历已应用迁移: %w", err)
	}

	ms, err := LoadMigrations()
	if err != nil {
		return err
	}

	var ran int
	for _, m := range ms {
		if old, ok := applied[m.Name]; ok {
			if old != m.Checksum {
				return fmt.Errorf("迁移 %s 已应用但内容已变（库中 %s，当前 %s）——"+
					"已应用的迁移不可修改，schema 变更请新增文件",
					m.Name, old, m.Checksum)
			}
			continue
		}
		// 每个文件一个事务：失败则该文件整体回滚，不留半截 schema。
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("开启事务 %s: %w", m.Name, err)
		}
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("应用 %s: %w", m.Name, err)
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO schema_migrations(name, checksum) VALUES ($1,$2)",
			m.Name, m.Checksum); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("记录 %s: %w", m.Name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("提交 %s: %w", m.Name, err)
		}
		logger.Info("已应用迁移", "name", m.Name, "checksum", m.Checksum)
		ran++
	}

	logger.Info("迁移完成", "applied", ran, "total", len(ms), "skipped", len(ms)-ran)
	return nil
}
