package admin

import (
	"testing"
	"time"
)

func TestTokenRoundTrip(t *testing.T) {
	s := newTokenStore()
	tok, err := s.issue("max_hops", "5", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !s.consume(tok, "max_hops", "5", 1) {
		t.Fatal("刚签发的令牌应可消耗")
	}
}

// 令牌一次性：第二次必须失败，否则试错可以无限重来。
func TestTokenIsSingleUse(t *testing.T) {
	s := newTokenStore()
	tok, _ := s.issue("max_hops", "5", 1)
	if !s.consume(tok, "max_hops", "5", 1) {
		t.Fatal("首次消耗应成功")
	}
	if s.consume(tok, "max_hops", "5", 1) {
		t.Fatal("令牌应一次性——重复消耗必须失败")
	}
}

// 09 §3 的核心防护：令牌绑定具体 diff，防"确认了 A 却提交了 B"。
func TestTokenBoundToDiff(t *testing.T) {
	s := newTokenStore()
	tok, _ := s.issue("probe_global_cost_cap_ratio", "0.01", 3)

	if s.consume(tok, "probe_global_cost_cap_ratio", "0.99", 3) {
		t.Fatal("换了值仍能提交——防的就是这个（确认 0.01 却提交 0.99）")
	}
}

func TestTokenBoundToKey(t *testing.T) {
	s := newTokenStore()
	tok, _ := s.issue("max_hops", "5", 1)
	if s.consume(tok, "min_hop_ms", "5", 1) {
		t.Fatal("换了键仍能提交")
	}
}

// 期望版本也进哈希：基于 v7 的确认不能用来提交 v8 的改动。
func TestTokenBoundToExpectedVersion(t *testing.T) {
	s := newTokenStore()
	tok, _ := s.issue("max_hops", "5", 7)
	if s.consume(tok, "max_hops", "5", 8) {
		t.Fatal("换了期望版本仍能提交——那会绕过乐观锁的语义")
	}
}

// 失败的消耗也要销毁令牌：否则攻击者可以拿一个令牌反复试不同的值。
func TestFailedConsumeStillBurnsToken(t *testing.T) {
	s := newTokenStore()
	tok, _ := s.issue("max_hops", "5", 1)
	if s.consume(tok, "max_hops", "999", 1) {
		t.Fatal("值不符应失败")
	}
	if s.consume(tok, "max_hops", "5", 1) {
		t.Fatal("失败的尝试也应销毁令牌——否则可用同一令牌反复试值")
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	s := newTokenStore()
	tok, _ := s.issue("max_hops", "5", 1)
	// 直接改过期时间，避免测试里真等 5 分钟
	s.mu.Lock()
	e := s.tokens[tok]
	e.expiresAt = time.Now().Add(-time.Second)
	s.tokens[tok] = e
	s.mu.Unlock()

	if s.consume(tok, "max_hops", "5", 1) {
		t.Fatal("过期令牌应被拒")
	}
}

func TestUnknownTokenRejected(t *testing.T) {
	s := newTokenStore()
	if s.consume("deadbeef", "max_hops", "5", 1) {
		t.Fatal("不存在的令牌应被拒")
	}
}

func TestTokensAreUnique(t *testing.T) {
	s := newTokenStore()
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := s.issue("max_hops", "5", 1)
		if err != nil {
			t.Fatal(err)
		}
		if seen[tok] {
			t.Fatal("令牌重复——随机源有问题")
		}
		seen[tok] = true
	}
}
