//go:build with_tailscale

package outbound

import (
	"net/netip"
	"testing"

	"github.com/metacubex/mihomo/constant"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

func TestSanitizeTailscaleHostname(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		hostname string
		fallback string
		want     string
	}{
		{
			name:     "fallback and lowercase",
			fallback: "TS Main",
			want:     "ts-main",
		},
		{
			name:     "trim invalid runes",
			hostname: "__Edge.Node__",
			fallback: "ignored",
			want:     "edge-node",
		},
		{
			name:     "truncate to dns label length",
			hostname: "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk",
			fallback: "ignored",
			want:     "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk",
		},
		{
			name:     "empty result uses default hostname",
			hostname: "___",
			fallback: "ignored",
			want:     "mihomo",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sanitizeTailscaleHostname(tt.hostname, tt.fallback); got != tt.want {
				t.Fatalf("sanitizeTailscaleHostname() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDescribeUnsupportedTailscaleOptions(t *testing.T) {
	t.Parallel()

	option := TailscaleOption{
		BasicOption: BasicOption{
			DialerProxy: "proxy-a",
			Interface:   "utun7",
			RoutingMark: 66,
			IPVersion:   constant.IPv4Only,
			TFO:         true,
			MPTCP:       true,
		},
	}

	got := describeUnsupportedTailscaleOptions(option)
	want := []string{"dialer-proxy", "interface-name", "routing-mark", "ip-version", "tfo", "mptcp"}
	if len(got) != len(want) {
		t.Fatalf("describeUnsupportedTailscaleOptions() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("describeUnsupportedTailscaleOptions()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMatchPeer(t *testing.T) {
	t.Parallel()

	peer := &ipnstate.PeerStatus{
		ID:             tailcfg.StableNodeID("n12345"),
		DNSName:        "exit-gateway.example.ts.net.",
		HostName:       "exit-gateway",
		TailscaleIPs:   []netip.Addr{netip.MustParseAddr("100.64.0.8")},
		Online:         true,
		ExitNodeOption: true,
	}

	cases := []string{
		"n12345",
		"exit-gateway.example.ts.net",
		"EXIT-GATEWAY",
		"100.64.0.8",
	}

	for _, raw := range cases {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if !matchPeer(peer, raw) {
				t.Fatalf("matchPeer(%q) = false, want true", raw)
			}
		})
	}
}
