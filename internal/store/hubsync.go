package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// HubSyncConfig 是 all-api-hub WebDAV 定时同步的那一行配置（hub_sync_config，id=1）。
//
// 两个密码是明文（与 collector_credentials 同一取舍，FR-113 一期）。它们**只出现在
// 这个结构里与去 WebDAV 的那个请求里** —— 管理接口只报 has_*，不回显内容。
type HubSyncConfig struct {
	WebDAVURL      string
	WebDAVUsername string
	WebDAVPassword string
	BackupPassword string

	Enabled         bool
	IntervalMinutes int
	ApplyMode       string

	LastRunAt  *time.Time
	LastError  string
	LastResult json.RawMessage
}

// HubSyncApplyMode 的两个取值。与迁移里的 CHECK 一致。
const (
	HubSyncModeReport = "report"
	HubSyncModeImport = "import"
)

// LoadHubSyncConfig 读那一行。迁移里已经 INSERT 了 id=1，所以正常不会缺行。
func LoadHubSyncConfig(ctx context.Context, db DBTX) (HubSyncConfig, error) {
	var c HubSyncConfig
	err := db.QueryRow(ctx, `
SELECT webdav_url, webdav_username, webdav_password, backup_password,
       enabled, interval_minutes, apply_mode,
       last_run_at, last_error, COALESCE(last_result,'null'::jsonb)
  FROM hub_sync_config WHERE id=1`).Scan(
		&c.WebDAVURL, &c.WebDAVUsername, &c.WebDAVPassword, &c.BackupPassword,
		&c.Enabled, &c.IntervalMinutes, &c.ApplyMode,
		&c.LastRunAt, &c.LastError, &c.LastResult)
	if err != nil {
		return HubSyncConfig{}, fmt.Errorf("读 all-api-hub 同步配置: %w", err)
	}
	return c, nil
}

// SaveHubSyncConfig 写配置。
//
// **两个密码留空 = 保持原值**，不是清空：管理接口从不回显它们，所以界面上那两个
// 输入框每次打开都是空的 —— 若把空串当"清空"，任何一次只改间隔的保存都会顺手
// 把密码抹掉，而症状要等到下一次定时同步 401 才出现。
// 非密码字段一律按传入值覆盖（它们本来就会回显，空就是空）。
func SaveHubSyncConfig(ctx context.Context, db DBTX, c HubSyncConfig) error {
	_, err := db.Exec(ctx, `
UPDATE hub_sync_config
   SET webdav_url       = $1,
       webdav_username  = $2,
       webdav_password  = COALESCE(NULLIF($3,''), webdav_password),
       backup_password  = COALESCE(NULLIF($4,''), backup_password),
       enabled          = $5,
       interval_minutes = $6,
       apply_mode       = $7,
       updated_at       = now()
 WHERE id = 1`,
		c.WebDAVURL, c.WebDAVUsername, c.WebDAVPassword, c.BackupPassword,
		c.Enabled, c.IntervalMinutes, c.ApplyMode)
	if err != nil {
		return fmt.Errorf("写 all-api-hub 同步配置: %w", err)
	}
	return nil
}

// RecordHubSyncRun 记一次同步的结果。errMsg 为空表示这轮成功。
//
// 成功时必须把 last_error 清掉 —— 否则界面会一直挂着一条早就修好的报错。
func RecordHubSyncRun(
	ctx context.Context, db DBTX, at time.Time, errMsg string, result json.RawMessage,
) error {
	if len(result) == 0 {
		result = json.RawMessage("null")
	}
	_, err := db.Exec(ctx, `
UPDATE hub_sync_config
   SET last_run_at = $1, last_error = $2, last_result = $3, updated_at = now()
 WHERE id = 1`, at, errMsg, result)
	if err != nil {
		return fmt.Errorf("记录 all-api-hub 同步结果: %w", err)
	}
	return nil
}
