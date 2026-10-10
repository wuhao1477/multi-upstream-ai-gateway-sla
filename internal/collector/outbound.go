package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// 出站策略（#33）：令牌采集、站型探测与 WebDAV 默认只连公网地址；内网目标须由
// 部署环境在 SLA_OUTBOUND_PRIVATE_TARGETS 里按"主机 → 网段"显式授权。
//
// 校验发生在**每次建连**：解析出全部地址、逐个判定，任何一个不合规就整体拒绝，
// 然后直接拨已判定的 IP。只校验 URL 不够 —— 管理员填的域名可以在下一次解析时
// 指向内网（DNS 重绑定），而 Go 的默认拨号器会再解析一次。

// ErrOutboundBlocked 是目标既非公网、也未获授权时的错误。文案固定，不带地址。
var ErrOutboundBlocked = errors.New("出站目标不是公网地址，且未在 SLA_OUTBOUND_PRIVATE_TARGETS 中授权")

// OutboundPolicy 持有一份内网授权表及按它拨号的 Transport（共用连接池）。
type OutboundPolicy struct {
	private   map[string][]netip.Prefix // "host:port" 或 "host"（任意端口）→ 允许的网段
	lookup    func(context.Context, string) ([]netip.Addr, error)
	transport *http.Transport
}

func newOutboundPolicy(private map[string][]netip.Prefix) *OutboundPolicy {
	p := &OutboundPolicy{private: private, lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}}
	t := http.DefaultTransport.(*http.Transport).Clone()
	// 不走环境代理：代理会替我们重新解析 DNS，连接时校验就形同虚设。
	t.Proxy = nil
	t.DialContext = p.DialContext
	p.transport = t
	return p
}

var (
	publicOutbound = newOutboundPolicy(nil)
	outbound       atomic.Pointer[OutboundPolicy]
)

// ConfigureOutbound 在进程启动、构造任何客户端之前调用一次。空值即只允许公网。
//
// 格式：{"nas.lan:5005": ["192.168.1.10/32"], "127.0.0.1": ["127.0.0.1/32"]}。
// 省略端口表示该主机的任意端口。只认 JSON 键里写明的主机名：别的域名解析到
// 同一内网地址照样拒绝，所以上游改 DNS 也借不到这份授权。
func ConfigureOutbound(raw string) error {
	private, err := parseOutboundTargets(raw)
	if err != nil {
		return err
	}
	outbound.Store(newOutboundPolicy(private))
	return nil
}

func parseOutboundTargets(raw string) (map[string][]netip.Prefix, error) {
	private := map[string][]netip.Prefix{}
	if strings.TrimSpace(raw) == "" {
		return private, nil
	}
	var entries map[string][]string
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, errors.New(`SLA_OUTBOUND_PRIVATE_TARGETS 必须是 JSON 对象，例如 {"nas.lan:5005": ["192.168.1.10/32"]}`)
	}
	for target, cidrs := range entries {
		key, ok := outboundKey(target)
		if !ok || len(cidrs) == 0 {
			return nil, fmt.Errorf("SLA_OUTBOUND_PRIVATE_TARGETS 的目标 %q 无效：须为 host 或 host:port，且至少给一个网段", target)
		}
		for _, cidr := range cidrs {
			prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
			// /0 等于关掉整个策略，不接受。
			if err != nil || prefix.Bits() == 0 {
				return nil, fmt.Errorf("SLA_OUTBOUND_PRIVATE_TARGETS 中 %q 的网段 %q 无效", target, cidr)
			}
			private[key] = append(private[key], prefix.Masked())
		}
	}
	return private, nil
}

// outboundKey 规范化授权目标，与拨号时 normalizeHost + 端口拼出的键一致。
func outboundKey(target string) (string, bool) {
	target = strings.TrimSpace(target)
	if host, port, err := net.SplitHostPort(target); err == nil {
		n, err := strconv.Atoi(port)
		if host == "" || err != nil || n < 1 || n > 65535 {
			return "", false
		}
		return net.JoinHostPort(normalizeHost(host), strconv.Itoa(n)), true
	}
	if target == "" || strings.ContainsAny(target, "[]/") {
		return "", false
	}
	return normalizeHost(target), true
}

