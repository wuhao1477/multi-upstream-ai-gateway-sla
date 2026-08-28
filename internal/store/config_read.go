package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
)

// LoadConfigValues 读取当前生效的全局配置取值。
//
// 「当前生效」= 每个 param_key 的**最大 version**那一行（config_params 只
// INSERT 新版本、永不 UPDATE 旧行，09 §2 与价格版本同源的不可覆盖原则）。
//
// ⚠️ 关键项须 `confirmed_twice=true` 才生效（09 §3 末条）：未走完二次确认的
// 关键改动不进快照。这不是可选的严格性 —— 少了它，一个未确认的
// probe 预算改动会直接影响真实花钱。
func LoadConfigValues(ctx context.Context, conn *pgx.Conn) (map[string]string, error) {
	rows, err := conn.Query(ctx, `
SELECT DISTINCT ON (param_key) param_key, param_value, is_critical, confirmed_twice
  FROM config_params
 WHERE scope_type = 'global' AND scope_id = '*'
 ORDER BY param_key, version DESC`)
	if err != nil {
		return nil, fmt.Errorf("查询 config_params: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var (
			key       string
			raw       []byte
			critical  bool
			confirmed bool
		)
		if err := rows.Scan(&key, &raw, &critical, &confirmed); err != nil {
			return nil, fmt.Errorf("扫描配置行: %w", err)
		}
		if critical && !confirmed {
			// 跳过 → 快照回落到文档默认值（config.NewSnapshot 的行为）
			continue
		}
		s, err := jsonToScalar(raw)
		if err != nil {
			return nil, fmt.Errorf("配置项 %s: %w", key, err)
		}
		out[key] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历配置行: %w", err)
	}
	return out, nil
}

// jsonToScalar 把 JSONB 值还原成字符串标量。
//
// config_params.param_value 是 JSONB，而 config.Snapshot 以字符串存取
// （类型由文档默认值的形态推断）。数值不能走 fmt.Sprint(float64) ——
// 那会把 262144 打成 "262144" 没问题，但 0.05 可能变成 "0.05000000000000001"。
// 故用 json.Number 保留原始字面量。
func jsonToScalar(raw []byte) (string, error) {
	dec := json.NewDecoder(bytesReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("解析 JSONB %q: %w", string(raw), err)
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case json.Number:
		return t.String(), nil
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	default:
		// 对象/数组类配置一期不存在；出现即为数据错误，报出来而非静默 Sprint
		return "", fmt.Errorf("不支持的配置值类型 %T（值 %q）", v, string(raw))
	}
}

// bytesReader 避免为一次解码引入额外依赖。
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
