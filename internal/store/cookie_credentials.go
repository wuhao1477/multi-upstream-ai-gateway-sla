package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrCookieCredentialInput   = errors.New("Cookie 格式无效；首次保存必须提供 Cookie 请求头值，最大 16 KiB")
	ErrCookieCredentialChanged = errors.New("Cookie 配置已变更，请重新验证")
	ErrCookieSecret            = errors.New("Cookie 无法解密，请检查部署密钥或重新导入")
	ErrCookieSite              = errors.New("Cookie 当前仅支持已登记的 NewAPI 公网 HTTPS 站点")
)

type SaveCookieCredentialInput struct {
	Enabled      bool   `json:"enabled"`
	CookieHeader string `json:"cookie_header,omitempty"`
}

// CookieCredential 仅在内存使用，秘密不参与序列化。
type CookieCredential struct {
	AccountID      int64
	BaseURL        string
	ExternalUserID string
	CookieHeader   string `json:"-"`
	State          string
	UpdatedAt      time.Time
}

// CookieCredentialStore 复用调用方事务，不持有数据库连接。
type CookieCredentialStore struct{ cipher cipher.AEAD }

func NewCookieCredentialStore(key string) (*CookieCredentialStore, error) {
	if key == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("SLA_COOKIE_SECRET_KEY 必须是 Base64 编码的 32 字节密钥")
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, ErrCookieSecret
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrCookieSecret
	}
	return &CookieCredentialStore{cipher: aead}, nil
}

// CookieOrigin 只规范化 origin；DNS 的出站检查由 Cookie 请求路径执行。
func CookieOrigin(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", ErrCookieSite
	}
	if !cookieHostAllowed(strings.ToLower(u.Hostname())) {
		return "", ErrCookieSite
	}
	return "https://" + strings.ToLower(u.Host), nil
}

func cookieHostAllowed(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return IsPublicCookieIP(ip)
	}
	return true
}

// IsPublicCookieIP 同时供 URL 字面量与连接时的 DNS 结果校验使用。
func IsPublicCookieIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && ip.Zone() == "" &&
		!netip.MustParsePrefix("100.64.0.0/10").Contains(ip)
}

func normalizeCookieHeader(raw string) (string, error) {
	if len(raw) > 16<<10 {
		return "", ErrCookieCredentialInput
	}
	for _, ch := range raw {
		if ch < 32 || ch == 127 {
			return "", ErrCookieCredentialInput
		}
	}
	raw = strings.TrimSpace(raw)
	if len(raw) >= 7 && strings.EqualFold(raw[:7], "Cookie:") {
		raw = strings.TrimSpace(raw[7:])
	}
	cookies, err := http.ParseCookie(raw)
	if err != nil || len(cookies) == 0 {
		return "", ErrCookieCredentialInput
	}
	pairs := make([]string, len(cookies))
	for i, cookie := range cookies {
		pairs[i] = cookie.String()
	}
	return strings.Join(pairs, "; "), nil
}

func cookieAAD(id int64, origin string) []byte {
	b, _ := json.Marshal([]any{id, origin, "cookie"})
	return b
}

func (s *CookieCredentialStore) seal(id int64, origin string, plain []byte) ([]byte, error) {
	if s == nil {
		return nil, ErrCookieSecret
	}
	nonce := make([]byte, s.cipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, ErrCookieSecret
	}
	return s.cipher.Seal(nonce, nonce, plain, cookieAAD(id, origin)), nil
}

func (s *CookieCredentialStore) open(id int64, origin string, encrypted []byte) ([]byte, error) {
	if s == nil || len(encrypted) < s.cipher.NonceSize()+s.cipher.Overhead() {
		return nil, ErrCookieSecret
	}
	n := s.cipher.NonceSize()
	plain, err := s.cipher.Open(nil, encrypted[:n], encrypted[n:], cookieAAD(id, origin))
	if err != nil {
		return nil, ErrCookieSecret
	}
	return plain, nil
}

// Save 必须在事务中调用；锁定账号/渠道，与身份变更和清除串行。
func (s *CookieCredentialStore) Save(ctx context.Context, db DBTX, id int64, in SaveCookieCredentialInput) (bool, error) {
	if s == nil {
		return false, ErrCookieSecret
	}
	origin, err := cookieAccountOrigin(ctx, db, id)
	if err != nil {
		return false, err
	}
	var encrypted []byte
	var enabled bool
	err = db.QueryRow(ctx, `SELECT cookie_ciphertext, enabled FROM collector_cookie_credentials WHERE account_id=$1`, id).Scan(&encrypted, &enabled)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	exists := err == nil
	value, same, err := s.cookieValue(id, origin, encrypted, exists, in.CookieHeader)
	if err != nil {
		return false, err
	}
	if exists && same && enabled == in.Enabled {
		return false, nil
	}
	encrypted, err = s.seal(id, origin, []byte(value))
	if err != nil {
		return false, err
	}
	_, err = db.Exec(ctx, `INSERT INTO collector_cookie_credentials (account_id, enabled, cookie_ciphertext)
VALUES ($1,$2,$3) ON CONFLICT (account_id) DO UPDATE
SET enabled=EXCLUDED.enabled, cookie_ciphertext=EXCLUDED.cookie_ciphertext,
    state='unverified', updated_at=clock_timestamp()`, id, in.Enabled, encrypted)
	return err == nil, err
}

func cookieAccountOrigin(ctx context.Context, db DBTX, id int64) (string, error) {
	var base, family string
	err := db.QueryRow(ctx, `SELECT c.base_url, c.site_family FROM upstream_accounts a
JOIN channels c ON c.id=a.channel_id WHERE a.id=$1 FOR UPDATE OF a FOR SHARE OF c`, id).Scan(&base, &family)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if family != "newapi" {
		return "", ErrCookieSite
	}
	return CookieOrigin(base)
}

