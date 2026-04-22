//go:build with_tailscale

package outbound

import (
	"context"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ts "github.com/metacubex/mihomo/component/tailscale"
	"github.com/metacubex/mihomo/constant"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
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
			if got := ts.SanitizeHostname(tt.hostname, tt.fallback); got != tt.want {
				t.Fatalf("SanitizeHostname() = %q, want %q", got, tt.want)
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

func TestTailscaleInitRunsOnceUnderConcurrency(t *testing.T) {
	t.Parallel()

	var startCalls atomic.Int32
	var syncCalls atomic.Int32

	proxy := &Tailscale{
		option: TailscaleOption{Name: "ts-concurrent"},
		startInstance: func(context.Context) (*tsnet.Server, error) {
			startCalls.Add(1)
			time.Sleep(20 * time.Millisecond)
			return &tsnet.Server{}, nil
		},
		syncExitNodePreferenceFunc: func(context.Context) error {
			syncCalls.Add(1)
			time.Sleep(20 * time.Millisecond)
			return nil
		},
	}

	const concurrency = 32
	var wg sync.WaitGroup
	errCh := make(chan error, concurrency)
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- proxy.init(context.Background())
		}()
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatalf("init() returned error: %v", err)
		}
	}

	if got := startCalls.Load(); got != 1 {
		t.Fatalf("startInstance() call count = %d, want 1", got)
	}
	if got := syncCalls.Load(); got != 1 {
		t.Fatalf("syncExitNodePreference() call count = %d, want 1", got)
	}
	if !proxy.initOk.Load() {
		t.Fatal("expected initOk to be true after successful init")
	}
}
