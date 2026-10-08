package collection

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/gob"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

var errCookieIdentityUnavailable = errors.Join(collector.ErrCookieNeedsAction, collector.ErrCookieIdentityUnavailable)

// cookieUserID 只读候选 ID，不验证签名；必须用原 Cookie 请求 /api/user/self 确认。
// NewAPI v1.0.0-rc.21 main.go 使用仅签名的 CookieStore，controller/user.go 写入 id。
// ponytail: 仅支持默认 securecookie/Gob 格式；其他格式通过完整 all-api-hub 备份提供 ID。
func cookieUserID(header string) (string, error) {
	cookies := (&http.Request{Header: http.Header{"Cookie": {header}}}).CookiesNamed("session")
	// securecookie 默认最长 4096 字节；不接受任意大小的 Gob 输入。
	if len(cookies) != 1 || len(cookies[0].Value) > 4096 {
		return "", errCookieIdentityUnavailable
	}
	outer, err := base64.URLEncoding.DecodeString(cookies[0].Value)
	if err != nil {
		return "", errCookieIdentityUnavailable
	}
	parts := bytes.SplitN(outer, []byte("|"), 3)
	if len(parts) != 3 || len(parts[2]) != sha256.Size {
		return "", errCookieIdentityUnavailable
	}
	payload, err := base64.URLEncoding.DecodeString(string(parts[1]))
	if err != nil {
		return "", errCookieIdentityUnavailable
	}
	var values map[any]any
	if err := gob.NewDecoder(bytes.NewReader(payload)).Decode(&values); err != nil {
		return "", errCookieIdentityUnavailable
	}
	id, ok := values["id"].(int)
	if !ok || id <= 0 {
		return "", errCookieIdentityUnavailable
	}
	return strconv.Itoa(id), nil
}

func (a *CookieAccess) confirmIdentity(ctx context.Context, cred store.CookieCredential, userID string) (time.Time, error) {
	if cred.ExternalUserID != "" {
		return cred.UpdatedAt, a.mark(ctx, cred, "ready")
	}
	conn, release, err := a.Pool.Acquire(ctx)
	if err != nil {
		return time.Time{}, errors.New("保存 Cookie 身份失败")
	}
	defer release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	revision, err := a.Credentials.BindIdentity(ctx, tx, cred, userID)
	if err != nil {
		return time.Time{}, err
	}
	return revision, tx.Commit(ctx)
}
