//go:build with_tailscale

package inbound

import (
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
	listeners       []*blockingListener
	packetConns     []*blockingPacketConn
	listenPktErrMsg string
}

func (s *fakeTailscaleServer) Listen(network, address string) (net.Listener, error) {
	s.listenCalls.Add(1)
	l := newBlockingListener(&net.TCPAddr{IP: net.ParseIP("100.64.0.1"), Port: 1080})
	s.listeners = append(s.listeners, l)
	return l, nil
}

func (s *fakeTailscaleServer) ListenPacket(network, address string) (net.PacketConn, error) {
	call := s.listenPktCalls.Add(1)
	if s.listenPktErrMsg != "" && call > 1 {
		return nil, errors.New(s.listenPktErrMsg)
	}
	pc := newBlockingPacketConn(&net.UDPAddr{IP: net.ParseIP("100.64.0.1"), Port: 1080})
	s.packetConns = append(s.packetConns, pc)
	return pc, nil
}

func (s *fakeTailscaleServer) TailscaleIPs() (netip.Addr, netip.Addr) {
	return s.ip4, s.ip6
}

func TestTailscaleListenOnServerRollsBackPartialStartup(t *testing.T) {
	t.Parallel()

	var instanceCloseCalls atomic.Int32
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
		closeInstance: func() error { instanceCloseCalls.Add(1); return nil },
		server:        server,
	}

	err := listener.listenOnServer(server, noopTunnel{})
	if err == nil {
		t.Fatal("listenOnServer() error = nil, want rollback error")
	}
	if !strings.Contains(err.Error(), "tailscale listen udp6") {
		t.Fatalf("listenOnServer() error = %q, want udp6 failure", err)
	}

	if got := instanceCloseCalls.Load(); got != 1 {
		t.Fatalf("closeInstance() call count = %d, want 1", got)
	}
	if len(server.listeners) != 1 {
		t.Fatalf("tcp listener count = %d, want 1", len(server.listeners))
	}
	if got := server.listeners[0].closeCount.Load(); got != 1 {
		t.Fatalf("tcp listener close count = %d, want 1", got)
	}
	if len(server.packetConns) != 1 {
		t.Fatalf("udp listener count = %d, want 1", len(server.packetConns))
	}
	if got := server.packetConns[0].closeCount.Load(); got != 1 {
		t.Fatalf("udp listener close count = %d, want 1", got)
	}
	if listener.server != nil {
		t.Fatal("expected listener.server to be cleared after rollback")
	}
	if listener.closeInstance != nil {
		t.Fatal("expected closeInstance to be cleared after rollback")
	}
	if len(listener.lTCP) != 0 {
		t.Fatalf("expected no tcp listeners to remain, got %d", len(listener.lTCP))
	}
	if len(listener.lUDP) != 0 {
		t.Fatalf("expected no udp listeners to remain, got %d", len(listener.lUDP))
	}
}

func TestTailscaleForwardAutoListensTCPAndUDP(t *testing.T) {
	t.Parallel()

	var instanceCloseCalls atomic.Int32
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
		closeInstance: func() error { instanceCloseCalls.Add(1); return nil },
		server:        server,
	}

	if err := listener.listenOnServer(server, noopTunnel{}); err != nil {
		t.Fatalf("listenOnServer() error = %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	if got := server.listenCalls.Load(); got != 1 {
		t.Fatalf("tcp listen call count = %d, want 1", got)
	}
	if got := server.listenPktCalls.Load(); got != 1 {
		t.Fatalf("udp listen call count = %d, want 1", got)
	}
	if len(listener.lTCP) != 1 {
		t.Fatalf("tcp listener count = %d, want 1", len(listener.lTCP))
	}
	if len(listener.lUDP) != 1 {
		t.Fatalf("udp listener count = %d, want 1", len(listener.lUDP))
	}
	if !strings.Contains(listener.Address(), "tcp://") || !strings.Contains(listener.Address(), "udp://") {
		t.Fatalf("Address() = %q, want both tcp:// and udp://", listener.Address())
	}
	if got := instanceCloseCalls.Load(); got != 0 {
		t.Fatalf("closeInstance() before Close = %d, want 0", got)
	}
}
