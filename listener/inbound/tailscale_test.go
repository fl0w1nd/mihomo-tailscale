//go:build with_tailscale

package inbound

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
)

type noopTunnel struct{}

func (noopTunnel) HandleTCPConn(net.Conn, *C.Metadata) {}

func (noopTunnel) HandleUDPPacket(C.UDPPacket, *C.Metadata) {}

func (noopTunnel) NatTable() C.NatTable { return nil }

type blockingListener struct {
	addr       net.Addr
	closeCh    chan struct{}
	closeOnce  sync.Once
	closeCount atomic.Int32
}

func newBlockingListener(addr net.Addr) *blockingListener {
	return &blockingListener{
		addr:    addr,
		closeCh: make(chan struct{}),
	}
}

func (l *blockingListener) Accept() (net.Conn, error) {
	<-l.closeCh
	return nil, net.ErrClosed
}

func (l *blockingListener) Close() error {
	l.closeCount.Add(1)
	l.closeOnce.Do(func() {
		close(l.closeCh)
	})
	return nil
}

func (l *blockingListener) Addr() net.Addr {
	return l.addr
}

type blockingPacketConn struct {
	addr       net.Addr
	closeCh    chan struct{}
	closeOnce  sync.Once
	closeCount atomic.Int32
}

func newBlockingPacketConn(addr net.Addr) *blockingPacketConn {
	return &blockingPacketConn{
		addr:    addr,
		closeCh: make(chan struct{}),
	}
}

func (c *blockingPacketConn) ReadFrom([]byte) (int, net.Addr, error) {
	<-c.closeCh
	return 0, nil, net.ErrClosed
}

func (c *blockingPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	return len(p), nil
}

func (c *blockingPacketConn) Close() error {
	c.closeCount.Add(1)
	c.closeOnce.Do(func() {
		close(c.closeCh)
	})
	return nil
}

func (c *blockingPacketConn) LocalAddr() net.Addr {
	return c.addr
}

func (c *blockingPacketConn) SetDeadline(time.Time) error {
	return nil
}

func (c *blockingPacketConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *blockingPacketConn) SetWriteDeadline(time.Time) error {
	return nil
}

type fakeTailscaleServer struct {
	ip4 netip.Addr
	ip6 netip.Addr

	listenCalls     atomic.Int32
	listenPktCalls  atomic.Int32
	mu              sync.Mutex
	listeners       []*blockingListener
	packetConns     []*blockingPacketConn
	listenPktErrMsg string
}

func (s *fakeTailscaleServer) Listen(network, address string) (net.Listener, error) {
	s.listenCalls.Add(1)
	l := newBlockingListener(&net.TCPAddr{IP: net.ParseIP("100.64.0.1"), Port: 1080})
	s.mu.Lock()
	s.listeners = append(s.listeners, l)
	s.mu.Unlock()
	return l, nil
}

func (s *fakeTailscaleServer) ListenPacket(network, address string) (net.PacketConn, error) {
	call := s.listenPktCalls.Add(1)
	if s.listenPktErrMsg != "" && call > 1 {
		return nil, errors.New(s.listenPktErrMsg)
	}
	pc := newBlockingPacketConn(&net.UDPAddr{IP: net.ParseIP("100.64.0.1"), Port: 1080})
	s.mu.Lock()
	s.packetConns = append(s.packetConns, pc)
	s.mu.Unlock()
	return pc, nil
}

func (s *fakeTailscaleServer) TailscaleIPs() (netip.Addr, netip.Addr) {
	return s.ip4, s.ip6
}

func (s *fakeTailscaleServer) snapshotListeners() []*blockingListener {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*blockingListener, len(s.listeners))
	copy(out, s.listeners)
	return out
}

func (s *fakeTailscaleServer) snapshotPacketConns() []*blockingPacketConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*blockingPacketConn, len(s.packetConns))
	copy(out, s.packetConns)
	return out
}

// TestSetupForwardsRollsBackPartialStartup verifies the rollback path:
// when a forward fails mid-startup, every listener created during that
// attempt is closed and nothing is stashed on the receiver. Mirrors the
// invariant that bring-up retries must always start from a clean slate.
func TestSetupForwardsRollsBackPartialStartup(t *testing.T) {
	t.Parallel()

	server := &fakeTailscaleServer{
		ip4:             netip.MustParseAddr("100.64.0.1"),
		ip6:             netip.MustParseAddr("fd7a:115c:a1e0::1"),
		listenPktErrMsg: "udp6 failed",
	}
	listener := &TailscaleListener{
		config: &TailscaleOption{
			NameStr: "ts-inbound",
			Forwards: []TailscaleForward{{
				Listen: 1080,
				Target: "127.0.0.1:22",
			}},
		},
	}

	tcp, udp, err := listener.setupForwards(server, noopTunnel{})
	if err == nil {
		t.Fatal("setupForwards() error = nil, want rollback error")
	}
	if !strings.Contains(err.Error(), "tailscale listen udp6") {
		t.Fatalf("setupForwards() error = %q, want udp6 failure", err)
	}
	if tcp != nil {
		t.Fatalf("expected nil tcp slice on failure, got %d entries", len(tcp))
	}
	if udp != nil {
		t.Fatalf("expected nil udp slice on failure, got %d entries", len(udp))
	}

	listeners := server.snapshotListeners()
	if len(listeners) != 1 {
		t.Fatalf("tcp listener count = %d, want 1", len(listeners))
	}
	if got := listeners[0].closeCount.Load(); got != 1 {
		t.Fatalf("tcp listener close count = %d, want 1", got)
	}
	packetConns := server.snapshotPacketConns()
	if len(packetConns) != 1 {
		t.Fatalf("udp listener count = %d, want 1", len(packetConns))
	}
	if got := packetConns[0].closeCount.Load(); got != 1 {
		t.Fatalf("udp listener close count = %d, want 1", got)
	}
}

