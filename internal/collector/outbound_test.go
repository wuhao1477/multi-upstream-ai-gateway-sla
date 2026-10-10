package collector

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

// 出站策略本身是被测对象：输入是"某主机解析到某些地址"。真 DNS 不会按需给出私网、
// 混合或元数据地址，所以解析结果由用例注入（CLAUDE.md §1 的行为类例外）。
// 能连通的用例只连本机临时监听端口，不向任何真实内网或公网地址发包。

func TestIsPublicIP(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"1.1.1.1", true}, {"2606:4700::1111", true},
		{"10.0.0.5", false}, {"127.0.0.1", false}, {"169.254.1.1", false}, {"100.64.0.1", false},
		{"0.1.2.3", false}, {"192.0.0.8", false}, {"198.18.0.1", false}, {"240.0.0.1", false},
		{"::ffff:10.0.0.5", false}, {"::a00:5", false}, {"fc00::1", false}, {"fe80::1", false},
		{"64:ff9b::a00:5", false}, {"64:ff9b::101:101", true},
		{"2002:a00:5::1", false}, {"2002:101:101::1", true},
	} {
		if got := IsPublicIP(netip.MustParseAddr(tc.ip)); got != tc.want {
			t.Errorf("IsPublicIP(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

func TestOutboundDialChecksEveryResolvedAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	_, port, _ := net.SplitHostPort(listener.Addr().String())

	for _, tc := range []struct {
		name, config, host string
		answers            []string
		allowed            bool
	}{
		{"public_only_rejects_loopback", ``, "upstream.test", []string{"127.0.0.1"}, false},
		{"authorized_host_and_port", `{"UPSTREAM.test:` + port + `": ["127.0.0.1/32"]}`, "upstream.test", []string{"127.0.0.1"}, true},
		{"authorized_host_any_port", `{"upstream.test": ["127.0.0.0/8"]}`, "upstream.test", []string{"127.0.0.1"}, true},
		{"authorized_ip_literal", `{"127.0.0.1": ["127.0.0.1/32"]}`, "127.0.0.1", []string{"127.0.0.1"}, true},
		{"other_port", `{"upstream.test:1": ["127.0.0.1/32"]}`, "upstream.test", []string{"127.0.0.1"}, false},
		// 上游把自己的域名解析到已授权的内网地址：授权跟着主机名走，借不到。
		{"other_host_same_address", `{"nas.test": ["127.0.0.1/32"]}`, "upstream.test", []string{"127.0.0.1"}, false},
		{"outside_authorized_prefix", `{"upstream.test": ["127.0.0.2/32"]}`, "upstream.test", []string{"127.0.0.1"}, false},
		{"mixed_public_and_private", ``, "upstream.test", []string{"1.1.1.1", "10.0.0.1"}, false},
		{"authorized_plus_unlisted_private", `{"upstream.test": ["127.0.0.1/32"]}`, "upstream.test", []string{"127.0.0.1", "10.0.0.1"}, false},
		{"metadata_cannot_be_authorized", `{"upstream.test": ["169.254.0.0/16"]}`, "upstream.test", []string{"169.254.169.254"}, false},
		{"aws_ipv6_metadata_cannot_be_authorized", `{"upstream.test": ["fd00::/8"]}`, "upstream.test", []string{"fd00:ec2::254"}, false},
		{"nat64_metadata_cannot_be_authorized", `{"upstream.test": ["64:ff9b::/96"]}`, "upstream.test", []string{"64:ff9b::a9fe:a9fe"}, false},
		{"unspecified_cannot_be_authorized", `{"upstream.test": ["0.0.0.0/8"]}`, "upstream.test", []string{"0.0.0.0"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			private, err := parseOutboundTargets(tc.config)
			if err != nil {
				t.Fatal(err)
			}
			policy := newOutboundPolicy(private)
			var lookedUp string
			policy.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
				lookedUp = host
				addrs := make([]netip.Addr, len(tc.answers))
				for i, answer := range tc.answers {
					addrs[i] = netip.MustParseAddr(answer)
				}
				return addrs, nil
			}
			// upstream.test 在真 DNS 里不存在：连得上即证明拨的是校验过的 IP，而不是再解析一次。
			conn, err := policy.DialContext(context.Background(), "tcp", net.JoinHostPort(tc.host, port))
			if conn != nil {
				_ = conn.Close()
			}
			if tc.allowed && err != nil {
				t.Fatalf("authorized target rejected: %v", err)
			}
			if !tc.allowed && !errors.Is(err, ErrOutboundBlocked) {
				t.Fatalf("unauthorized target not blocked: conn=%v err=%v", conn != nil, err)
			}
			if lookedUp != strings.ToLower(tc.host) {
				t.Fatalf("resolved %q, want %q", lookedUp, tc.host)
			}
		})
	}
}

func TestParseOutboundTargets(t *testing.T) {
	got, err := parseOutboundTargets(`{" Nas.LAN:05005 ": ["192.168.1.10/24"], "::1": ["::1/128"],
		"[::ffff:127.0.0.1]:8080": ["127.0.0.1/32", "127.0.0.2/32"]}`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"nas.lan:5005":   "[192.168.1.0/24]",
		"::1":            "[::1/128]",
		"127.0.0.1:8080": "[127.0.0.1/32 127.0.0.2/32]",
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %v", got)
	}
	for key, prefixes := range want {
		if fmt.Sprint(got[key]) != prefixes {
			t.Errorf("%s = %v, want %s", key, got[key], prefixes)
		}
	}
	if empty, err := parseOutboundTargets("  "); err != nil || len(empty) != 0 {
		t.Fatalf("empty config = %v, %v", empty, err)
	}

	for _, raw := range []string{
		`not json`, `[]`, `{"nas.lan": []}`, `{"nas.lan": ["0.0.0.0/0"]}`, `{"nas.lan": ["::/0"]}`,
		`{"nas.lan": ["10.0.0.1"]}`, `{"nas.lan:0": ["10.0.0.0/8"]}`, `{"nas.lan:70000": ["10.0.0.0/8"]}`,
		`{"[::1]": ["::1/128"]}`, `{"": ["10.0.0.0/8"]}`, `{"nas.lan/x": ["10.0.0.0/8"]}`,
	} {
		if _, err := parseOutboundTargets(raw); err == nil {
			t.Errorf("accepted invalid config %s", raw)
		}
	}
}

func TestOutboundTransportBlocksBeforeSendingRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()

	policy := newOutboundPolicy(nil)
	if policy.transport.Proxy != nil {
		t.Fatal("outbound transport must not use environment proxies")
	}
	client := &Client{HC: &http.Client{Transport: policy.transport, CheckRedirect: NoRedirect}}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/status?token=test-secret-marker", nil)
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrOutboundBlocked) || hits.Load() != 0 {
		t.Fatalf("loopback request not blocked before sending: err=%v hits=%d", err, hits.Load())
	}
	if text := err.Error(); strings.Contains(text, "127.0.0.1") || strings.Contains(text, "test-secret-marker") {
		t.Fatalf("blocked error exposed the target: %q", text)
	}

	// 本包 TestMain 授权了回环；Cookie 用的 DialPublic 仍然只认公网。
	if _, err := DialPublic(context.Background(), "tcp", srv.Listener.Addr().String()); !errors.Is(err, ErrOutboundBlocked) {
		t.Fatalf("DialPublic inherited the private authorization: %v", err)
	}
	if OutboundHTTPClient(0).Transport != outbound.Load().transport {
		t.Fatal("default clients do not use the configured outbound policy")
	}
}
