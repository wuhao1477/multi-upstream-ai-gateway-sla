package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/config"
)

// SeedConfigParams 把 09 §4bis 的 72 键灌入 config_params，幂等。
//
// **为什么种子在 Go 里而不在 SQL 迁移里**：键清单是从文档生成的
// （internal/config/params_gen.go）。若再手写一份 INSERT，就有了第二份清单，
// 而两份清单必然漂移 —— 漂移后果是运行时静默取零值
// （catalog_missing_rounds=0 会让每轮采集都判模型下架，02 §1.3bis）。
// 故种子直接遍历生成物，清单只有一处。
//
// 幂等做法：只插入缺失的键，**不覆盖已有值**。运维改过的值不能被重启抹回默认
// —— 那会让"改配置"变成"重启前有效"。
func SeedConfigParams(ctx context.Context, conn *pgx.Conn, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启种子事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // 已 Commit 后 Rollback 是 no-op

	var inserted, skipped int
	for _, p := range config.Params {
		// EnvSourced 项**不落表**（09 §4bis 对 admin_token 的原文：
		// "不落 config_params（避免自己改自己）"）。若写进去，管理 API 就能
		// 修改自己的鉴权令牌 —— 那是提权，不只是设计洁癖问题。
		if p.EnvSourced {
			skipped++
			continue
		}
		// param_value 是 JSONB：数值与布尔按字面量存，字符串加引号。
		jsonVal := toJSONLiteral(p.Default)

		// scope_type='global' 时 scope_id 恒为哨兵 '*'（02 §2：用 NULL 会让
		// 唯一约束失效，因为 PG 普通 UNIQUE 允许多行 NULL）。
		tag, err := tx.Exec(ctx, `
INSERT INTO config_params (scope_type, scope_id, param_key, param_value,
                           is_critical, version, confirmed_twice, change_reason)
SELECT 'global', '*', $1, $2::jsonb, $3, 1, $3, '出厂默认值（09 §4bis 种子）'
WHERE NOT EXISTS (
  SELECT 1 FROM config_params
   WHERE scope_type='global' AND scope_id='*' AND param_key=$1
)`, p.Key, jsonVal, p.Critical)
		if err != nil {
			return fmt.Errorf("插入配置项 %s: %w", p.Key, err)
		}
		inserted += int(tag.RowsAffected())
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交种子: %w", err)
	}

	seedable := len(config.Params) - skipped
	logger.Info("配置种子完成",
		"inserted", inserted, "seedable", seedable,
		"existing", seedable-inserted,
		"env_sourced_skipped", skipped)
	return nil
}

// toJSONLiteral 把文档里的默认值字面量转成合法 JSON。
//
// 注意 `confirmed_twice` 对关键项直接置 true：种子是出厂默认值，
// 不是"人为修改"，不需要走二次确认。若置 false，决策路径会因
// "关键项未确认不进快照"（09 §3）而拿不到任何关键配置。
func toJSONLiteral(def string) string {
	switch def {
	case "true", "false":
		return def
	}
	if isNumeric(def) {
		return def
	}
	// 已是合法 JSON 数组/对象的字面量原样用（balance_text_patterns 是关键词表）。
	// 首版会把它当普通字符串再套一层引号，读回来就成了带引号的字符串而非数组。
	if looksLikeJSONComposite(def) && json.Valid([]byte(def)) {
		return def
	}
	// 字符串需要 JSON 引号；内部引号转义
	out := make([]rune, 0, len(def)+2)
	out = append(out, '"')
	for _, r := range def {
		if r == '"' || r == '\\' {
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	out = append(out, '"')
	return string(out)
}

// looksLikeJSONComposite 判断字面量是否为 JSON 数组或对象。
func looksLikeJSONComposite(s string) bool {
	if len(s) < 2 {
		return false
	}
	return (s[0] == '[' && s[len(s)-1] == ']') || (s[0] == '{' && s[len(s)-1] == '}')
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	dot := false
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '-' && i == 0:
		case r == '.' && !dot:
			dot = true
		default:
			return false
		}
	}
	return s != "-" && s != "."
}