// TestSetupForwardsAutoListensTCPAndUDP ensures network: auto creates
// both a TCP listener and a UDP packet conn on the IPv4-only fake.
func TestSetupForwardsAutoListensTCPAndUDP(t *testing.T) {
	t.Parallel()

	server := &fakeTailscaleServer{
		ip4: netip.MustParseAddr("100.64.0.1"),
	}
	listener := &TailscaleListener{
		config: &TailscaleOption{
			NameStr: "ts-inbound",
			Forwards: []TailscaleForward{{
				Listen: 53,
				Target: "127.0.0.1:5353",
			}},
		},
	}

	tcp, udp, err := listener.setupForwards(server, noopTunnel{})
	if err != nil {
		t.Fatalf("setupForwards() error = %v", err)
	}
	defer func() {
		for _, l := range tcp {
			_ = l.Close()
		}
		for _, l := range udp {
			_ = l.Close()
		}
	}()

	if got := server.listenCalls.Load(); got != 1 {
		t.Fatalf("tcp listen call count = %d, want 1", got)
	}
	if got := server.listenPktCalls.Load(); got != 1 {
		t.Fatalf("udp listen call count = %d, want 1", got)
	}
	if len(tcp) != 1 {
		t.Fatalf("tcp listener count = %d, want 1", len(tcp))
	}
	if len(udp) != 1 {
		t.Fatalf("udp listener count = %d, want 1", len(udp))
	}
}

// shrinkBackoff installs aggressive backoff/timeout values for tests and
// returns a restore function. Use t.Cleanup with it.
func shrinkBackoff(t *testing.T) {
	t.Helper()
	prevInitial := initialBackoff
	prevMax := maxBackoff
	prevAttempt := startAttemptTimeout
	prevGrace := closeShutdownGrace
	initialBackoff = 10 * time.Millisecond
	maxBackoff = 40 * time.Millisecond
	startAttemptTimeout = 500 * time.Millisecond
	closeShutdownGrace = 500 * time.Millisecond
	t.Cleanup(func() {
		initialBackoff = prevInitial
		maxBackoff = prevMax
		startAttemptTimeout = prevAttempt
		closeShutdownGrace = prevGrace
	})
}

func newTestListener(t *testing.T, startFn startInstanceFunc) *TailscaleListener {
	t.Helper()
	listener := &TailscaleListener{
		config: &TailscaleOption{
			NameStr: "ts-async",
			Forwards: []TailscaleForward{{
				Listen: 1080,
				Target: "127.0.0.1:22",
			}},
		},
		startInstanceFn: startFn,
	}
	return listener
}

// listenAsync runs Listen on a goroutine and returns the elapsed wall
// time. We don't call ts.Acquire in these tests, which means real
// instance acquisition is exercised; that's intentional because the
// startInstanceFn injection bypasses instance.Start so the acquired
// shared instance never authenticates. We immediately Close the
// listener at the end to release it.
func listenAsync(t *testing.T, listener *TailscaleListener) time.Duration {
	t.Helper()
	start := time.Now()
	if err := listener.Listen(noopTunnel{}); err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	return time.Since(start)
}

// TestListenReturnsImmediatelyOnSlowStart verifies the core invariant:
// Listen() must not block on tsnet bring-up. We inject a startFn that
// blocks on ctx.Done() and assert Listen() returns within a fraction of
// the per-attempt timeout.
func TestListenReturnsImmediatelyOnSlowStart(t *testing.T) {
	t.Parallel()
	shrinkBackoff(t)

	started := make(chan struct{})
	listener := newTestListener(t, func(ctx context.Context) (tailscaleListenServer, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})

	elapsed := listenAsync(t, listener)
	t.Cleanup(func() { _ = listener.Close() })

	if elapsed > 100*time.Millisecond {
		t.Fatalf("Listen() took %s, must return quickly even with hung bring-up", elapsed)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("bring-up goroutine did not invoke startFn")
	}
}

