package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"time"

	"github.com/jackc/pgx/v5"
)

// HubSyncConfig 是 all-api-hub WebDAV 定时同步的那一行配置（hub_sync_config，id=1）。
//
// 地址与两个密码加密存储（D1，#34）。解密后的值**只出现在这个结构里与去 WebDAV
// 的那个请求里** —— 管理接口只回地址的展示值和密码的 has_*，不回显秘密。
type HubSyncConfig struct {
	WebDAVURL      string // 完整地址，可能带认证用的 query/userinfo
	WebDAVUsername string
	WebDAVPassword string
	BackupPassword string

	// WebDAVURLDisplay 是去掉 userinfo/query/fragment 的地址，管理接口只回它。
	// 保存时传回同一个展示值表示"地址不变"，见 SaveHubSyncConfig。
	WebDAVURLDisplay string

	Enabled         bool
	IntervalMinutes int
	ApplyMode       string

	// LastRunAt 不是本表的列，是 hub_sync_runs 最新一行的时刻（随查随算）。
	// 存一份在配置表上的话，它与历史表迟早分叉，而分叉的表现是定时器按一个
	// 时间走、界面显示另一个时间。
	LastRunAt *time.Time
}

// HubSyncApplyMode 的两个取值。与迁移里的 CHECK 一致。
const (
	HubSyncModeReport = "report"
	HubSyncModeImport = "import"
)

// 触发方式。排查时的第一个问题就是"这轮是谁触发的"。
const (
	HubSyncTriggerSchedule = "schedule"
	HubSyncTriggerManual   = "manual"
)

// hubSyncRunsKeep 保留的历史条数。
//
// 每轮的 result 含逐站明细（真实备份 110 站约 20KB），6 小时一轮的话
// 50 条覆盖约半个月 —— 够回答"最近怎么老失败"，又不会让这张表无限长。
const hubSyncRunsKeep = 50

// HubSyncRun 是一轮同步的记录。
type HubSyncRun struct {
	ID         int64
	StartedAt  time.Time
	FinishedAt time.Time
	Trigger    string
	Applied    bool
	Error      string
	Result     json.RawMessage
}

// LoadHubSyncConfig 读那一行，并带上最近一轮的时刻。
// 迁移里已经 INSERT 了 id=1，所以正常不会缺行。
func LoadHubSyncConfig(ctx context.Context, db DBTX) (HubSyncConfig, error) {
	var c HubSyncConfig
	var url, password, backup []byte
	err := db.QueryRow(ctx, `
SELECT webdav_url_ciphertext, webdav_url_display, webdav_username,
       webdav_password_ciphertext, backup_password_ciphertext,
       enabled, interval_minutes, apply_mode,
       (SELECT max(started_at) FROM hub_sync_runs)
  FROM hub_sync_config WHERE id=1`).Scan(
		&url, &c.WebDAVURLDisplay, &c.WebDAVUsername, &password, &backup,
		&c.Enabled, &c.IntervalMinutes, &c.ApplyMode, &c.LastRunAt)
	if err != nil {
		return HubSyncConfig{}, fmt.Errorf("读 all-api-hub 同步配置: %w", err)
	}
	for _, f := range []struct {
		name string
		in   []byte
		out  *string
	}{{"webdav_url", url, &c.WebDAVURL}, {"webdav_password", password, &c.WebDAVPassword},
		{"backup_password", backup, &c.BackupPassword}} {
		if *f.out, err = openCredential("hub_sync_config", hubSyncRowID, f.name, f.in); err != nil {
			return HubSyncConfig{}, err
		}
	}
	return c, nil
}

// hubSyncRowID 是 hub_sync_config 唯一一行的 id（CHECK id = 1），也是密文 AAD 里的记录 ID。
const hubSyncRowID = 1

// webdavDisplayURL 去掉可能承载认证信息的 userinfo、query 与 fragment。
func webdavDisplayURL(raw string) string {
	u, err := neturl.Parse(raw)
	if err != nil {
		return ""
	}
	if u.User == nil && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery {
		return raw // 没有可隐藏的部分就原样展示，避免规范化差异被误报为"已隐藏"
	}
	u.User, u.RawQuery, u.Fragment, u.ForceQuery = nil, "", "", false
	return u.String()
}

// SaveHubSyncConfig 写配置。
//
// **两个密码留空 = 保持原值**，不是清空：管理接口从不回显它们，所以界面上那两个
// 输入框每次打开都是空的 —— 若把空串当"清空"，任何一次只改间隔的保存都会顺手
// 把密码抹掉，而症状要等到下一次定时同步 401 才出现。
//
// **地址传回当前展示值 = 保持原地址**：界面拿到的是去掉认证信息的展示值，原样
// 提交时若按传入值覆盖，就会用它顶掉带认证参数的真实地址。其他值按新地址保存，
// 空串即清空。其余非秘密字段一律按传入值覆盖。
func SaveHubSyncConfig(ctx context.Context, db DBTX, c HubSyncConfig) error {
	var encrypted [3][]byte
	for i, f := range []struct{ name, value string }{
		{"webdav_url", c.WebDAVURL}, {"webdav_password", c.WebDAVPassword}, {"backup_password", c.BackupPassword},
	} {
		var err error
		if encrypted[i], err = sealCredential("hub_sync_config", hubSyncRowID, f.name, f.value); err != nil {
			return err
		}
	}
	_, err := db.Exec(ctx, `
UPDATE hub_sync_config
   SET webdav_url_ciphertext = CASE WHEN $1 = webdav_url_display THEN webdav_url_ciphertext ELSE $2 END,
       webdav_url_display    = CASE WHEN $1 = webdav_url_display THEN webdav_url_display ELSE $3 END,
       webdav_username  = $4,
       webdav_password_ciphertext = COALESCE($5, webdav_password_ciphertext),
       backup_password_ciphertext = COALESCE($6, backup_password_ciphertext),
       enabled          = $7,
       interval_minutes = $8,
       apply_mode       = $9,
       updated_at       = now()
 WHERE id = 1`,
		c.WebDAVURL, encrypted[0], webdavDisplayURL(c.WebDAVURL), c.WebDAVUsername,
		encrypted[1], encrypted[2], c.Enabled, c.IntervalMinutes, c.ApplyMode)
	if err != nil {
		return fmt.Errorf("写 all-api-hub 同步配置: %w", err)
	}
	return nil
}

