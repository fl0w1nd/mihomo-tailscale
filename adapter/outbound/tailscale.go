//go:build with_tailscale

package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/atomic"
	ts "github.com/metacubex/mihomo/component/tailscale"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

type TailscaleOption struct {
	BasicOption
	Name         string `proxy:"name"`
	AuthKey      string `proxy:"auth-key,omitempty"`
	Hostname     string `proxy:"hostname,omitempty"`
	ControlURL   string `proxy:"control-url,omitempty"`
	Ephemeral    bool   `proxy:"ephemeral,omitempty"`
	ExitNode     string `proxy:"exit-node,omitempty"`
	StateDir     string `proxy:"state-dir,omitempty"`
	AcceptRoutes *bool  `proxy:"accept-routes,omitempty"`
}

type Tailscale struct {
	*Base
	option   TailscaleOption
	instance *ts.Instance
	release  func()
	server   *tsnet.Server // cached after first init

	startInstance              func(context.Context) (*tsnet.Server, error)
	syncExitNodePreferenceFunc func(context.Context) error
	tailscaleIPsFunc           func() (netip.Addr, netip.Addr)
	lifecycleCtx               context.Context
	cancelLifecycle            context.CancelFunc

	initOk    atomic.Bool
	initMutex sync.Mutex
	closed    atomic.Bool
}

const initLockRetryInterval = 10 * time.Millisecond

var errTailscaleClosed = errors.New("tailscale proxy closed")

func NewTailscale(option TailscaleOption) (*Tailscale, error) {
	option = normalizeTailscaleOption(option)
	if option.Name == "" {
		return nil, errors.New("tailscale proxy name must not be empty")
	}

	if unsupported := describeUnsupportedTailscaleOptions(option); len(unsupported) != 0 {
		log.Warnln("[TS](%s) ignoring unsupported options: %s", option.Name, strings.Join(unsupported, ", "))
	}

	instance, release := ts.Acquire(ts.ServerOptions{
		Name:         option.Name,
		AuthKey:      option.AuthKey,
		Hostname:     option.Hostname,
		ControlURL:   option.ControlURL,
		Ephemeral:    option.Ephemeral,
		StateDir:     option.StateDir,
		AcceptRoutes: option.AcceptRoutes,
	})

	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	t := &Tailscale{
		Base: NewBase(BaseOption{
			Name:         option.Name,
			Type:         C.Tailscale,
			ProviderName: option.ProviderName,
			UDP:          true,
		}),
		option:          option,
		instance:        instance,
		release:         release,
		lifecycleCtx:    lifecycleCtx,
		cancelLifecycle: cancelLifecycle,
	}
	t.startInstance = instance.Start
	t.syncExitNodePreferenceFunc = t.syncExitNodePreference
	t.tailscaleIPsFunc = func() (netip.Addr, netip.Addr) {
		if t.server == nil {
			return netip.Addr{}, netip.Addr{}
		}
		return t.server.TailscaleIPs()
	}

	return t, nil
}

// Warmup eagerly drives the one-shot tsnet startup so the first real
// DialContext does not pay the authentication / control-plane handshake
// latency. The provided context bounds the bring-up: if it expires before
// tsnet authenticates, the warmup returns with an error logged, the
// initMutex is released, and a subsequent Dial / Warmup may retry.
//
// Warmup uses initMutex.TryLock so multiple ApplyConfig calls cannot pile
// up goroutines waiting on a single in-flight bring-up. If an init is
// already running (warmup or on-demand), follow-up Warmups short-circuit
// immediately. This is critical because the executor schedules a Warmup
// goroutine on every ApplyConfig; without TryLock, a hung first-run auth
// would accumulate one extra goroutine per reload until it eventually
// succeeded or the user fixed the auth state.
//
// Warmup must NOT be called from inside ApplyConfig: it is meant to run
// after tunnel.OnRunning() so it can never race with sing-tun's own
// initialisation, which is what previously caused the global
// net.DefaultResolver hijack to interleave with TUN bring-up.
func (t *Tailscale) Warmup(ctx context.Context) {
	if t.closed.Load() {
		return
	}
	if !t.initMutex.TryLock() {
		log.Debugln("[TS](%s) warmup skipped: init already in progress", t.option.Name)
		return
	}
	defer t.initMutex.Unlock()
	if t.initOk.Load() {
		return
	}
	ctx, cancel := t.withLifecycleContext(ctx)
	defer cancel()
	if err := t.startAndSync(ctx); err != nil {
		if errors.Is(err, errTailscaleClosed) {
			log.Debugln("[TS](%s) warmup stopped: proxy closed", t.option.Name)
			return
		}
		log.Errorln("[TS](%s) startup warmup failed: %v", t.option.Name, err)
	}
}

