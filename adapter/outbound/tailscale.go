//go:build with_tailscale

package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/metacubex/mihomo/common/atomic"
	_ "github.com/metacubex/mihomo/component/tailscalestamp"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
)

type TailscaleOption struct {
	BasicOption
	Name       string `proxy:"name"`
	AuthKey    string `proxy:"auth-key,omitempty"`
	Hostname   string `proxy:"hostname,omitempty"`
	ControlURL string `proxy:"control-url,omitempty"`
	Ephemeral  bool   `proxy:"ephemeral,omitempty"`
	ExitNode   string `proxy:"exit-node,omitempty"`
	StateDir   string `proxy:"state-dir,omitempty"`
}

type Tailscale struct {
	*Base
	option TailscaleOption
	server *tsnet.Server

	initOk    atomic.Bool
	initMutex sync.Mutex
	initErr   error

	resolverDialHeld bool
}

var (
	hostnameRegexp        = regexp.MustCompile(`[^a-z0-9-]`)
	defaultResolverMu     sync.Mutex
	defaultResolverUsers  int
	defaultResolverDialFn func(ctx context.Context, network, address string) (net.Conn, error)
)

func NewTailscale(option TailscaleOption) (*Tailscale, error) {
	option = normalizeTailscaleOption(option)
	if option.Name == "" {
		return nil, errors.New("tailscale proxy name must not be empty")
	}

	if unsupported := describeUnsupportedTailscaleOptions(option); len(unsupported) != 0 {
		log.Warnln("[TS](%s) ignoring unsupported options: %s", option.Name, strings.Join(unsupported, ", "))
	}

	t := &Tailscale{
		Base: NewBase(BaseOption{
			Name:         option.Name,
			Type:         C.Tailscale,
			ProviderName: option.ProviderName,
			UDP:          true,
		}),
		option: option,
	}

	return t, nil
}

func (t *Tailscale) init(ctx context.Context) error {
	if t.initOk.Load() {
		return nil
	}
	t.initMutex.Lock()
	defer t.initMutex.Unlock()
	if t.initOk.Load() {
		return nil
	}
	if t.initErr != nil {
		return t.initErr
	}

	stateDir := t.option.StateDir
	if stateDir == "" {
		stateDir = filepath.Join(C.Path.HomeDir(), "tailscale", t.option.Name)
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.initErr = fmt.Errorf("create tailscale state dir: %w", err)
		return t.initErr
	}

	acquireSystemResolverDial()
	t.resolverDialHeld = true

	tsLogf := func(format string, args ...any) {
		log.Debugln("[TS](%s) %s", t.option.Name, fmt.Sprintf(format, args...))
	}
	tsUserLogf := func(format string, args ...any) {
		log.Infoln("[TS](%s) %s", t.option.Name, fmt.Sprintf(format, args...))
	}

	t.server = &tsnet.Server{
		Dir:        stateDir,
		AuthKey:    t.option.AuthKey,
		Hostname:   sanitizeTailscaleHostname(t.option.Hostname, t.option.Name),
		ControlURL: t.option.ControlURL,
		Ephemeral:  t.option.Ephemeral,
		Logf:       logger.Logf(tsLogf),
		UserLogf:   logger.Logf(tsUserLogf),
	}

	if _, err := t.server.Up(ctx); err != nil {
		_ = t.server.Close()
		t.server = nil
		t.releaseSystemResolverDial()
		return fmt.Errorf("tailscale up: %w", err)
	}

	if err := t.syncExitNodePreference(ctx); err != nil {
		if t.option.ExitNode != "" {
			log.Warnln("[TS](%s) failed to set exit node %q: %v, falling back to direct", t.option.Name, t.option.ExitNode, err)
		} else {
			log.Warnln("[TS](%s) failed to clear persisted exit node state: %v", t.option.Name, err)
		}
	}

	t.initOk.Store(true)
	return nil
}

