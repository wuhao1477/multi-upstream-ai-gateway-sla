package collector

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// all-api-hub 的 WebDAV 备份：取回 + 解密。
//
// 形态不是猜的，取自上游源码 `src/services/webdav/webdavBackupEncryption.ts`
// 与 `webdavService.ts`（qixing-jk/all-api-hub，commit 85977e73，2026-09-13）。
// 解密这一侧的夹具由**上游自己的** encryptWebdavBackupContent 产出，
// 不是我方再写一遍加密器去自证 —— 那样两边会一起错（CLAUDE.md §1）。
//
// 解密后的明文就是平时手工导出的那份备份 JSON（上游的 BackupFullV2），
// 所以它直接喂给 ParseHubBackup，不需要第二套解析。

const (
	// hubEnvelopeType 等值是上游写进信封并在解析时校验的常量，
	// 用来把加密备份与明文备份区分开。四个都必须对上才认。
	hubEnvelopeType   = "all-api-hub-webdav-backup-encrypted"
	hubEnvelopeVer    = 1
	hubEnvelopeKDF    = "PBKDF2"
	hubEnvelopeCipher = "AES-GCM"

	// hubBackupFolder / hubBackupFile 复刻上游 ensureFilename 的默认路径：
	// 配置里给的是目录时，真正的备份文件在这个固定位置。
	hubBackupFolder = "all-api-hub-backup"
	hubBackupFile   = "all-api-hub-1-0.json"

	// hubMaxIterations 是我方加的上限，上游没有。
	//
	// iter 来自远端文件，而 PBKDF2 的开销与它成正比 —— 一个被改过的备份写上
	// 十亿次迭代，就能让采集进程在这里空转几分钟。上游跑在浏览器里、密码是
	// 用户当场输的，我们是无人值守的定时任务，这个差别决定了这里必须有个上限。
	hubMaxIterations = 5_000_000

	// hubMaxBackupBytes 与 ParseHubBackup 的 32MB 同量级：真实导出 200KB 上下，
	// 留足余量。没有它的话，一个超大响应会在解密前就把内存吃掉。
	hubMaxBackupBytes = 32 << 20
)

// ErrHubBackupEncrypted 表示远端是加密备份，但没给解密密码。
//
// 单列一个错误值是为了让上层能把它渲染成"去填密码"，而不是一句泛泛的解密失败 ——
// 两者的处置完全不同。
var ErrHubBackupEncrypted = errors.New("远端是加密备份，但没有配置解密密码")

// ErrHubBackupNotFound 表示 WebDAV 上还没有备份文件。
//
// 这不是故障：扩展那侧还没上传过一次就是这个状态，定时同步该把它报成
// "还没有备份"而不是"同步失败"。
var ErrHubBackupNotFound = errors.New("WebDAV 上还没有 all-api-hub 备份文件")

// ErrHubBackupDecrypt 表示信封取回来了但解不开。
//
// 与取回失败分开，是因为处置完全不同：这条要人去改密码（4xx，我方配置问题），
// 而取回失败是远端或网络的事（5xx）。混成一个的话，界面上只能给一句
// "同步失败"，而这两件事该做的下一步正好相反。
var ErrHubBackupDecrypt = errors.New("解密失败：密码不对，或备份文件已被篡改")

// hubEnvelope 是加密备份的信封（v1）。字段名取自上游，短名是它定的。
type hubEnvelope struct {
	Type   string `json:"type"`
	V      int    `json:"v"`
	KDF    string `json:"kdf"`
	Cipher string `json:"cipher"`
	Iter   int    `json:"iter"`
	Salt   string `json:"salt"`
	IV     string `json:"iv"`
	CT     string `json:"ct"`
}

// HubWebDAVConfig 是取备份需要的全部东西。
type HubWebDAVConfig struct {
	// URL 可以是目录（自动补出默认文件名）或直接指向 .json 文件。
	URL      string
	Username string
	Password string
}

// ResolveHubBackupURL 是 hubBackupURL 的导出名，给管理接口在保存配置时**当场**
// 校验地址用：填错的人正站在界面前，此刻告诉他比等定时器在日志里报错便宜。
func ResolveHubBackupURL(raw string) (string, error) { return hubBackupURL(raw) }

// hubBackupURL 复刻上游 ensureFilename：已经指向 .json 就原样用，
// 否则当成目录，补上 all-api-hub-backup/all-api-hub-1-0.json。
//
// 判据用 URL 的 path 而不是整个字符串：带 ?token=…json 的直链不该被误判成文件。
func hubBackupURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("WebDAV 地址为空")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("WebDAV 地址无法解析: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("WebDAV 地址必须是 http/https，收到 %q", u.Scheme)
	}
	if strings.HasSuffix(strings.ToLower(u.Path), ".json") {
		return u.String(), nil
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + hubBackupFolder + "/" + hubBackupFile
	return u.String(), nil
}

