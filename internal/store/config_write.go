package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ApplyResult 是一次 apply 的结果。
type ApplyResult struct {
	NewVersion int
	PrevValue  string
	Idempotent bool // true = 该 idempotency_key 已生效过，本次未产生新版本
}

// 三类可区分的失败，供 admin 层映射成 HTTP 状态码。
var (
	// ErrVersionConflict：expected_current_version 与库中不符（09 §3 → 409）。
	// 含义是"preview 已过期，须重新预览"——不是重试能解决的。
	ErrVersionConflict = errors.New("config: 版本冲突（preview 已过期）")
	// ErrUnknownParam：键不在 09 §4bis 清单（→ 400）。
	ErrUnknownParam = errors.New("config: 未知配置键")
	// ErrNeedsConfirm：关键项未走二次确认（→ 400）。
	ErrNeedsConfirm = errors.New("config: 关键项必须走二次确认")
)

// ApplyConfig 写入一个新配置版本。
//
// **全部动作在单事务内**（09 §3 明确要求）：锁定当前最大版本 → 比对
// expected_current_version → 分配 version=max+1 → INSERT。
// 分开做则窗口期内仍可并发插入两个相同 version。
//
// 幂等：同一 idempotency_key 重复投递只生效一次（依赖
// config_params 的 UNIQUE(param_key, idempotency_key)）。网络重试是常态，
// 不幂等会造出重复版本。
func ApplyConfig(
	ctx context.Context,
	conn *pgx.Conn,
	paramKey, newValueJSON, changedBy, changeReason string,
	expectedVersion int,
	isCritical, confirmedTwice bool,
	idempotencyKey string,
) (*ApplyResult, error) {
	if isCritical && !confirmedTwice {
		return nil, ErrNeedsConfirm
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("开启事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 幂等前置检查：该 key 已生效过则直接返回，不再走版本比对
	// （重试时库中版本已是 max+1，比对必然失败而误报 409）。
	if idempotencyKey != "" {
		var existingVer int
		err := tx.QueryRow(ctx, `
SELECT version FROM config_params
 WHERE param_key = $1 AND idempotency_key = $2`,
			paramKey, idempotencyKey).Scan(&existingVer)
		switch {
		case err == nil:
			return &ApplyResult{NewVersion: existingVer, Idempotent: true}, nil
		case errors.Is(err, pgx.ErrNoRows):
			// 首次投递，继续
		default:
			return nil, fmt.Errorf("幂等检查: %w", err)
		}
	}

	// 锁定该参数的全部现有版本行，防并发分配同一 version。
	// 用 FOR UPDATE 而非乐观重试：配置写入是低频操作，直接串行化最简单。
	var curVersion int
	var prevValue string
	err = tx.QueryRow(ctx, `
SELECT version, param_value#>>'{}'
  FROM config_params
 WHERE scope_type='global' AND scope_id='*' AND param_key=$1
 ORDER BY version DESC
 LIMIT 1
   FOR UPDATE`, paramKey).Scan(&curVersion, &prevValue)
	if errors.Is(err, pgx.ErrNoRows) {
		// 键存在于清单但库中无行 → 种子未跑或键是新加的。
		// 不在这里补种子：那会掩盖"种子没跑"这个真实问题。
		return nil, fmt.Errorf("%w: %s 在库中无记录（种子未执行？）",
			ErrUnknownParam, paramKey)
	}
	if err != nil {
		return nil, fmt.Errorf("锁定当前版本: %w", err)
	}

	if expectedVersion != 0 && expectedVersion != curVersion {
		return nil, fmt.Errorf("%w: 期望 %d，当前 %d",
			ErrVersionConflict, expectedVersion, curVersion)
	}

	newVersion := curVersion + 1
	_, err = tx.Exec(ctx, `
INSERT INTO config_params (scope_type, scope_id, param_key, param_value,
                           is_critical, version, prev_value, changed_by,
                           change_reason, confirmed_twice, idempotency_key)
VALUES ('global','*',$1,$2::jsonb,$3,$4,$5::jsonb,$6,$7,$8,$9)`,
		paramKey, newValueJSON, isCritical, newVersion,
		toJSONLiteral(prevValue), changedBy, changeReason,
		confirmedTwice, nullIfEmpty(idempotencyKey))
	if err != nil {
		return nil, fmt.Errorf("插入新版本: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("提交: %w", err)
	}
	return &ApplyResult{NewVersion: newVersion, PrevValue: prevValue}, nil
}

// ConfigVersion 是一条历史版本（FR-099：前后值/责任人/原因/时间）。
type ConfigVersion struct {
	Version        int
	Value          string
	PrevValue      string
	ChangedBy      string
	ChangeReason   string
	ConfirmedTwice bool
	EffectiveAt    string
}

// ConfigHistory 返回某键的全部版本，新版本在前。
func ConfigHistory(ctx context.Context, conn *pgx.Conn, paramKey string) ([]ConfigVersion, error) {
	rows, err := conn.Query(ctx, `
SELECT version,
       param_value#>>'{}',
       COALESCE(prev_value#>>'{}',''),
       COALESCE(changed_by,''),
       COALESCE(change_reason,''),
       confirmed_twice,
       to_char(effective_at,'YYYY-MM-DD"T"HH24:MI:SSOF')
  FROM config_params
 WHERE scope_type='global' AND scope_id='*' AND param_key=$1
 ORDER BY version DESC`, paramKey)
	if err != nil {
		return nil, fmt.Errorf("查询历史: %w", err)
	}
	defer rows.Close()

	var out []ConfigVersion
	for rows.Next() {
		var v ConfigVersion
		if err := rows.Scan(&v.Version, &v.Value, &v.PrevValue, &v.ChangedBy,
			&v.ChangeReason, &v.ConfirmedTwice, &v.EffectiveAt); err != nil {
			return nil, fmt.Errorf("扫描历史行: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CurrentConfigVersion 返回某键当前的最大 version，供 preview 回带。
func CurrentConfigVersion(ctx context.Context, conn *pgx.Conn, paramKey string) (int, string, error) {
	var ver int
	var val string
	err := conn.QueryRow(ctx, `
SELECT version, param_value#>>'{}'
  FROM config_params
 WHERE scope_type='global' AND scope_id='*' AND param_key=$1
 ORDER BY version DESC LIMIT 1`, paramKey).Scan(&ver, &val)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", fmt.Errorf("%w: %s", ErrUnknownParam, paramKey)
	}
	if err != nil {
		return 0, "", fmt.Errorf("查询当前版本: %w", err)
	}
	return ver, val, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
