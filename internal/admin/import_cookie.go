package admin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

func validateHubCookie(a collector.HubAccount, d collector.DetectResult, base string) error {
	if a.AuthType != "cookie" {
		return nil
	}
	if d.Family != collector.FamilyNewAPI {
		return store.ErrCookieSite
	}
	if _, err := store.CookieOrigin(base); err != nil {
		return err
	}
	id, err := strconv.ParseInt(a.UserID(), 10, 64)
	if err != nil || id <= 0 {
		return errors.New("导入 Cookie 需要 account_info.id 中的有效上游用户 ID")
	}
	return nil
}

func (s *Server) saveImportedCookie(ctx context.Context, db store.DBTX, a collector.HubAccount, accountID int64) (bool, error) {
	var enabled bool
	err := db.QueryRow(ctx, `SELECT enabled FROM collector_cookie_credentials WHERE account_id=$1`, accountID).Scan(&enabled)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	// 导出里没有 Cookie：与没令牌的条目同样处理 —— 渠道/账号照建，
	// "没有凭证"的告警探测阶段已写上；已有配置原样保留。
	if a.CookieAuth.SessionCookie == "" && (!exists || !a.Disabled || !enabled) {
		return false, nil
	}
	if !exists {
		enabled = true
	}
	return s.CookieCredentials.Save(ctx, db, accountID, store.SaveCookieCredentialInput{
		Enabled: enabled && !a.Disabled, CookieHeader: a.CookieAuth.SessionCookie,
	})
}

// previewCookieImport 只读取本地台账与密文，不保存、不验证 Cookie、不调用 Key 读取。
func (s *Server) previewCookieImport(ctx context.Context, db store.DBTX, a collector.HubAccount, d collector.DetectResult, it *collector.HubImportItem) error {
	base, err := validateBaseURL(a.SiteURL)
	if err != nil {
		return err
	}
	if err := validateHubCookie(a, d, base); err != nil {
		return err
	}
	channels, err := store.ListChannels(ctx, db)
	if err != nil {
		return err
	}
	for _, ch := range channels {
		normalized, err := validateBaseURL(ch.BaseURL)
		if err == nil && normalized == base {
			return s.previewExistingCookie(ctx, db, a, d, ch, it)
		}
	}
	if a.CookieAuth.SessionCookie == "" {
		return nil // 状态已是 would_import，"没有凭证"的告警探测阶段写过
	}
	if _, _, err := s.CookieCredentials.Differs(ctx, db, 0, a.CookieAuth.SessionCookie); err != nil {
		return err
	}
	it.Status = "would_import"
	it.Changes = append(it.Changes, "将登记 Cookie（尚未验证）")
	return nil
}

func (s *Server) previewExistingCookie(ctx context.Context, db store.DBTX, a collector.HubAccount, d collector.DetectResult, ch store.Channel, it *collector.HubImportItem) error {
	if ch.SiteFamily != "newapi" && !needsFamilyRepair(ch.SiteFamily, d.Family) {
		return store.ErrCookieSite
	}
	accountID, err := importAccountID(ctx, db, ch.ID, a.UserID())
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	differs, err := s.previewCookieChange(ctx, db, a, accountID)
	if err != nil {
		return err
	}
	missing, err := s.incompleteParts(ctx, db, ch.ID, ch.SiteFamily, d.Family, a)
	if err != nil {
		return err
	}
	it.ChannelID, it.AccountID = ch.ID, accountID
	it.Status = "unchanged"
	if len(missing) > 0 {
		it.Status = "would_import"
		it.Changes = append(it.Changes, "预计补齐"+strings.Join(missing, "、"))
	} else if differs {
		it.Status = "updated"
		it.Changes = append(it.Changes, "Cookie 配置将更新")
	}
	return nil
}

func (s *Server) previewCookieChange(ctx context.Context, db store.DBTX, a collector.HubAccount, accountID int64) (bool, error) {
	differs, exists, err := s.CookieCredentials.Differs(ctx, db, accountID, a.CookieAuth.SessionCookie)
	if err != nil {
		return false, err
	}
	if a.Disabled && exists {
		var enabled bool
		if err := db.QueryRow(ctx, `SELECT enabled FROM collector_cookie_credentials WHERE account_id=$1`, accountID).Scan(&enabled); err != nil {
			return false, err
		}
		differs = differs || enabled
	}
	if differs && s.CookieCredentials == nil {
		return false, store.ErrCookieSecret
	}
	return differs, nil
}

func (s *Server) previewHubCookies(ctx context.Context, accounts []collector.HubAccount, detects []collector.DetectResult, result *collector.HubImportResult) {
	conn, release, err := s.DB.Acquire(ctx)
	if err != nil {
		for i, a := range accounts {
			if a.AuthType == "cookie" && result.Items[i].Status == "would_import" {
				result.Items[i].Status, result.Items[i].Reason = "failed", "无法读取本地 Cookie 配置进行比较"
			}
		}
		return
	}
	defer release()
	for i, a := range accounts {
		if a.AuthType != "cookie" || result.Items[i].Status != "would_import" {
			continue
		}
		if err := s.previewCookieImport(ctx, conn, a, detects[i], &result.Items[i]); err != nil {
			result.Items[i].Status = "failed"
			result.Items[i].Reason = fmt.Sprintf("Cookie 预览失败：%s", shortErr(err))
		}
	}
}
