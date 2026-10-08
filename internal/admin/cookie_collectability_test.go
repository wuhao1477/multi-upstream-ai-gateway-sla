package admin

import (
	"testing"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

func TestCookieCollectabilityDoesNotOverrideValidToken(t *testing.T) {
	ch := store.Channel{SiteFamily: "newapi"}
	for _, tc := range []struct {
		state        string
		token, ready bool
	}{
		{"ready", false, true}, {"unverified", false, true},
		{"expired", false, false}, {"needs_action", false, false}, {"expired", true, true},
	} {
		a := store.Account{Status: "active", CookieConfigured: true, CookieEnabled: true, CookieState: tc.state}
		if tc.token {
			a.CredType, a.CredStatus = "newapi_access_token", "valid"
		}
		got := accountCollect(ch, nil, a)
		if (got.Mode == CollectAuto) != tc.ready {
			t.Errorf("state=%s token=%v mode=%s", tc.state, tc.token, got.Mode)
		}
		channel := channelCollect(ch, nil, []store.Account{a})
		if (channel.Ready == 1) != tc.ready {
			t.Error("channel and account readiness differ")
		}
	}
}
