package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// confirmTTL 是 confirm_token 的有效期（09 §3 定 5 分钟）。
const confirmTTL = 5 * time.Minute

// tokenStore 保管一次性 confirm_token。
//
// **为什么放内存而不落库**：token 的语义是"同一个人在 5 分钟内完成两步操作"，
// 它不需要跨实例共享 —— preview 与 apply 由同一个运维在同一个会话里连着做，
// Caddy 不代理 /admin/*（06 §1），管理请求本来就只到一个实例。
// 落库反而要处理清理与过期扫描，收益为零。
//
// ⚠️ 代价明示：实例重启会让在途 token 失效，运维需重新 preview。
// 这是可接受的 —— 重新预览的成本远低于引入一张需要 GC 的表。
type tokenStore struct {
	mu     sync.Mutex
	tokens map[string]tokenEntry
}

type tokenEntry struct {
	// diffHash 绑定**具体 diff**（09 §3）：改了值再 apply 会因哈希不符被拒，
	// 防"确认了 A 却提交了 B"。
	diffHash  string
	expiresAt time.Time
}

func newTokenStore() *tokenStore {
	return &tokenStore{tokens: map[string]tokenEntry{}}
}

// diffHash 把"这次要改什么"归一成一个哈希。
// 包含期望版本：基于 v7 的确认不能用来提交 v8 的改动。
func diffHash(paramKey, newValue string, expectedVersion int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d",
		paramKey, newValue, expectedVersion)))
	return hex.EncodeToString(h[:])
}

// issue 签发一个绑定该 diff 的一次性令牌。
func (s *tokenStore) issue(paramKey, newValue string, expectedVersion int) (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成令牌: %w", err)
	}
	tok := hex.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.tokens[tok] = tokenEntry{
		diffHash:  diffHash(paramKey, newValue, expectedVersion),
		expiresAt: time.Now().Add(confirmTTL),
	}
	return tok, nil
}

// consume 校验并**一次性消耗**令牌。
//
// 返回 false 的三种情形都不该放行：不存在、已过期、diff 不符。
// 不区分原因是刻意的 —— 对调用方而言处置相同（重新 preview），
// 而区分会泄露"这个 token 存在但 diff 不对"这类信息。
func (s *tokenStore) consume(tok, paramKey, newValue string, expectedVersion int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.tokens[tok]
	if !ok {
		return false
	}
	// 无论是否匹配都删除：令牌是一次性的，试错不该有第二次机会。
	delete(s.tokens, tok)

	if time.Now().After(e.expiresAt) {
		return false
	}
	return e.diffHash == diffHash(paramKey, newValue, expectedVersion)
}

// pruneLocked 清理过期令牌。在 issue 时顺带做，避免额外的后台任务 ——
// 管理平面调用频率低，令牌表不会长到需要专门 GC。
func (s *tokenStore) pruneLocked() {
	now := time.Now()
	for k, v := range s.tokens {
		if now.After(v.expiresAt) {
			delete(s.tokens, k)
		}
	}
}
