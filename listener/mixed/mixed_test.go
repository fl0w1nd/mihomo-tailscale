package mixed

import (
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	adapterInbound "github.com/metacubex/mihomo/adapter/inbound"
	componentAuth "github.com/metacubex/mihomo/component/auth"
	C "github.com/metacubex/mihomo/constant"
	authStore "github.com/metacubex/mihomo/listener/auth"
	LC "github.com/metacubex/mihomo/listener/config"
)

type testTunnel struct {
	tcpCh chan *C.Metadata
}

func (t *testTunnel) HandleTCPConn(conn net.Conn, metadata *C.Metadata) {
	select {
	case t.tcpCh <- metadata:
	default:
	}
	_ = conn.Close()
}

func (*testTunnel) HandleUDPPacket(C.UDPPacket, *C.Metadata) {}

func (*testTunnel) NatTable() C.NatTable { return nil }

type channelListener struct {
	addr net.Addr
	ch   chan net.Conn
	once sync.Once
}

func newChannelListener(addr net.Addr) *channelListener {
	return &channelListener{
		addr: addr,
		ch:   make(chan net.Conn, 1),
	}
}

func (l *channelListener) Accept() (net.Conn, error) {
	conn, ok := <-l.ch
	if !ok {
		return nil, net.ErrClosed
	}
	return conn, nil
}

func (l *channelListener) Close() error {
	l.once.Do(func() {
		close(l.ch)
	})
	return nil
}

func (l *channelListener) Addr() net.Addr {
	return l.addr
}

type observedConn struct {
	net.Conn
	local     net.Addr
	remote    net.Addr
	closeCh   chan struct{}
	closeOnce sync.Once
}

func (c *observedConn) LocalAddr() net.Addr {
	return c.local
}

func (c *observedConn) RemoteAddr() net.Addr {
	return c.remote
}

func (c *observedConn) Close() error {
	err := c.Conn.Close()
	c.closeOnce.Do(func() {
		close(c.closeCh)
	})
	return err
}

func TestNewWithListenerRejectsDisallowedRemoteAddr(t *testing.T) {
	restoreInboundGlobals(t)

	adapterInbound.SetAllowedIPs([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	adapterInbound.SetDisAllowedIPs([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	adapterInbound.SetSkipAuthPrefixes(nil)

	tunnel := &testTunnel{tcpCh: make(chan *C.Metadata, 1)}
	listenAddr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1080}
	rawListener := newChannelListener(listenAddr)
	serverSide, clientSide := net.Pipe()
	t.Cleanup(func() {
		_ = clientSide.Close()
		_ = rawListener.Close()
	})

	serverConn := &observedConn{
		Conn:    serverSide,
		local:   listenAddr,
		remote:  &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 40000},
		closeCh: make(chan struct{}),
	}

	listener := NewWithListener(rawListener, "127.0.0.1:1080", LC.AuthServer{
		Enable:    true,
		Listen:    "127.0.0.1:1080",
		AuthStore: authStore.Default,
	}, tunnel)
	t.Cleanup(func() {
		_ = listener.Close()
	})

	rawListener.ch <- serverConn

	select {
	case <-serverConn.closeCh:
	case <-time.After(time.Second):
		t.Fatal("expected listener to close disallowed connection")
	}

	select {
	case <-tunnel.tcpCh:
		t.Fatal("expected disallowed connection to stop before reaching tunnel")
	default:
	}
}

func TestNewWithListenerAppliesSkipAuthPrefixes(t *testing.T) {
	restoreInboundGlobals(t)

	adapterInbound.SetAllowedIPs([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	adapterInbound.SetDisAllowedIPs(nil)
	adapterInbound.SetSkipAuthPrefixes([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})

	previousAuthenticator := authStore.Default.Authenticator()
	authStore.Default.SetAuthenticator(componentAuth.NewAuthenticator([]componentAuth.AuthUser{{
		User: "demo",
		Pass: "secret",
	}}))
	t.Cleanup(func() {
		authStore.Default.SetAuthenticator(previousAuthenticator)
	})

	tunnel := &testTunnel{tcpCh: make(chan *C.Metadata, 1)}
	listenAddr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1081}
	rawListener := newChannelListener(listenAddr)
	serverSide, clientSide := net.Pipe()
	t.Cleanup(func() {
		_ = clientSide.Close()
		_ = rawListener.Close()
	})

	serverConn := &observedConn{
		Conn:    serverSide,
		local:   listenAddr,
		remote:  &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 40001},
		closeCh: make(chan struct{}),
	}

	listener := NewWithListener(rawListener, "127.0.0.1:1081", LC.AuthServer{
		Enable:    true,
		Listen:    "127.0.0.1:1081",
		AuthStore: authStore.Default,
	}, tunnel)
	t.Cleanup(func() {
		_ = listener.Close()
	})

	rawListener.ch <- serverConn

	if err := clientSide.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set pipe deadline: %v", err)
	}
	request := "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"
	if _, err := clientSide.Write([]byte(request)); err != nil {
		t.Fatalf("write CONNECT request: %v", err)
	}

	buf := make([]byte, 256)
	n, err := clientSide.Read(buf)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if got := string(buf[:n]); !strings.Contains(got, "200 Connection established") {
		t.Fatalf("expected skip-auth CONNECT success, got %q", got)
	}

	select {
	case metadata := <-tunnel.tcpCh:
		if metadata == nil {
			t.Fatal("expected metadata from tunnel")
		}
	case <-time.After(time.Second):
		t.Fatal("expected tunnel to receive CONNECT request")
	}
}

func restoreInboundGlobals(t *testing.T) {
	t.Helper()

	allowed := append([]netip.Prefix(nil), adapterInbound.AllowedIPs()...)
	disallowed := append([]netip.Prefix(nil), adapterInbound.DisAllowedIPs()...)
	skipAuth := append([]netip.Prefix(nil), adapterInbound.SkipAuthPrefixes()...)

	t.Cleanup(func() {
		adapterInbound.SetAllowedIPs(allowed)
		adapterInbound.SetDisAllowedIPs(disallowed)
		adapterInbound.SetSkipAuthPrefixes(skipAuth)
	})
}
