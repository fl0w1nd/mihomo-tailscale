package outbound

import (
	"fmt"
	"net/netip"
)

func pickTailscaleUDPBind(dst, ip4, ip6 netip.Addr) (string, error) {
	if !dst.IsValid() {
		return "", fmt.Errorf("tailscale udp requires resolved destination ip")
	}

	if dst.Is4() {
		if ip4.IsValid() {
			return netip.AddrPortFrom(ip4, 0).String(), nil
		}
		return "", fmt.Errorf("tailscale node has no ipv4 address for destination %s", dst)
	}

	if ip6.IsValid() {
		return netip.AddrPortFrom(ip6, 0).String(), nil
	}

	return "", fmt.Errorf("tailscale node has no ipv6 address for destination %s", dst)
}