// init performs the one-shot tsnet startup on the on-demand path
// (DialContext / ListenPacketContext). It is safe to call concurrently:
// only the first caller actually runs the startup, the rest block on the
// mutex and return as soon as it completes.
//
// The caller's context is propagated to tsnet.Server.Up() and to the
// post-Up preference sync. In normal operation the on-demand path runs
// only after Warmup has already authenticated; if a short per-request
// context arrives before Warmup completes, it will short-circuit and
// the next request retries. Errors are not cached so a transient failure
// (network blip, slow first request, control-plane outage) does not
// permanently disable the proxy.
func (t *Tailscale) init(ctx context.Context) error {
	if t.initOk.Load() {
		return nil
	}
	if t.closed.Load() {
		return errTailscaleClosed
	}
	ctx, cancel := t.withLifecycleContext(ctx)
	defer cancel()
	if err := lockWithContext(ctx, &t.initMutex); err != nil {
		if t.closed.Load() && errors.Is(err, context.Canceled) {
			return errTailscaleClosed
		}
		return err
	}
	defer t.initMutex.Unlock()
	if t.initOk.Load() {
		return nil
	}
	if t.closed.Load() {
		return errTailscaleClosed
	}
	return t.startAndSync(ctx)
}

func (t *Tailscale) withLifecycleContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if t.lifecycleCtx == nil {
		return ctx, func() {}
	}

	merged, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(t.lifecycleCtx, cancel)
	return merged, func() {
		stop()
		cancel()
	}
}

func lockWithContext(ctx context.Context, mu *sync.Mutex) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if mu.TryLock() {
		if err := ctx.Err(); err != nil {
			mu.Unlock()
			return err
		}
		return nil
	}

	ticker := time.NewTicker(initLockRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if mu.TryLock() {
				if err := ctx.Err(); err != nil {
					mu.Unlock()
					return err
				}
				return nil
			}
		}
	}
}

// startAndSync runs the actual one-shot bring-up. Callers must hold
// initMutex and have already double-checked initOk. The provided ctx
// bounds both tsnet.Up and the post-Up preference sync, so callers that
// need a longer auth window (Warmup) and callers that pass a per-request
// deadline (DialContext) get exactly the timeout they asked for.
func (t *Tailscale) startAndSync(ctx context.Context) error {
	if t.closed.Load() {
		return errTailscaleClosed
	}
	server, err := t.startInstance(ctx)
	if err != nil {
		if t.closed.Load() && errors.Is(err, context.Canceled) {
			return errTailscaleClosed
		}
		log.Errorln("[TS](%s) startup failed: %v", t.option.Name, err)
		return err
	}
	if t.closed.Load() {
		return errTailscaleClosed
	}
	t.server = server

	if err := t.syncExitNodePreferenceFunc(ctx); err != nil {
		if errors.Is(err, errTailscaleClosed) || (t.closed.Load() && errors.Is(err, context.Canceled)) {
			return errTailscaleClosed
		}
		if t.option.ExitNode != "" {
			log.Warnln("[TS](%s) failed to set exit node %q: %v, falling back to direct", t.option.Name, t.option.ExitNode, err)
		} else {
			log.Warnln("[TS](%s) failed to clear persisted exit node state: %v", t.option.Name, err)
		}
	}
	if t.closed.Load() {
		return errTailscaleClosed
	}

	var ip4, ip6 netip.Addr
	if t.tailscaleIPsFunc != nil {
		ip4, ip6 = t.tailscaleIPsFunc()
	}
	logTailscaleOnline(t.option.Name, ip4, ip6)

	t.initOk.Store(true)
	return nil
}

// logTailscaleOnline emits a single info-level line confirming that tsnet
// has finished authenticating and is ready to serve traffic. Either or
// both addresses may be invalid (e.g. when only one address family is
// assigned, or in unit tests); the format adapts accordingly.
func logTailscaleOnline(name string, ip4, ip6 netip.Addr) {
	switch {
	case ip4.IsValid() && ip6.IsValid():
		log.Infoln("[TS](%s) online (ipv4=%s, ipv6=%s)", name, ip4, ip6)
	case ip4.IsValid():
		log.Infoln("[TS](%s) online (ipv4=%s)", name, ip4)
	case ip6.IsValid():
		log.Infoln("[TS](%s) online (ipv6=%s)", name, ip6)
	default:
		log.Infoln("[TS](%s) online", name)
	}
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
	if t.closed.Load() {
		return errTailscaleClosed
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
	if t.closed.Load() {
		return errTailscaleClosed
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
	ip4, ip6 := t.server.TailscaleIPs()
	bindAddr, err := pickTailscaleUDPBind(metadata.DstIP, ip4, ip6)
	if err != nil {
		return nil, err
	}
	pc, err := t.server.ListenPacket("udp", bindAddr)
	if err != nil {
		return nil, err
	}
	return newPacketConn(pc, t), nil
}

// Close implements C.ProxyAdapter. It releases the shared tsnet instance
// reference; the underlying server is torn down only after the last
// reference is released and the close grace period has elapsed.
func (t *Tailscale) Close() error {
	if t.closed.Swap(true) {
		return nil
	}
	if t.cancelLifecycle != nil {
		t.cancelLifecycle()
	}
	t.server = nil
	if t.release != nil {
		t.release()
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
	// accept-routes defaults to true: the typical use case for routing
	// rules to advertised subnets requires the local tsnet to accept
	// those subnet routes from peers (subnet routers).
	if option.AcceptRoutes == nil {
		v := true
		option.AcceptRoutes = &v
	}
	return option
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