// InsertHubSyncRun 记一轮同步，并把历史裁到最近 hubSyncRunsKeep 条。
//
// 裁剪跟着写入做，不另起一个清理任务：这张表每轮只多一行，顺手裁掉一行的成本
// 可以忽略；而一个"以后再加"的清理任务在这种低频表上多半永远不会被加。
func InsertHubSyncRun(ctx context.Context, db DBTX, r HubSyncRun) (int64, error) {
	// 没有结果时写 **SQL NULL**，不写 JSON 标量 `null`。
	//
	// 两者在 jsonb 里不是一回事，而差别会在读那一侧炸：列表查询要
	// `result - 'items'` 剥掉逐站明细，而 jsonb 的 `-` 运算符只接受对象/数组，
	// 碰上标量 null 直接报 `cannot delete from scalar`（SQLSTATE 22023）——
	// 于是**一轮失败的同步会让整个历史列表 500**，而失败恰恰是最需要看历史的时候。
	// 2026-09-13 实测撞到：密码错那轮写进去之后，列表接口就一直报这个。
	var result any
	if len(r.Result) > 0 && string(r.Result) != "null" {
		result = r.Result
	}
	var id int64
	err := db.QueryRow(ctx, `
INSERT INTO hub_sync_runs (started_at, finished_at, trigger, applied, error, result)
VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		r.StartedAt, r.FinishedAt, r.Trigger, r.Applied, r.Error, result).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("记录 all-api-hub 同步结果: %w", err)
	}
	if _, err := db.Exec(ctx, `
DELETE FROM hub_sync_runs
 WHERE id NOT IN (SELECT id FROM hub_sync_runs ORDER BY id DESC LIMIT $1)`,
		hubSyncRunsKeep); err != nil {
		return id, fmt.Errorf("裁剪 all-api-hub 同步历史: %w", err)
	}
	return id, nil
}

// ListHubSyncRuns 按时间倒序列出历史。
//
// **不带 result**：列表页只需要计数与状态，而每行的 result 含逐站明细，
// 几十行一起回等于把上兆 JSON 塞进一个列表响应。明细走 GetHubSyncRun。
func ListHubSyncRuns(ctx context.Context, db DBTX, limit int) ([]HubSyncRun, error) {
	if limit <= 0 || limit > hubSyncRunsKeep {
		limit = hubSyncRunsKeep
	}
	rows, err := db.Query(ctx, `
SELECT id, started_at, finished_at, trigger, applied, error,
       -- 逐站明细留给详情接口；列表只带汇总那几个数。
       -- 先判类型再减：jsonb 的减号只接受对象/数组，碰上标量会报
       -- cannot delete from scalar。写入侧已经保证"要么 SQL NULL 要么对象"，
       -- 这里再判一次是因为**这条查询炸的后果是整个历史列表打不开**，
       -- 而历史列表正是出事时唯一能看的地方。
       CASE WHEN jsonb_typeof(result) = 'object' THEN result - 'items'
            ELSE COALESCE(result, 'null'::jsonb) END
  FROM hub_sync_runs ORDER BY started_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("列 all-api-hub 同步历史: %w", err)
	}
	defer rows.Close()
	out := []HubSyncRun{}
	for rows.Next() {
		var r HubSyncRun
		if err := rows.Scan(&r.ID, &r.StartedAt, &r.FinishedAt, &r.Trigger,
			&r.Applied, &r.Error, &r.Result); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	// 遍历中途出错时 rows.Next() 只是返回 false，与"正常读完"无从区分 ——
	// 不查这一下就会回一个**被截断**的列表，而它看起来完全正常。
	return out, rows.Err()
}

// GetHubSyncRun 读一轮的完整记录（含逐站明细）。
func GetHubSyncRun(ctx context.Context, db DBTX, id int64) (HubSyncRun, error) {
	var r HubSyncRun
	err := db.QueryRow(ctx, `
SELECT id, started_at, finished_at, trigger, applied, error,
       COALESCE(result,'null'::jsonb)
  FROM hub_sync_runs WHERE id=$1`, id).Scan(
		&r.ID, &r.StartedAt, &r.FinishedAt, &r.Trigger, &r.Applied, &r.Error, &r.Result)
	if errors.Is(err, pgx.ErrNoRows) {
		return HubSyncRun{}, ErrNotFound
	}
	if err != nil {
		return HubSyncRun{}, fmt.Errorf("读 all-api-hub 同步记录 %d: %w", id, err)
	}
	return r, nil
}
