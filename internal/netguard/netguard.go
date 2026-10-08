package netguard

import (
	"net"
	"net/netip"
)

// 浏览器与沙箱代理共用的出口判断：只放行公网全局单播地址。
// Go 的 IsPrivate 只覆盖 RFC 1918 与 fc00::/7，漏掉了运营商级 NAT（100.64.0.0/10，阿里云元数据 100.100.100.200 就在其中）、
// 基准测试网段等特殊用途地址，所以按 IANA 特殊用途地址表补全。见 docs/deep-questions.md 的 Q18。
var special = prefixes(
	"0.0.0.0/8",       // 本网络
	"10.0.0.0/8",      // 私有
	"100.64.0.0/10",   // 运营商级 NAT；也被 Tailscale、阿里云元数据使用
	"127.0.0.0/8",     // 回环
	"169.254.0.0/16",  // 链路本地；AWS、GCP 等云元数据 169.254.169.254
	"172.16.0.0/12",   // 私有
	"192.0.0.0/24",    // IETF 协议分配
	"192.0.2.0/24",    // 文档示例
	"192.88.99.0/24",  // 6to4 中继（已废弃）
	"192.168.0.0/16",  // 私有
	"198.18.0.0/15",   // 基准测试
	"198.51.100.0/24", // 文档示例
	"203.0.113.0/24",  // 文档示例
	"224.0.0.0/4",     // 组播
	"240.0.0.0/4",     // 保留，含广播 255.255.255.255
	"64:ff9b::/96",    // NAT64：地址里嵌着一个 IPv4，可能是内网
	"64:ff9b:1::/48",  // 本地 NAT64
	"100::/64",        // 丢弃
	"2001::/23",       // IETF 协议分配（含 Teredo）
	"2001:db8::/32",   // 文档示例
	"2002::/16",       // 6to4：地址里嵌着一个 IPv4
	"fc00::/7",        // 唯一本地
)

func prefixes(list ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(list))
	for i, s := range list {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// Internal 报告 ip 是否不该被访问：回环、链路本地、组播、未指定，以及上面任何一个特殊用途网段。
// IPv4 映射的 IPv6 地址（::ffff:a.b.c.d）先还原成 IPv4 再判断。
func Internal(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() {
		return true
	}
	for _, p := range special {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