// TestListenRetriesOnStartFailure verifies the exponential-backoff
// retry path: a startFn that fails twice before succeeding eventually
// produces a Ready listener with the configured forwards.
func TestListenRetriesOnStartFailure(t *testing.T) {
	t.Parallel()
	shrinkBackoff(t)

	var calls atomic.Int32
	successServer := &fakeTailscaleServer{
		ip4: netip.MustParseAddr("100.64.0.1"),
	}
	listener := newTestListener(t, func(ctx context.Context) (tailscaleListenServer, error) {
		n := calls.Add(1)
		if n < 3 {
			return nil, errors.New("simulated start failure")
		}
		return successServer, nil
	})

	if err := listener.Listen(noopTunnel{}); err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	deadline := time.After(2 * time.Second)
	for !listener.ready.Load() {
		select {
		case <-deadline:
			t.Fatalf("listener never reached ready state; calls=%d", calls.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}

	if got := calls.Load(); got != 3 {
		t.Fatalf("startFn call count = %d, want 3", got)
	}
	addr := listener.Address()
	if !strings.Contains(addr, "tcp://") || !strings.Contains(addr, "udp://") {
		t.Fatalf("Address() = %q, want both tcp:// and udp:// after success", addr)
	}
}

// TestListenCloseInterruptsRetryLoop verifies that Close() promptly
// breaks out of the retry loop even when every startFn invocation
// fails. Without lifecycle cancellation we would either leak the
// goroutine or pay the closeShutdownGrace fallback every time.
func TestListenCloseInterruptsRetryLoop(t *testing.T) {
	t.Parallel()
	shrinkBackoff(t)

	releaseObserved := atomic.Int32{}
	var calls atomic.Int32
	listener := newTestListener(t, func(ctx context.Context) (tailscaleListenServer, error) {
		calls.Add(1)
		return nil, errors.New("always fails")
	})
	// Wrap closeInstance after Listen so we can observe the release call.
	if err := listener.Listen(noopTunnel{}); err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	listener.mu.Lock()
	originalClose := listener.closeInstance
	listener.closeInstance = func() error {
		releaseObserved.Add(1)
		return originalClose()
	}
	listener.mu.Unlock()

	// Let the retry loop spin for at least a couple of attempts.
	time.Sleep(50 * time.Millisecond)
	closeStart := time.Now()
	if err := listener.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if elapsed := time.Since(closeStart); elapsed > closeShutdownGrace {
		t.Fatalf("Close() blocked %s, expected fast lifecycle cancel", elapsed)
	}
	if got := releaseObserved.Load(); got != 1 {
		t.Fatalf("release callback called %d times, want 1", got)
	}
	if calls.Load() < 1 {
		t.Fatal("expected at least one startFn attempt before Close")
	}

	// A second Close must be a no-op.
	if err := listener.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if got := releaseObserved.Load(); got != 1 {
		t.Fatalf("release callback called %d times after second Close, want 1", got)
	}
}

// TestListenCloseBeforeReady verifies that closing while the bring-up
// goroutine is still inside its very first startFn call still works:
// no listeners exist yet, but the release callback must fire exactly
// once and the goroutine must exit.
func TestListenCloseBeforeReady(t *testing.T) {
	t.Parallel()
	shrinkBackoff(t)

	releaseObserved := atomic.Int32{}
	blockUntilCtxDone := make(chan struct{})
	listener := newTestListener(t, func(ctx context.Context) (tailscaleListenServer, error) {
		<-ctx.Done()
		select {
		case <-blockUntilCtxDone:
		default:
			close(blockUntilCtxDone)
		}
		return nil, ctx.Err()
	})

	if err := listener.Listen(noopTunnel{}); err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	listener.mu.Lock()
	originalClose := listener.closeInstance
	listener.closeInstance = func() error {
		releaseObserved.Add(1)
		return originalClose()
	}
	listener.mu.Unlock()

	time.Sleep(20 * time.Millisecond)
	if err := listener.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	select {
	case <-blockUntilCtxDone:
	case <-time.After(time.Second):
		t.Fatal("startFn context was not cancelled by Close")
	}
	if got := releaseObserved.Load(); got != 1 {
		t.Fatalf("release callback called %d times, want 1", got)
	}
	if listener.ready.Load() {
		t.Fatal("listener should not be ready after Close before bring-up succeeded")
	}
	if addr := listener.Address(); addr != "" {
		t.Fatalf("Address() = %q, want empty before ready", addr)
	}
}

// TestNormalizeAcceptRoutesDefaultTrue locks in the inbound default so a
// later refactor cannot silently change it to nil and re-introduce the
// outbound/inbound first-wins conflict on shared identities.
func TestNormalizeAcceptRoutesDefaultTrue(t *testing.T) {
	t.Parallel()

	opt := normalizeTailscaleOption(&TailscaleOption{NameStr: "t"})
	if opt.AcceptRoutes == nil {
		t.Fatal("AcceptRoutes should default to non-nil")
	}
	if !*opt.AcceptRoutes {
		t.Fatal("AcceptRoutes default should be true to match outbound")
	}

	v := false
	opt = normalizeTailscaleOption(&TailscaleOption{NameStr: "t", AcceptRoutes: &v})
	if opt.AcceptRoutes == nil || *opt.AcceptRoutes {
		t.Fatal("explicit AcceptRoutes=false should be preserved")
	}
}