func (t *Tailscale) syncExitNodePreference(ctx context.Context) error {
	lc, err := t.server.LocalClient()
	if err != nil {
		return fmt.Errorf("get local client: %w", err)
	}

	prefs, err := lc.GetPrefs(ctx)
	if err != nil {
		return fmt.Errorf("get prefs: %w", err)
	}

	if t.option.ExitNode == "" {
		return t.clearExitNode(ctx, lc, prefs)
	}

	return t.setupExitNode(ctx, lc, prefs)
}

func (t *Tailscale) setupExitNode(ctx context.Context, lc *local.Client, prefs *ipn.Prefs) error {
	id, ok, err := t.resolveExitNode(ctx, lc, t.option.ExitNode)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("exit node %q not found or not available as exit node", t.option.ExitNode)
	}
	if prefs.ExitNodeID == id && !prefs.ExitNodeIP.IsValid() && prefs.AutoExitNode == "" {
		return nil
	}

	_, err = lc.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			ExitNodeID: id,
		},
		ExitNodeIDSet:   true,
		ExitNodeIPSet:   true,
		AutoExitNodeSet: true,
	})
	if err != nil {
		return fmt.Errorf("set exit node prefs: %w", err)
	}

	log.Infoln("[TS](%s) exit node set to %s (ID: %s)", t.option.Name, t.option.ExitNode, id)
	return nil
}

func (t *Tailscale) clearExitNode(ctx context.Context, lc *local.Client, prefs *ipn.Prefs) error {
	if prefs.ExitNodeID == "" && !prefs.ExitNodeIP.IsValid() && prefs.AutoExitNode == "" {
		return nil
	}

	_, err := lc.EditPrefs(ctx, &ipn.MaskedPrefs{
		ExitNodeIDSet:   true,
		ExitNodeIPSet:   true,
		AutoExitNodeSet: true,
	})
	if err != nil {
		return fmt.Errorf("clear exit node prefs: %w", err)
	}

	log.Infoln("[TS](%s) cleared persisted exit node selection", t.option.Name)
	return nil
}

func (t *Tailscale) resolveExitNode(ctx context.Context, lc *local.Client, raw string) (tailcfg.StableNodeID, bool, error) {
	st, err := lc.Status(ctx)
	if err != nil {
		return "", false, fmt.Errorf("get status: %w", err)
	}

	for _, peer := range st.Peer {
		if !peer.ExitNodeOption || !peer.Online {
			continue
		}
		if matchPeer(peer, raw) {
			return peer.ID, true, nil
		}
	}
	return "", false, nil
}

func matchPeer(peer *ipnstate.PeerStatus, raw string) bool {
	if string(peer.ID) == raw {
		return true
	}
	dnsName := strings.TrimSuffix(peer.DNSName, ".")
	if dnsName != "" && strings.EqualFold(dnsName, raw) {
		return true
	}
	if peer.HostName != "" && strings.EqualFold(peer.HostName, raw) {
		return true
	}
	for _, ip := range peer.TailscaleIPs {
		if ip.String() == raw {
			return true
		}
	}
	return false
}

func (t *Tailscale) DialContext(ctx context.Context, metadata *C.Metadata) (_ C.Conn, err error) {
	if err = t.init(ctx); err != nil {
		return nil, err
	}
	conn, err := t.server.Dial(ctx, "tcp", metadata.RemoteAddress())
	if err != nil {
		return nil, err
	}
	updateMetadataRemoteIP(metadata, conn.RemoteAddr())
	return NewConn(conn, t), nil
}

func (t *Tailscale) ListenPacketContext(ctx context.Context, metadata *C.Metadata) (_ C.PacketConn, err error) {
	if err = t.init(ctx); err != nil {
		return nil, err
	}
	// TODO: tsnet.Server.Dial with "udp" returns a net.Conn, not a net.PacketConn.
	// If this doesn't work correctly in practice, fall back to t.server.ListenPacket("udp", ":0")
	// and use WriteTo to send packets to the target address.
	conn, err := t.server.Dial(ctx, "udp", metadata.RemoteAddress())
	if err != nil {
		return nil, err
	}
	updateMetadataRemoteIP(metadata, conn.RemoteAddr())
	return newPacketConn(newFakePacketConn(conn), t), nil
}

