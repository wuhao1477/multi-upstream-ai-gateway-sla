package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// beforeMigration 是"在某个迁移文件的事务里、先于它的 SQL 执行"的 Go 步骤。
// 只用于纯 SQL 做不了的数据迁移（这里需要部署密钥）。
var beforeMigration = map[string]func(context.Context, pgx.Tx) error{
	"035_drop_plaintext_credentials.sql": encryptPlaintextCredentials,
}

// plaintextColumn 是一个待加密的明文列；密文写入同名加 _ciphertext 的列（034 新增）。
// 表名、列名都是下面的常量，不来自输入，拼进 SQL 是安全的。
type plaintextColumn struct{ table, idColumn, field string }

var plaintextColumns = []plaintextColumn{
	{"upstream_keys", "id", "secret"},
	{"collector_credentials", "account_id", "access_token"},
	{"collector_credentials", "account_id", "refresh_token"},
	{"hub_sync_config", "id", "webdav_url"},
	{"hub_sync_config", "id", "webdav_password"},
	{"hub_sync_config", "id", "backup_password"},
}

// encryptPlaintextCredentials 把现有明文加密写入密文列，再逐行解密比对。
// 任何失败都让 035 整体回滚：明文列保留，035 不记为已应用。
func encryptPlaintextCredentials(ctx context.Context, tx pgx.Tx) error {
	for _, c := range plaintextColumns {
		if err := c.encrypt(ctx, tx); err != nil {
			if errors.Is(err, ErrCredentialKeyMissing) {
				return fmt.Errorf("库中存在明文凭证，需先配置 SLA_CREDENTIAL_SECRET_KEY 再升级: %w", err)
			}
			return err
		}
	}
	var empty int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM upstream_keys WHERE secret_ciphertext IS NULL`).Scan(&empty); err != nil {
		return err
	}
	if empty > 0 {
		return fmt.Errorf("upstream_keys 有 %d 行 secret 为空，无法加密；请先修正或删除这些行", empty)
	}
	// 前缀与运行时 secretPrefix 一致：都按字符取前 8 个。
	if _, err := tx.Exec(ctx, `UPDATE upstream_keys SET secret_prefix = left(secret, 8)`); err != nil {
		return err
	}
	var url string
	if err := tx.QueryRow(ctx, `SELECT webdav_url FROM hub_sync_config WHERE id = 1`).Scan(&url); err != nil &&
		!errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE hub_sync_config SET webdav_url_display = $1 WHERE id = 1`,
		webdavDisplayURL(url)); err != nil {
		return err
	}
	for _, c := range plaintextColumns {
		if err := c.verify(ctx, tx); err != nil {
			return err
		}
	}
	return nil
}

type plaintextRow struct {
	id    int64
	plain *string
}

func (c plaintextColumn) encrypt(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT %s, %s FROM %s`, c.idColumn, c.field, c.table))
	if err != nil {
		return fmt.Errorf("读 %s.%s: %w", c.table, c.field, err)
	}
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (plaintextRow, error) {
		var row plaintextRow
		return row, r.Scan(&row.id, &row.plain)
	})
	if err != nil {
		return fmt.Errorf("读 %s.%s: %w", c.table, c.field, err)
	}
	update := fmt.Sprintf(`UPDATE %s SET %s_ciphertext = $2 WHERE %s = $1`, c.table, c.field, c.idColumn)
	for _, row := range found {
		encrypted, err := sealCredential(c.table, row.id, c.field, deref(row.plain))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, update, row.id, encrypted); err != nil {
			return fmt.Errorf("写 %s.%s_ciphertext（记录 %d）: %w", c.table, c.field, row.id, err)
		}
	}
	return nil
}

// verify 在同一事务里回读密文、解密并与明文逐行比对（全量，不抽样）。
func (c plaintextColumn) verify(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT %s, %s, %s_ciphertext FROM %s`,
		c.idColumn, c.field, c.field, c.table))
	if err != nil {
		return fmt.Errorf("校验 %s.%s: %w", c.table, c.field, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var plain *string
		var encrypted []byte
		if err := rows.Scan(&id, &plain, &encrypted); err != nil {
			return fmt.Errorf("校验 %s.%s: %w", c.table, c.field, err)
		}
		got, err := openCredential(c.table, id, c.field, encrypted)
		if err != nil || got != deref(plain) {
			return fmt.Errorf("迁移校验失败：%s.%s 记录 %d 加密后回读不一致", c.table, c.field, id)
		}
	}
	return rows.Err()
}
