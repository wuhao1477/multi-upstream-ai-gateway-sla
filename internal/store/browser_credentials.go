package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrBrowserCredentialInput   = errors.New("浏览器凭据不完整；首次保存或更换用户名必须输入密码")
	ErrBrowserCredentialChanged = errors.New("浏览器配置已变更，旧会话已作废")
	ErrBrowserSecret            = errors.New("浏览器凭据无法解密，请检查部署密钥或重新配置")
	ErrBrowserSite              = errors.New("浏览器凭据当前仅支持已登记的 NewAPI HTTPS 站点")
)

type SaveBrowserCredentialInput struct {
	Enabled  bool   `json:"enabled"`
	Username string `json:"username"`
	Password string `json:"password,omitempty"`
}

// BrowserCredential 仅在内存使用，禁止序列化秘密。
type BrowserCredential struct {
	AccountID int64
	BaseURL   string
	Username  string
	Password  string `json:"-"`
	Session   []byte `json:"-"`
	UpdatedAt time.Time
}

// BrowserCredentialStore 只保存加密器，数据库事务由调用方持有。
type BrowserCredentialStore struct{ cipher cipher.AEAD }

func NewBrowserCredentialStore(key string) (*BrowserCredentialStore, error) {
	if key == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("SLA_BROWSER_SECRET_KEY 必须是 Base64 编码的 32 字节密钥")
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, ErrBrowserSecret
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrBrowserSecret
	}
	return &BrowserCredentialStore{cipher: aead}, nil
}

func browserOrigin(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return "", ErrBrowserSite
	}
	return "https://" + strings.ToLower(u.Host), nil
}

func browserAAD(id int64, origin, purpose string) []byte {
	b, _ := json.Marshal([]any{id, origin, purpose})
	return b
}

func (s *BrowserCredentialStore) seal(id int64, origin, purpose string, plain []byte) ([]byte, error) {
	if s == nil {
		return nil, ErrBrowserSecret
	}
	nonce := make([]byte, s.cipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, ErrBrowserSecret
	}
	return s.cipher.Seal(nonce, nonce, plain, browserAAD(id, origin, purpose)), nil
}

func (s *BrowserCredentialStore) open(id int64, origin, purpose string, encrypted []byte) ([]byte, error) {
	if s == nil || len(encrypted) < s.cipher.NonceSize()+s.cipher.Overhead() {
		return nil, ErrBrowserSecret
	}
	n := s.cipher.NonceSize()
	plain, err := s.cipher.Open(nil, encrypted[:n], encrypted[n:], browserAAD(id, origin, purpose))
	if err != nil {
		return nil, ErrBrowserSecret
	}
	return plain, nil
}

// Save 必须在事务中调用；锁定账号/渠道，避免保存时地址或身份被并发修改。
func (s *BrowserCredentialStore) Save(ctx context.Context, db DBTX, id int64, in SaveBrowserCredentialInput) error {
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" || len(in.Username) > 320 || len(in.Password) > 4096 {
		return ErrBrowserCredentialInput
	}
	var base, family string
	err := db.QueryRow(ctx, `SELECT c.base_url, c.site_family FROM upstream_accounts a
JOIN channels c ON c.id=a.channel_id WHERE a.id=$1 FOR UPDATE OF a FOR SHARE OF c`, id).Scan(&base, &family)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if family != "newapi" {
		return ErrBrowserSite
	}
	origin, err := browserOrigin(base)
	if err != nil {
		return err
	}
	encrypted, err := s.passwordForSave(ctx, db, id, origin, in)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `INSERT INTO collector_browser_credentials (account_id, enabled, username, password_ciphertext)
VALUES ($1,$2,$3,$4) ON CONFLICT (account_id) DO UPDATE
SET enabled=EXCLUDED.enabled, username=EXCLUDED.username, password_ciphertext=EXCLUDED.password_ciphertext,
    session_ciphertext=NULL, state='unverified', updated_at=clock_timestamp()`, id, in.Enabled, in.Username, encrypted)
	return err
}

func (s *BrowserCredentialStore) passwordForSave(ctx context.Context, db DBTX, id int64, origin string, in SaveBrowserCredentialInput) ([]byte, error) {
	if in.Password != "" {
		return s.seal(id, origin, "password", []byte(in.Password))
	}
	var username string
	var encrypted []byte
	err := db.QueryRow(ctx, `SELECT username, password_ciphertext FROM collector_browser_credentials WHERE account_id=$1`, id).Scan(&username, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBrowserCredentialInput
	}
	if err != nil {
		return nil, err
	}
	if username != in.Username {
		return nil, ErrBrowserCredentialInput
	}
	_, err = s.open(id, origin, "password", encrypted)
	return encrypted, err
}

func (s *BrowserCredentialStore) Load(ctx context.Context, db DBTX, id int64) (BrowserCredential, error) {
	cred := BrowserCredential{AccountID: id}
	var password, session []byte
	err := db.QueryRow(ctx, `SELECT c.base_url, b.username, b.password_ciphertext, b.session_ciphertext, b.updated_at
FROM collector_browser_credentials b JOIN upstream_accounts a ON a.id=b.account_id
JOIN channels c ON c.id=a.channel_id WHERE b.account_id=$1 AND b.enabled
AND b.state NOT IN ('invalid','needs_action') AND a.status='active' AND c.status='enabled' AND c.site_family='newapi'`, id).
		Scan(&cred.BaseURL, &cred.Username, &password, &session, &cred.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return cred, ErrNotFound
	}
	if err != nil {
		return cred, err
	}
	origin, err := browserOrigin(cred.BaseURL)
	if err != nil {
		return cred, err
	}
	plain, err := s.open(id, origin, "password", password)
	if err != nil {
		return cred, err
	}
	cred.Password = string(plain)
	if len(session) > 0 {
		cred.Session, err = s.open(id, origin, "session", session)
	}
	return cred, err
}

// SaveSession 只接受配置修订未改变的结果，清除后不会重建记录。
func (s *BrowserCredentialStore) SaveSession(ctx context.Context, db DBTX, cred BrowserCredential, session []byte, state string) error {
	if state != "ready" && state != "invalid" && state != "needs_action" {
		return ErrBrowserCredentialInput
	}
	var encrypted []byte
	if state == "ready" {
		origin, err := browserOrigin(cred.BaseURL)
		if err != nil {
			return err
		}
		encrypted, err = s.seal(cred.AccountID, origin, "session", session)
		if err != nil {
			return err
		}
	}
	tag, err := db.Exec(ctx, `UPDATE collector_browser_credentials b SET session_ciphertext=$3, state=$4
FROM upstream_accounts a JOIN channels c ON c.id=a.channel_id
WHERE b.account_id=$1 AND b.updated_at=$2 AND b.enabled AND a.id=b.account_id
AND a.status='active' AND c.status='enabled' AND c.base_url=$5 AND c.site_family='newapi'`,
		cred.AccountID, cred.UpdatedAt, encrypted, state, cred.BaseURL)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrBrowserCredentialChanged
	}
	return nil
}

// DeleteBrowserCredential 必须在事务中调用，与 Save 使用相同的账号锁。
func DeleteBrowserCredential(ctx context.Context, db DBTX, id int64) error {
	// 与保存锁同一账号，保证并发清除不会被在途保存重新创建。
	var exists int64
	if err := db.QueryRow(ctx, `SELECT id FROM upstream_accounts WHERE id=$1 FOR UPDATE`, id).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	_, err := db.Exec(ctx, `DELETE FROM collector_browser_credentials WHERE account_id=$1`, id)
	return err
}