// Close implements C.ProxyAdapter
func (t *Tailscale) Close() error {
	defer t.releaseSystemResolverDial()
	if t.server != nil {
		err := t.server.Close()
		t.server = nil
		return err
	}
	return nil
}

// IsL3Protocol implements C.ProxyAdapter
func (t *Tailscale) IsL3Protocol(metadata *C.Metadata) bool {
	return true
}

// ProxyInfo implements C.ProxyAdapter
func (t *Tailscale) ProxyInfo() C.ProxyInfo {
	return t.Base.ProxyInfo()
}

// fakePacketConn wraps a net.Conn to implement net.PacketConn for UDP-over-Dial.
type fakePacketConn struct {
	net.Conn
}

func newFakePacketConn(conn net.Conn) *fakePacketConn {
	return &fakePacketConn{Conn: conn}
}

func (c *fakePacketConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	n, err = c.Conn.Read(p)
	return n, c.Conn.RemoteAddr(), err
}

func (c *fakePacketConn) WriteTo(p []byte, _ net.Addr) (n int, err error) {
	return c.Conn.Write(p)
}

func normalizeTailscaleOption(option TailscaleOption) TailscaleOption {
	option.Name = strings.TrimSpace(option.Name)
	option.AuthKey = strings.TrimSpace(option.AuthKey)
	option.Hostname = strings.TrimSpace(option.Hostname)
	option.ControlURL = strings.TrimSpace(option.ControlURL)
	option.ExitNode = strings.TrimSpace(option.ExitNode)
	option.StateDir = strings.TrimSpace(option.StateDir)
	if option.StateDir != "" {
		option.StateDir = filepath.Clean(option.StateDir)
	}
	return option
}

func sanitizeTailscaleHostname(hostname, fallback string) string {
	if hostname == "" {
		hostname = fallback
	}
	hostname = strings.ToLower(hostname)
	hostname = hostnameRegexp.ReplaceAllString(hostname, "-")
	hostname = strings.Trim(hostname, "-")
	if len(hostname) > 63 {
		hostname = strings.Trim(hostname[:63], "-")
	}
	if hostname == "" {
		return "mihomo"
	}
	return hostname
}

func describeUnsupportedTailscaleOptions(option TailscaleOption) []string {
	var unsupported []string
	if option.DialerProxy != "" {
		unsupported = append(unsupported, "dialer-proxy")
	}
	if option.Interface != "" {
		unsupported = append(unsupported, "interface-name")
	}
	if option.RoutingMark != 0 {
		unsupported = append(unsupported, "routing-mark")
	}
	if option.IPVersion != C.DualStack {
		unsupported = append(unsupported, "ip-version")
	}
	if option.TFO {
		unsupported = append(unsupported, "tfo")
	}
	if option.MPTCP {
		unsupported = append(unsupported, "mptcp")
	}
	return unsupported
}

func updateMetadataRemoteIP(metadata *C.Metadata, addr net.Addr) {
	if metadata == nil || metadata.Resolved() {
		return
	}

	remote := &C.Metadata{}
	if err := remote.SetRemoteAddr(addr); err != nil {
		return
	}
	if remote.DstIP.IsValid() {
		metadata.DstIP = remote.DstIP
	}
}

func acquireSystemResolverDial() {
	defaultResolverMu.Lock()
	defer defaultResolverMu.Unlock()

	if defaultResolverUsers == 0 {
		defaultResolverDialFn = net.DefaultResolver.Dial
		net.DefaultResolver.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, address)
		}
	}
	defaultResolverUsers++
}

func releaseSystemResolverDial() {
	defaultResolverMu.Lock()
	defer defaultResolverMu.Unlock()

	if defaultResolverUsers == 0 {
		return
	}
	defaultResolverUsers--
	if defaultResolverUsers == 0 {
		net.DefaultResolver.Dial = defaultResolverDialFn
		defaultResolverDialFn = nil
	}
}

func (t *Tailscale) releaseSystemResolverDial() {
	if !t.resolverDialHeld {
		return
	}
	releaseSystemResolverDial()
	t.resolverDialHeld = false
}
