//go:build with_tailscale

package outbound

import (
	"context"
	"errors"
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

// TestTailscaleInitPropagatesContext verifies that the caller-supplied
// context is honored by both startInstance and the post-Up sync. The
// previous version of init() captured a context.Background() internally,
// which silently made tsnet.Up unbounded; this regression test fails if
// that behavior is reintroduced.
func TestTailscaleInitPropagatesContext(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	proxy := &Tailscale{
		option: TailscaleOption{Name: "ts-ctx"},
		startInstance: func(ctx context.Context) (*tsnet.Server, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
		syncExitNodePreferenceFunc: func(context.Context) error {
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- proxy.init(ctx)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("init did not enter startInstance promptly")
	}

	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("init returned nil after ctx cancel, expected error")
		}
		if proxy.initOk.Load() {
			t.Fatal("initOk should not be true after init failure")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("init did not return after ctx cancel; caller ctx is being ignored")
	}
}

func TestTailscaleInitLockWaitHonorsContext(t *testing.T) {
	t.Parallel()

	var startCalls atomic.Int32
	proxy := &Tailscale{
		option: TailscaleOption{Name: "ts-lock-ctx"},
		startInstance: func(context.Context) (*tsnet.Server, error) {
			startCalls.Add(1)
			return &tsnet.Server{}, nil
		},
		syncExitNodePreferenceFunc: func(context.Context) error {
			return nil
		},
	}

	proxy.initMutex.Lock()
	defer proxy.initMutex.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := proxy.init(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("init error = %v, want context deadline exceeded", err)
	}
	if got := startCalls.Load(); got != 0 {
		t.Fatalf("startInstance call count = %d, want 0", got)
	}
}

func TestWarmupAfterCloseDoesNotStart(t *testing.T) {
	t.Parallel()

	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	var startCalls atomic.Int32
	proxy := &Tailscale{
		option:          TailscaleOption{Name: "ts-closed"},
		lifecycleCtx:    lifecycleCtx,
		cancelLifecycle: cancelLifecycle,
		startInstance: func(context.Context) (*tsnet.Server, error) {
			startCalls.Add(1)
			return &tsnet.Server{}, nil
		},
		syncExitNodePreferenceFunc: func(context.Context) error {
			return nil
		},
	}

	if err := proxy.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	proxy.Warmup(context.Background())

	if got := startCalls.Load(); got != 0 {
		t.Fatalf("startInstance call count = %d, want 0", got)
	}
}

func TestCloseCancelsRunningWarmup(t *testing.T) {
	t.Parallel()

	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan struct{})
	var startCalls atomic.Int32
	proxy := &Tailscale{
		option:          TailscaleOption{Name: "ts-close-cancel"},
		lifecycleCtx:    lifecycleCtx,
		cancelLifecycle: cancelLifecycle,
		startInstance: func(ctx context.Context) (*tsnet.Server, error) {
			startCalls.Add(1)
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
		syncExitNodePreferenceFunc: func(context.Context) error {
			return nil
		},
	}

	go func() {
		proxy.Warmup(context.Background())
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Warmup did not enter startInstance")
	}

	if err := proxy.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Warmup did not return after Close")
	}
	if got := startCalls.Load(); got != 1 {
		t.Fatalf("startInstance call count = %d, want 1", got)
	}
	if proxy.initOk.Load() {
		t.Fatal("initOk should not be true after Close cancels Warmup")
	}
}

// TestWarmupSkipsWhenInitInProgress verifies that the TryLock fast-path
// in Warmup keeps follow-up ApplyConfig goroutines from accumulating
// behind an already-running init. The first Warmup holds initMutex
// (simulated by blocking inside startInstance); subsequent Warmups must
// return immediately rather than queueing on the mutex.
func TestWarmupSkipsWhenInitInProgress(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	proceed := make(chan struct{})
	var startCalls atomic.Int32
	proxy := &Tailscale{
		option: TailscaleOption{Name: "ts-pile"},
		startInstance: func(ctx context.Context) (*tsnet.Server, error) {
			startCalls.Add(1)
			close(started)
			<-proceed
			return &tsnet.Server{}, nil
		},
		syncExitNodePreferenceFunc: func(context.Context) error {
			return nil
		},
	}

	go proxy.Warmup(context.Background())

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first Warmup did not enter startInstance")
	}

	const piledUp = 8
	done := make(chan struct{}, piledUp)
	deadline := time.After(500 * time.Millisecond)
	for range piledUp {
		go func() {
			proxy.Warmup(context.Background())
			done <- struct{}{}
		}()
	}
	for i := 0; i < piledUp; i++ {
		select {
		case <-done:
		case <-deadline:
			t.Fatalf("Warmup %d/%d did not return promptly; goroutines are piling up on initMutex", i+1, piledUp)
		}
	}

	close(proceed)

	deadline = time.After(2 * time.Second)
	for !proxy.initOk.Load() {
		select {
		case <-deadline:
			t.Fatal("first Warmup never finished")
		case <-time.After(10 * time.Millisecond):
		}
	}

	if got := startCalls.Load(); got != 1 {
		t.Fatalf("startInstance call count = %d, want 1", got)
	}
}
