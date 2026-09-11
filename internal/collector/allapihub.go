package collector

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// all-api-hub 导入（https://github.com/qixing-jk/all-api-hub）。
//
// 该工具是浏览器扩展，导出一份含全部站点与账号的 JSON。对我们的价值是：
// 运维手上已有几十上百个站点，逐个手填不现实。
//
// ⚠️ **一条铁律：不信任导出数据里的 `site_type`。**
// 实测 106 站中有 2 站声明错（redacted-channel-12 标 sub2api 实为 newapi、
// redacted-channel-16 标 new-api 实为 sub2api）。站型决定全部字段映射，信错一次
// 就会把余额、额度、模型全部解析错，而且**错得静默**。故导入一律以我方
// `Detect()` 的结果为准，声明值只作为对照记录（用于报告"导出侧标错了几站"）。

// HubBackup 是 all-api-hub 的导出结构（只取我们需要的部分）。
type HubBackup struct {
	Version  string `json:"version"`
	Accounts struct {
		Accounts []HubAccount `json:"accounts"`
	} `json:"accounts"`
}

// HubAccount 是导出里的一个站点账号。
type HubAccount struct {
	ID       string `json:"id"`
	SiteName string `json:"site_name"`
	SiteURL  string `json:"site_url"`
	// SiteType 是导出侧的站型声明。**仅作对照，不作依据**（见上方铁律）。
	SiteType string `json:"site_type"`
	// ExchangeRate 是该站的显示货币换算（多为 7.2/7.3 即人民币每美元）。
	//
	// ⚠️ 实测它并非总是汇率：有 22 站为 1、还有 0.2/2/5/10 的值 ——
	// 那些站的额度单位本身不锚定美元。这正是 ISSUE-005 §6 T-1「充值倍率」
	// 要处理的维度。P1 只**原样记录**、不参与任何计算（P1 无成本排序），
	// 留给 P3 决定如何折算。
	ExchangeRate float64 `json:"exchange_rate"`
	AuthType     string  `json:"authType"`
	Disabled     bool    `json:"disabled"`
	Health       struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	} `json:"health"`
	AccountInfo struct {
		// ID 是上游的数字用户 ID —— NewAPI 系的用户 ID 头正需要它（04 §3.1）。
		ID json.RawMessage `json:"id"`
		// AccessToken 是明文凭证。**绝不进日志、报告或任何响应。**
		AccessToken string          `json:"access_token"`
		Username    string          `json:"username"`
		Quota       json.RawMessage `json:"quota"`
	} `json:"account_info"`
}

// UserID 返回上游数字用户 ID 的字符串形式。
// 导出里它可能是数字也可能是字符串，故用 RawMessage 兜住两种形态。
func (a HubAccount) UserID() string {
	s := strings.Trim(string(a.AccountInfo.ID), `"`)
	if s == "null" {
		return ""
	}
	return s
}

// QuotaRaw 返回导出记录的额度原值（仅作对照，不作依据）。
//
// 为何"仅作对照"：导出里的 quota 是**上游原始单位**，换算需要
// quota_per_unit，而那必须逐站探测（实测有 500000 与 1000000 两种，
// 写死会让后者的余额算成 2 倍）。真值一律以我们自己采集的为准。
func (a HubAccount) QuotaRaw() float64 {
	s := strings.Trim(string(a.AccountInfo.Quota), `"`)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// HasCredential 报告该条目是否带可用凭证。
func (a HubAccount) HasCredential() bool {
	return strings.TrimSpace(a.AccountInfo.AccessToken) != ""
}

// ParseHubBackup 解析导出文件。
func ParseHubBackup(r io.Reader) (*HubBackup, error) {
	// 导出文件可达数百 KB，但不该无上限 —— 管理接口不接受任意大的上传
	lr := io.LimitReader(r, 32<<20)
	var b HubBackup
	if err := json.NewDecoder(lr).Decode(&b); err != nil {
		return nil, fmt.Errorf("解析 all-api-hub 导出失败: %w", err)
	}
	if len(b.Accounts.Accounts) == 0 {
		return nil, fmt.Errorf("导出文件里没有站点（accounts.accounts 为空）")
	}
	return &b, nil
}

// HubImportItem 是一个站点的导入结果。
type HubImportItem struct {
	SiteName string `json:"site_name"`
	SiteURL  string `json:"site_url"`
	// DeclaredFamily 是导出侧的声明，DetectedFamily 是我方探测结果。
	// 两者不一致时**以探测为准**，并在报告里标出来。
	DeclaredFamily string `json:"declared_family"`
	DetectedFamily Family `json:"detected_family,omitempty"`
	Mismatch       bool   `json:"family_mismatch,omitempty"`
	ChannelID      int64  `json:"channel_id,omitempty"`
	// AccountID 只供导入完成后定位该条目的账号，不进入响应。
	AccountID int64  `json:"-"`
	Status    string `json:"status"` // imported | skipped | failed
	Reason    string `json:"reason,omitempty"`
	// Warning 记录可用但需注意的情形（如开盾站点采集不可行）。
	Warning      string `json:"warning,omitempty"`
	KeysFound    int    `json:"keys_found,omitempty"`
	KeysImported int    `json:"keys_imported,omitempty"`
	KeysSkipped  int    `json:"keys_skipped,omitempty"`
	KeysFailed   int    `json:"keys_failed,omitempty"`
	KeysDeferred int    `json:"keys_deferred,omitempty"`
}

// HubImportResult 是整次导入的汇总。
type HubImportResult struct {
	Total        int             `json:"total"`
	Imported     int             `json:"imported"`
	Skipped      int             `json:"skipped"`
	Failed       int             `json:"failed"`
	Mismatches   int             `json:"family_mismatches"`
	Shielded     int             `json:"shielded_sites"`
	NoCredential int             `json:"without_credential"`
	KeysFound    int             `json:"keys_found"`
	KeysImported int             `json:"keys_imported"`
	KeysSkipped  int             `json:"keys_skipped"`
	KeysFailed   int             `json:"keys_failed"`
	KeysDeferred int             `json:"keys_deferred"`
	Items        []HubImportItem `json:"items"`
}

// normalizeFamily 把 all-api-hub 的 site_type 映射到我们的家族名。
//
// 只用于**对照**（判断导出侧是否标错），不用于决定适配器。
// 映射在 internal/admin 的 normalizeDeclared —— 唯一调用点所在的包。
// 这里曾有一份逐字相同、从未被调用的 normalizeFamily。