func normalizeHost(host string) string {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().String()
	}
	return strings.ToLower(host)
}

// OutboundHTTPClient 是所有默认上游客户端的唯一构造入口：不跟随重定向，
// 连接时执行 ConfigureOutbound 配置的策略（未配置时只允许公网）。
func OutboundHTTPClient(timeout time.Duration) *http.Client {
	p := outbound.Load()
	if p == nil {
		p = publicOutbound
	}
	return &http.Client{Timeout: timeout, CheckRedirect: NoRedirect, Transport: p.transport}
}

// DialPublic 只允许公网目标，不受 ConfigureOutbound 授权影响（Cookie 读取用）。
func DialPublic(ctx context.Context, network, address string) (net.Conn, error) {
	return publicOutbound.DialContext(ctx, network, address)
}

// DialContext 解析并校验全部地址后，直接拨已校验的 IP。
func (p *OutboundPolicy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrOutboundBlocked
	}
	host = normalizeHost(host)
	addrs, err := p.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	allowed := p.private[net.JoinHostPort(host, port)]
	anyPort := p.private[host]
	for _, ip := range addrs {
		if !outboundIPAllowed(ip, allowed) && !outboundIPAllowed(ip, anyPort) {
			return nil, ErrOutboundBlocked
		}
	}
	err = &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	dialer := net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	for _, ip := range addrs {
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if dialErr == nil {
			return conn, nil
		}
		err = dialErr
	}
	return nil, err
}

func outboundIPAllowed(ip netip.Addr, allowed []netip.Prefix) bool {
	ip = ip.Unmap()
	if IsPublicIP(ip) {
		return true
	}
	if neverAllowed(ip) {
		return false
	}
	for _, prefix := range allowed {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// neverAllowed 是授权也打不开的地址：链路本地（含 169.254.169.254 云元数据）、
// 未指定、组播、保留段。嵌在 NAT64/6to4 里的 IPv4 按嵌入的那个判。
func neverAllowed(ip netip.Addr) bool {
	if v4, ok := embeddedIPv4(ip); ok && neverAllowed(v4) {
		return true
	}
	if !ip.IsValid() || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	for _, prefix := range neverAllowedPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

var neverAllowedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),         // 本网络；部分系统把 0.x 当本机
	netip.MustParsePrefix("240.0.0.0/4"),       // 保留（含广播）
	netip.MustParsePrefix("fd00:ec2::254/128"), // AWS IPv6 元数据，落在 ULA 段里
}

// IsPublicIP 同时供 URL 字面量与连接时的 DNS 结果校验使用。
func IsPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	// NAT64 / 6to4 把 IPv4 嵌在 IPv6 里：按嵌入的那个 IPv4 判。不拆的话 64:ff9b::a00:5
	// 在 NAT64 网络里就是 10.0.0.5；整段拒绝又会让纯 IPv6 + DNS64 主机连不上任何公网站。
	if v4, ok := embeddedIPv4(ip); ok {
		return IsPublicIP(v4)
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.Zone() != "" {
		return false
	}
	for _, p := range nonPublicPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// nonPublicPrefixes 是 IsGlobalUnicast 放行、但不是公网站点的段。
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),     // 本网络
	netip.MustParsePrefix("100.64.0.0/10"), // 运营商级 NAT（Tailscale 等也在这里）
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF 协议分配
	netip.MustParsePrefix("198.18.0.0/15"), // 基准测试网
	netip.MustParsePrefix("240.0.0.0/4"),   // 保留（含广播）
	netip.MustParsePrefix("::/96"),         // 已废弃的 IPv4 兼容地址 ::a.b.c.d
}

var (
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour   = netip.MustParsePrefix("2002::/16")
)

// embeddedIPv4 取出 NAT64（末 32 位）与 6to4（第 2~5 字节）里嵌的 IPv4。
func embeddedIPv4(ip netip.Addr) (netip.Addr, bool) {
	if !ip.Is6() {
		return netip.Addr{}, false
	}
	b := ip.As16()
	switch {
	case nat64Prefix.Contains(ip):
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case sixToFour.Contains(ip):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
}
