// Package config 承载 config_params 的静态元数据与取值。
//
// 分工（09 §2/§4）：
//   - 静态元数据（键、默认值、是否关键、分组）由 params_gen.go 从文档生成，不手写。
//   - 运行时取值由 store 层读 config_params 表，本包只做查找与类型转换。
//
// 纪律：决策路径只读内存快照、不查库（01 §5）。本包的 Snapshot 就是那份快照。
package config

import (
	"fmt"
	"strconv"
	"time"
)

// specByKey 在包初始化时建索引，避免每次查找都线性扫 72 项。
var specByKey = func() map[string]ParamSpec {
	m := make(map[string]ParamSpec, len(Params))
	for _, p := range Params {
		m[p.Key] = p
	}
	return m
}()

// Spec 返回某键的静态元数据。ok=false 表示该键不在 §4bis 清单里。
//
// 白名单语义（09 §3）：/admin/config 必须拒绝清单外的键（返回 400）。
// 这不是防御性编程——清单是种子与白名单的共同来源，键不在其中就意味着
// 它没有默认值、没有文档说明，写进去只会在运行时静默取零值。
func Spec(key string) (ParamSpec, bool) {
	s, ok := specByKey[key]
	return s, ok
}

// Keys 返回全部键名，顺序与文档一致（供种子生成与设置页渲染）。
func Keys() []string {
	ks := make([]string, 0, len(Params))
	for _, p := range Params {
		ks = append(ks, p.Key)
	}
	return ks
}

// Snapshot 是一份不可变的配置取值快照。
//
// 缺键时回落到文档默认值，而不是回落到零值 —— 零值会造成静默错误行为
// （catalog_missing_rounds=0 会让每轮采集都判模型下架，02 §1.3bis）。
type Snapshot struct {
	values map[string]string
	loaded time.Time
}

// NewSnapshot 用库中取值构造快照；未提供的键回落到文档默认值。
//
// **跳过 EnvSourced 项**（目前只有 admin_token）：它们不落 config_params，
// 其"默认值"在文档里是一句说明（"由 env ADMIN_TOKEN 注入"）而不是真值，
// 放进快照会让调用方读到那句中文当令牌用。
func NewSnapshot(dbValues map[string]string) *Snapshot {
	v := make(map[string]string, len(Params))
	for _, p := range Params {
		if p.EnvSourced {
			continue
		}
		if got, ok := dbValues[p.Key]; ok {
			v[p.Key] = got
		} else {
			v[p.Key] = p.Default
		}
	}
	return &Snapshot{values: v, loaded: time.Now()}
}

// LoadedAt 返回快照构建时刻，供陈旧度监控使用。
func (s *Snapshot) LoadedAt() time.Time { return s.loaded }

// raw 取原始字符串。键不存在即为编程错误（键名应来自常量或生成物），
// 故返回 error 而非静默给零值。
func (s *Snapshot) raw(key string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("config: 快照未初始化（键 %q）", key)
	}
	v, ok := s.values[key]
	if !ok {
		return "", fmt.Errorf("config: 未知键 %q（不在 09 §4bis 清单）", key)
	}
	return v, nil
}

// Int 取整型配置。解析失败或键不存在时返回 error ——
// 调用方应在启动时一次性校验全部用到的键，而不是在热路径上忽略错误。
func (s *Snapshot) Int(key string) (int, error) {
	v, err := s.raw(key)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: 键 %q 的值 %q 不是整数: %w", key, v, err)
	}
	return n, nil
}

// Float 取浮点配置（比例类参数，如 billing_variance_tolerance=0.05）。
func (s *Snapshot) Float(key string) (float64, error) {
	v, err := s.raw(key)
	if err != nil {
		return 0, err
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("config: 键 %q 的值 %q 不是浮点数: %w", key, v, err)
	}
	return f, nil
}

// Bool 取布尔配置（开关类，如 data_policy_enabled=false）。
func (s *Snapshot) Bool(key string) (bool, error) {
	v, err := s.raw(key)
	if err != nil {
		return false, err
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("config: 键 %q 的值 %q 不是布尔值: %w", key, v, err)
	}
	return b, nil
}

// Validate 校验快照里全部键的值都能按其默认值的形态解析。
//
// 在启动时调用（bootstrap），把"某个键被改成了非法值"暴露在启动阶段，
// 而不是等到采集器凌晨跑到那一行才失败。
func (s *Snapshot) Validate() error {
	for _, p := range Params {
		if p.EnvSourced {
			continue // 不在快照里，无从校验
		}
		var err error
		switch inferKind(p.Default) {
		case kindInt:
			_, err = s.Int(p.Key)
		case kindFloat:
			_, err = s.Float(p.Key)
		case kindBool:
			_, err = s.Bool(p.Key)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

type kind int

const (
	kindString kind = iota
	kindInt
	kindFloat
	kindBool
)

// inferKind 按文档默认值的字面形态推断类型。
//
// 之所以不在 §4bis 表里加一列"类型"：默认值本身已经表达了类型，
// 加一列就多一处可能与默认值矛盾的信息。
func inferKind(def string) kind {
	switch def {
	case "true", "false":
		return kindBool
	}
	if _, err := strconv.Atoi(def); err == nil {
		return kindInt
	}
	if _, err := strconv.ParseFloat(def, 64); err == nil {
		return kindFloat
	}
	return kindString
}