// FetchHubBackup 从 WebDAV 取回备份原文（可能是明文 JSON，也可能是加密信封）。
//
// 只发 GET —— 我们是只读消费方，不上传、不建目录，所以用不着 WebDAV 的那套
// PROPFIND/MKCOL 动词，也就不需要引一个 WebDAV 库。
func FetchHubBackup(
	ctx context.Context, client *http.Client, cfg HubWebDAVConfig,
) ([]byte, error) {
	target, err := hubBackupURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(cfg.Username, cfg.Password)
	req.Header.Set("Accept", "application/json")

	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("取 WebDAV 备份失败: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	switch {
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone:
		return nil, ErrHubBackupNotFound
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("WebDAV 鉴权失败（HTTP %d）：检查用户名与密码", res.StatusCode)
	case res.StatusCode < 200 || res.StatusCode >= 300:
		return nil, fmt.Errorf("取 WebDAV 备份失败：HTTP %d", res.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, hubMaxBackupBytes))
	if err != nil {
		return nil, fmt.Errorf("读 WebDAV 备份正文失败: %w", err)
	}
	// 空正文也算"还没有备份"：坚果云等实现会先建出一个 0 字节的占位文件。
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, ErrHubBackupNotFound
	}
	return body, nil
}

// DecryptHubBackup 把取回的原文归一成明文备份 JSON。
//
// 明文备份原样返回 —— 上游的加密是**可选**的，两种形态都要能读
// （它自己的 downloadBackup 也是这个次序：先试信封，不是就当明文）。
func DecryptHubBackup(raw []byte, password string) ([]byte, error) {
	env, ok := parseHubEnvelope(raw)
	if !ok {
		return raw, nil
	}
	if password == "" {
		return nil, ErrHubBackupEncrypted
	}
	if env.Iter <= 0 || env.Iter > hubMaxIterations {
		return nil, fmt.Errorf("加密备份的 PBKDF2 迭代数 %d 不在 1..%d 内",
			env.Iter, hubMaxIterations)
	}
	salt, err := base64.StdEncoding.DecodeString(env.Salt)
	if err != nil {
		return nil, fmt.Errorf("加密备份的 salt 不是合法 base64: %w", err)
	}
	iv, err := base64.StdEncoding.DecodeString(env.IV)
	if err != nil {
		return nil, fmt.Errorf("加密备份的 iv 不是合法 base64: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(env.CT)
	if err != nil {
		return nil, fmt.Errorf("加密备份的密文不是合法 base64: %w", err)
	}

	// PBKDF2-HMAC-SHA256 → 32 字节 = AES-256。三个参数都由信封给，
	// 与上游 deriveAesKeyFromPassword 一一对应。
	key, err := pbkdf2.Key(sha256.New, password, salt, env.Iter, 32)
	if err != nil {
		return nil, fmt.Errorf("派生解密密钥失败: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("构造 AES 失败: %w", err)
	}
	// WebCrypto 的 AES-GCM 把 16 字节认证标签**附在密文尾部**，且默认不带 AAD——
	// Go 的 gcm.Open 正是这个约定，所以密文可以整块传进去，不用自己切标签。
	gcm, err := cipher.NewGCMWithNonceSize(block, len(iv))
	if err != nil {
		return nil, fmt.Errorf("构造 AES-GCM 失败（iv %d 字节）: %w", len(iv), err)
	}
	plain, err := gcm.Open(nil, iv, ct, nil)
	if err != nil {
		// 不透传底层错误：GCM 认证失败时"密码错"和"文件被改过"在密码学上
		// 不可区分，写成两种可能比装作知道是哪一种诚实。
		return nil, ErrHubBackupDecrypt
	}
	return plain, nil
}

// parseHubEnvelope 判断这份内容是不是加密信封。
//
// 四个常量字段全中才认，与上游 tryParseEncryptedWebdavBackupEnvelope 同判据：
// 明文备份也是 JSON，只看"能不能解析成 JSON"分不开两者。
func parseHubEnvelope(raw []byte) (hubEnvelope, bool) {
	var env hubEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return hubEnvelope{}, false
	}
	ok := env.Type == hubEnvelopeType &&
		env.V == hubEnvelopeVer &&
		env.KDF == hubEnvelopeKDF &&
		env.Cipher == hubEnvelopeCipher &&
		env.Salt != "" && env.IV != "" && env.CT != ""
	return env, ok
}