func (s *CookieCredentialStore) cookieValue(id int64, origin string, encrypted []byte, exists bool, input string) (string, bool, error) {
	old, decryptErr := s.open(id, origin, encrypted)
	if input == "" {
		if !exists {
			return "", false, ErrCookieCredentialInput
		}
		return string(old), true, decryptErr
	}
	value, err := normalizeCookieHeader(input)
	return value, decryptErr == nil && string(old) == value, err
}

// Load 包含失效状态供显式验证使用；自动采集须跳过 expired/needs_action。
func (s *CookieCredentialStore) Load(ctx context.Context, db DBTX, id int64) (CookieCredential, error) {
	cred := CookieCredential{AccountID: id}
	var encrypted []byte
	err := db.QueryRow(ctx, `SELECT c.base_url, COALESCE(a.external_user_id,''), b.cookie_ciphertext, b.state, b.updated_at
FROM collector_cookie_credentials b JOIN upstream_accounts a ON a.id=b.account_id
JOIN channels c ON c.id=a.channel_id WHERE b.account_id=$1 AND b.enabled
AND a.status='active' AND c.status='enabled' AND c.site_family='newapi'`, id).
		Scan(&cred.BaseURL, &cred.ExternalUserID, &encrypted, &cred.State, &cred.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return cred, ErrNotFound
	}
	if err != nil {
		return cred, err
	}
	origin, err := CookieOrigin(cred.BaseURL)
	if err != nil {
		return cred, err
	}
	plain, err := s.open(id, origin, encrypted)
	if err != nil {
		return cred, err
	}
	cred.CookieHeader = string(plain)
	return cred, nil
}

// Differs 供只读导入预览比较秘密，不改变修订或失效状态。
func (s *CookieCredentialStore) Differs(ctx context.Context, db DBTX, id int64, header string) (bool, bool, error) {
	if header != "" {
		if s == nil {
			return false, false, ErrCookieSecret
		}
		var err error
		header, err = normalizeCookieHeader(header)
		if err != nil {
			return false, false, err
		}
	}
	var base string
	var encrypted []byte
	err := db.QueryRow(ctx, `SELECT c.base_url, b.cookie_ciphertext
FROM collector_cookie_credentials b JOIN upstream_accounts a ON a.id=b.account_id
JOIN channels c ON c.id=a.channel_id WHERE b.account_id=$1`, id).Scan(&base, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if header == "" {
		return false, true, nil
	}
	origin, err := CookieOrigin(base)
	if err != nil {
		return false, true, err
	}
	old, err := s.open(id, origin, encrypted)
	// 与 Save 相同：新 Cookie 可以替换旧密钥下已无法解密的值。
	return err != nil || string(old) != header, true, nil
}

// MarkState 只接受配置修订未改变的结果，不重建已清除的记录。
func (s *CookieCredentialStore) MarkState(ctx context.Context, db DBTX, cred CookieCredential, state string) error {
	if state != "ready" && state != "expired" && state != "needs_action" {
		return ErrCookieCredentialInput
	}
	tag, err := db.Exec(ctx, `UPDATE collector_cookie_credentials b SET state=$3
FROM upstream_accounts a JOIN channels c ON c.id=a.channel_id
WHERE b.account_id=$1 AND b.updated_at=$2 AND b.enabled AND a.id=b.account_id
AND a.status='active' AND c.status='enabled' AND c.base_url=$4 AND c.site_family='newapi'`,
		cred.AccountID, cred.UpdatedAt, state, cred.BaseURL)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrCookieCredentialChanged
	}
	return nil
}

// BindIdentity 必须在事务中调用，且仅接受已经原站验证的 ID。只补空值，不改绑账号。
func (s *CookieCredentialStore) BindIdentity(ctx context.Context, db DBTX, cred CookieCredential, userID string) (time.Time, error) {
	id, err := strconv.ParseInt(userID, 10, 64)
	if cred.ExternalUserID != "" || err != nil || id <= 0 {
		return time.Time{}, ErrCookieCredentialInput
	}
	var current string
	err = db.QueryRow(ctx, `SELECT COALESCE(a.external_user_id,'') FROM upstream_accounts a
JOIN channels c ON c.id=a.channel_id WHERE a.id=$1 FOR UPDATE OF a FOR SHARE OF c`, cred.AccountID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrCookieCredentialChanged
	}
	if err != nil {
		return time.Time{}, err
	}
	if current != "" {
		return time.Time{}, ErrCookieCredentialChanged
	}
	// 先检查 Cookie 修订、启用状态与站点；任一步失败均由调用方回滚。
	if err := s.MarkState(ctx, db, cred, "ready"); err != nil {
		return time.Time{}, err
	}
	if _, err := db.Exec(ctx, `UPDATE upstream_accounts SET external_user_id=$2 WHERE id=$1`, cred.AccountID, userID); err != nil {
		return time.Time{}, err
	}
	// 身份变更触发器会更新 Cookie 修订；同一事务内恢复 ready 并返回新修订。
	var revision time.Time
	err = db.QueryRow(ctx, `UPDATE collector_cookie_credentials SET state='ready'
WHERE account_id=$1 RETURNING updated_at`, cred.AccountID).Scan(&revision)
	return revision, err
}

// DeleteCookieCredential 必须在事务中调用，与 Save 使用相同的账号锁。
func DeleteCookieCredential(ctx context.Context, db DBTX, id int64) error {
	var exists int64
	if err := db.QueryRow(ctx, `SELECT id FROM upstream_accounts WHERE id=$1 FOR UPDATE`, id).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	_, err := db.Exec(ctx, `DELETE FROM collector_cookie_credentials WHERE account_id=$1`, id)
	return err
}
