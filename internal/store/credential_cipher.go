package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
)

// 非 Cookie 凭证的加密存储（D1，#34）：upstream_keys.secret、collector_credentials 的
// access/refresh token、hub_sync_config 的 WebDAV 地址与两个密码。
//
// 密钥来自部署环境 SLA_CREDENTIAL_SECRET_KEY，与 Cookie 密钥独立、不从 ADMIN_TOKEN
// 派生。未配置时进程照常启动（库里没有秘密时不需要它），但任何读写秘密的路径都会
// 明确失败 —— **不退回明文**。

var (
	ErrCredentialKeyMissing = errors.New("未配置 SLA_CREDENTIAL_SECRET_KEY，无法读写上游凭证")
	ErrCredentialDecrypt    = errors.New("上游凭证解密失败：部署密钥与加密时不一致，或密文已被改动")
)

// 密文 = 格式版本 ‖ 密钥版本 ‖ nonce ‖ AES-GCM 密文。两个版本字节也进 AAD，
// 改了它们就解不开。轮换密钥时递增密钥版本，旧值据此找到旧密钥。
const (
	credentialFormatV1 byte = 1
	credentialKeyV1    byte = 1
)

var credentialAEAD atomic.Pointer[cipher.AEAD]

// ConfigureCredentialKey 在进程启动、迁移之前调用一次。空值表示未配置。
func ConfigureCredentialKey(key string) error {
	if key == "" {
		credentialAEAD.Store(nil)
		return nil
	}
	aead, err := newAESGCM(key, "SLA_CREDENTIAL_SECRET_KEY")
	if err != nil {
		return err
	}
	credentialAEAD.Store(&aead)
	return nil
}

// newAESGCM 是 Cookie 与凭证共用的加密组件，不依赖任何表。
func newAESGCM(key, name string) (cipher.AEAD, error) {
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("%s 必须是 Base64 编码的 32 字节密钥", name)
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// gcmSeal 返回 nonce ‖ 密文，每次调用生成新的随机 nonce。
func gcmSeal(aead cipher.AEAD, plain, aad []byte) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plain, aad), nil
}

func gcmOpen(aead cipher.AEAD, sealed, aad []byte) ([]byte, error) {
	n := aead.NonceSize()
	if len(sealed) < n+aead.Overhead() {
		return nil, errors.New("密文过短")
	}
	return aead.Open(nil, sealed[:n], sealed[n:], aad)
}

// credentialAAD 绑定表名、记录 ID 与字段名：密文不能被挪到别的账号、别的行或别的列。
func credentialAAD(table string, id int64, field string) []byte {
	b, _ := json.Marshal([]any{table, id, field})
	return append([]byte{credentialFormatV1, credentialKeyV1}, b...)
}

// sealCredential 加密一个秘密。空串表示"没有这个秘密"，存 NULL，不需要密钥。
func sealCredential(table string, id int64, field, plain string) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	aead := credentialAEAD.Load()
	if aead == nil {
		return nil, ErrCredentialKeyMissing
	}
	sealed, err := gcmSeal(*aead, []byte(plain), credentialAAD(table, id, field))
	if err != nil {
		return nil, fmt.Errorf("加密 %s.%s（记录 %d）失败", table, field, id)
	}
	return append([]byte{credentialFormatV1, credentialKeyV1}, sealed...), nil
}

// openCredential 解密一个秘密。NULL 即没有秘密；其余任何失败都报错，不当作"未配置"。
func openCredential(table string, id int64, field string, encrypted []byte) (string, error) {
	if encrypted == nil {
		return "", nil
	}
	aead := credentialAEAD.Load()
	if aead == nil {
		return "", ErrCredentialKeyMissing
	}
	if len(encrypted) < 2 || encrypted[0] != credentialFormatV1 || encrypted[1] != credentialKeyV1 {
		return "", fmt.Errorf("%w（%s.%s，记录 %d：未知的密文版本）", ErrCredentialDecrypt, table, field, id)
	}
	plain, err := gcmOpen(*aead, encrypted[2:], credentialAAD(table, id, field))
	if err != nil {
		return "", fmt.Errorf("%w（%s.%s，记录 %d）", ErrCredentialDecrypt, table, field, id)
	}
	return string(plain), nil
}
