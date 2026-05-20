//go:build with_tailscale

package inbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter/inbound"
	"github.com/metacubex/mihomo/common/atomic"
	ts "github.com/metacubex/mihomo/component/tailscale"
	C "github.com/metacubex/mihomo/constant"
	LT "github.com/metacubex/mihomo/listener/tunnel"
	"github.com/metacubex/mihomo/log"
)

type tailscaleListenServer interface {
	Listen(network, address string) (net.Listener, error)
	ListenPacket(network, address string) (net.PacketConn, error)
	TailscaleIPs() (netip.Addr, netip.Addr)
}

type TailscaleForward struct {
	Listen  uint16 `inbound:"listen"`
	Target  string `inbound:"target"`
	Network string `inbound:"network,omitempty"`
	Proxy   string `inbound:"proxy,omitempty"`
}

func (f TailscaleForward) networks() ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(f.Network)) {
	case "", "auto":
		return []string{"tcp", "udp"}, nil
	case "tcp":
		return []string{"tcp"}, nil
	case "udp":
		return []string{"udp"}, nil
	default:
		return nil, fmt.Errorf("unsupported tailscale forward network %q", f.Network)
	}
}

func (f TailscaleForward) listenAddr() string {
	return ":" + strconv.Itoa(int(f.Listen))
}

func (f TailscaleForward) targetAddr() string {
	return strings.TrimSpace(f.Target)
}

func (f TailscaleForward) proxyName() string {
	return strings.TrimSpace(f.Proxy)
}

type TailscaleOption struct {
	NameStr      string             `inbound:"name"`
	SpecialRules string             `inbound:"rule,omitempty"`
	SpecialProxy string             `inbound:"proxy,omitempty"`
	AuthKey      string             `inbound:"auth-key,omitempty"`
	Hostname     string             `inbound:"hostname,omitempty"`
	ControlURL   string             `inbound:"control-url,omitempty"`
	Ephemeral    bool               `inbound:"ephemeral,omitempty"`
	StateDir     string             `inbound:"state-dir,omitempty"`
	AcceptRoutes *bool              `inbound:"accept-routes,omitempty"`
	Forwards     []TailscaleForward `inbound:"forwards"`
}

func (o TailscaleOption) Name() string {
	return o.NameStr
}

func (o TailscaleOption) Equal(config C.InboundConfig) bool {
	return optionToString(o) == optionToString(config)
}

func (o TailscaleOption) additions() []inbound.Addition {
	return []inbound.Addition{
		inbound.WithInName(o.NameStr),
		inbound.WithSpecialRules(o.SpecialRules),
	}
}

func (o TailscaleOption) globalProxy() string {
	return strings.TrimSpace(o.SpecialProxy)
}

// Backoff schedule for the inbound bring-up goroutine. Tunable via vars
// so tests can shrink the schedule.
var (
	// initialBackoff is the delay after the first failed Start attempt.
	initialBackoff = 1 * time.Second
	// maxBackoff caps the exponential backoff between Start attempts so
	// recovery latency stays bounded even after long failure streaks.
	maxBackoff = 60 * time.Second
	// startAttemptTimeout bounds a single instance.Start invocation. If
	// tsnet.Up does not return within this window (network unreachable,
	// control plane silent, auth-key wrong, etc.) the attempt is aborted,
	// the resolver bypass is released, and a new attempt is scheduled
	// after the next backoff slice.
	startAttemptTimeout = 60 * time.Second
	// closeShutdownGrace caps how long Close() waits for the bring-up
	// goroutine to observe cancellation and exit. The goroutine always
	// honours lifecycleCtx so this is a defence-in-depth bound, not the
	// expected path.
	closeShutdownGrace = 5 * time.Second
)

// startInstanceFunc is the bring-up callback used by the inbound
// goroutine. The default implementation delegates to the shared
// `*ts.Instance.Start`. Tests inject a fake so Listen() can be exercised
// without booting tsnet.
type startInstanceFunc func(ctx context.Context) (tailscaleListenServer, error)

type TailscaleListener struct {
	config *TailscaleOption

	startInstanceFn startInstanceFunc

	mu              sync.Mutex
	instance        *ts.Instance
	closeInstance   func() error
	server          tailscaleListenServer
	lTCP            []*LT.Listener
	lUDP            []*LT.PacketConn
	lifecycleCtx    context.Context
	cancelLifecycle context.CancelFunc
	doneCh          chan struct{}

	closed atomic.Bool
	ready  atomic.Bool
}

func NewTailscale(options *TailscaleOption) (*TailscaleListener, error) {
	options = normalizeTailscaleOption(options)
	if options.NameStr == "" {
		return nil, errors.New("tailscale listener name must not be empty")
	}
	if len(options.Forwards) == 0 {
		return nil, errors.New("tailscale listener requires at least one forward")
	}
	for _, forward := range options.Forwards {
		if err := validateTailscaleForward(forward); err != nil {
			return nil, err
		}
	}

	return &TailscaleListener{
		config: options,
	}, nil
}

// Name implements constant.InboundListener
func (t *TailscaleListener) Name() string {
	return t.config.Name()
}

// Config implements constant.InboundListener
func (t *TailscaleListener) Config() C.InboundConfig {
	return t.config
}

// RawAddress implements constant.InboundListener
func (t *TailscaleListener) RawAddress() string {
	return t.Address()
}

// Address implements constant.InboundListener. Returns the empty string
// while the bring-up goroutine has not yet finished creating forwards;
// callers that surface this to humans should treat empty as "pending".
func (t *TailscaleListener) Address() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var addrList []string
	for _, l := range t.lTCP {
		addrList = append(addrList, "tcp://"+l.Address())
	}
	for _, l := range t.lUDP {
		addrList = append(addrList, "udp://"+l.Address())
	}
	return strings.Join(addrList, ",")
}

// Listen implements constant.InboundListener.
//
// Listen acquires the shared tsnet Instance and immediately returns nil
// after spawning the background bring-up goroutine. The tsnet handshake
// (which can take seconds at best and is unbounded at worst when the
// control plane is unreachable or the auth-key is wrong) never blocks
// hub.executor.ApplyConfig, listener.PatchInboundListeners, or any
// other caller in the configuration-apply path. Failures are logged and
// retried with exponential backoff until Close() is invoked.
//
// Trade-off: a fresh deployment may have its forwards "eventually
// available" rather than ready the instant Listen() returns. Configured
// targets are reachable from the tailnet as soon as the goroutine
// finishes a successful Start + setupForwards round; until then,
// `Address()` returns the empty string and the listener is considered
// pending.
func (t *TailscaleListener) Listen(tunnel C.Tunnel) error {
	if t.closed.Load() {
		return errors.New("tailscale listener already closed")
	}

	cfg := t.config
	instance, release := ts.Acquire(ts.ServerOptions{
		Name:         cfg.NameStr,
		AuthKey:      cfg.AuthKey,
		Hostname:     cfg.Hostname,
		ControlURL:   cfg.ControlURL,
		Ephemeral:    cfg.Ephemeral,
		StateDir:     cfg.StateDir,
		AcceptRoutes: cfg.AcceptRoutes,
	})

	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	doneCh := make(chan struct{})

	startFn := t.startInstanceFn
	if startFn == nil {
		startFn = func(ctx context.Context) (tailscaleListenServer, error) {
			return instance.Start(ctx)
		}
	}

	t.mu.Lock()
	t.instance = instance
	t.closeInstance = func() error {
		release()
		return nil
	}
	t.lifecycleCtx = lifecycleCtx
	t.cancelLifecycle = cancelLifecycle
	t.doneCh = doneCh
	t.mu.Unlock()

	go t.runBringUp(lifecycleCtx, startFn, tunnel, doneCh)
	log.Infoln("Tailscale[%s] listener registered; tsnet handshake running in background", t.Name())
	return nil
}

// runBringUp is the long-lived goroutine that drives tsnet.Up and
// forward creation. It exits only when lifecycleCtx is cancelled (by
// Close()) or after a successful Start + setupForwards round.
func (t *TailscaleListener) runBringUp(lifecycleCtx context.Context, startFn startInstanceFunc, tunnel C.Tunnel, doneCh chan struct{}) {
	defer close(doneCh)

	backoff := initialBackoff
	for attempt := 1; ; attempt++ {
		if lifecycleCtx.Err() != nil {
			return
		}

		server, err := t.doStartAttempt(lifecycleCtx, startFn)
		if err == nil {
			if setupErr := t.applyForwards(lifecycleCtx, server, tunnel); setupErr != nil {
				err = setupErr
			} else {
				t.ready.Store(true)
				log.Infoln("Tailscale[%s] forwards listening at: %s", t.Name(), t.Address())
				return
			}
		}

		if lifecycleCtx.Err() != nil {
			return
		}

		log.Errorln("Tailscale[%s] bring-up attempt %d failed: %v (retry in %s)", t.Name(), attempt, err, backoff)

		select {
		case <-lifecycleCtx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// doStartAttempt runs one bounded instance.Start cycle. The per-attempt
// timeout caps how long a single tsnet.Up may hang before we surface a
// retry, which in turn caps how long the process-wide resolver bypass
// inside component/tailscale may stay acquired during a hung attempt.
func (t *TailscaleListener) doStartAttempt(lifecycleCtx context.Context, startFn startInstanceFunc) (tailscaleListenServer, error) {
	ctx, cancel := context.WithTimeout(lifecycleCtx, startAttemptTimeout)
	defer cancel()
	return startFn(ctx)
}

// applyForwards is the post-Start handoff: it stashes the live server
// on the listener and calls setupForwards. On any failure it rolls back
// all listeners created during this attempt and clears the cached
// server pointer, so the next retry starts from a clean slate.
func (t *TailscaleListener) applyForwards(lifecycleCtx context.Context, server tailscaleListenServer, tunnel C.Tunnel) error {
	if lifecycleCtx.Err() != nil {
		return lifecycleCtx.Err()
	}

	t.mu.Lock()
	t.server = server
	t.mu.Unlock()

	tcp, udp, err := t.setupForwards(server, tunnel)
	if err != nil {
		t.mu.Lock()
		t.server = nil
		t.mu.Unlock()
		return err
	}

	t.mu.Lock()
	t.lTCP = tcp
	t.lUDP = udp
	t.mu.Unlock()
	return nil
}

// setupForwards creates every TCP/UDP listener requested by the config
// against the provided tsnet server. On any error every listener created
// during this invocation is closed and the error is returned; nothing is
// stashed on the receiver until the caller (applyForwards) decides the
// attempt succeeded.
func (t *TailscaleListener) setupForwards(server tailscaleListenServer, tunnel C.Tunnel) (tcp []*LT.Listener, udp []*LT.PacketConn, err error) {
	cfg := t.config
	additions := cfg.additions()
	globalProxy := cfg.globalProxy()

	defer func() {
		if err == nil {
			return
		}
		for _, l := range tcp {
			_ = l.Close()
		}
		for _, l := range udp {
			_ = l.Close()
		}
		tcp = nil
		udp = nil
	}()

	for _, forward := range cfg.Forwards {
		networks, nerr := forward.networks()
		if nerr != nil {
			err = nerr
			return
		}

		listenAddr := forward.listenAddr()
		targetAddr := forward.targetAddr()
		proxyName := globalProxy
		if forwardProxy := forward.proxyName(); forwardProxy != "" {
			proxyName = forwardProxy
		}

		for _, network := range networks {
			switch network {
			case "tcp":
				tcpListener, lerr := server.Listen("tcp", listenAddr)
				if lerr != nil {
					err = fmt.Errorf("tailscale listen tcp %s: %w", listenAddr, lerr)
					return
				}
				wrapped, werr := LT.NewWithListener(tcpListener, listenAddr, targetAddr, proxyName, tunnel, additions...)
				if werr != nil {
					_ = tcpListener.Close()
					err = fmt.Errorf("tailscale forward tcp %s -> %s: %w", listenAddr, targetAddr, werr)
					return
				}
				tcp = append(tcp, wrapped)
			case "udp":
				udpListeners, uerr := t.openForwardUDP(server, listenAddr, targetAddr, proxyName, tunnel, additions...)
				if uerr != nil {
					err = uerr
					return
				}
				udp = append(udp, udpListeners...)
			}
		}
	}
	return tcp, udp, nil
}

func (t *TailscaleListener) openForwardUDP(server tailscaleListenServer, listenAddr, targetAddr, proxyName string, tunnel C.Tunnel, additions ...inbound.Addition) (listeners []*LT.PacketConn, err error) {
	ip4, ip6 := server.TailscaleIPs()
	port := strings.TrimPrefix(listenAddr, ":")

	defer func() {
		if err == nil {
			return
		}
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()

	if ip4.IsValid() {
		bindAddr := net.JoinHostPort(ip4.String(), port)
		pc, lerr := server.ListenPacket("udp", bindAddr)
		if lerr != nil {
			err = fmt.Errorf("tailscale listen udp4 %s: %w", bindAddr, lerr)
			return listeners, err
		}
		l, werr := LT.NewUDPWithPacketConn(pc, bindAddr, targetAddr, proxyName, tunnel, additions...)
		if werr != nil {
			_ = pc.Close()
			err = fmt.Errorf("tailscale forward udp4 %s -> %s: %w", bindAddr, targetAddr, werr)
			return listeners, err
		}
		listeners = append(listeners, l)
	}

	if ip6.IsValid() {
		bindAddr := net.JoinHostPort(ip6.String(), port)
		pc, lerr := server.ListenPacket("udp", bindAddr)
		if lerr != nil {
			err = fmt.Errorf("tailscale listen udp6 %s: %w", bindAddr, lerr)
			return listeners, err
		}
		l, werr := LT.NewUDPWithPacketConn(pc, bindAddr, targetAddr, proxyName, tunnel, additions...)
		if werr != nil {
			_ = pc.Close()
			err = fmt.Errorf("tailscale forward udp6 %s -> %s: %w", bindAddr, targetAddr, werr)
			return listeners, err
		}
		listeners = append(listeners, l)
	}

	return listeners, nil
}

// Close implements constant.InboundListener. It is idempotent: callers
// may invoke it from rollback paths and reload paths without worrying
// about double-release. After Close returns, the bring-up goroutine is
// guaranteed to have exited (modulo the closeShutdownGrace defence) and
// the shared Instance reference is released exactly once.
func (t *TailscaleListener) Close() error {
	if t.closed.Swap(true) {
		return nil
	}

	t.mu.Lock()
	cancel := t.cancelLifecycle
	doneCh := t.doneCh
	tcpListeners := t.lTCP
	udpListeners := t.lUDP
	closeInstance := t.closeInstance
	t.cancelLifecycle = nil
	t.doneCh = nil
	t.lTCP = nil
	t.lUDP = nil
	t.instance = nil
	t.closeInstance = nil
	t.server = nil
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if doneCh != nil {
		select {
		case <-doneCh:
		case <-time.After(closeShutdownGrace):
			log.Warnln("Tailscale[%s] bring-up goroutine did not exit within %s; releasing instance anyway", t.Name(), closeShutdownGrace)
		}
	}

	var errs []error
	for _, l := range tcpListeners {
		if err := l.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close tcp listener %s: %w", l.Address(), err))
		}
	}
	for _, l := range udpListeners {
		if err := l.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close udp listener %s: %w", l.Address(), err))
		}
	}
	if closeInstance != nil {
		if err := closeInstance(); err != nil {
			errs = append(errs, fmt.Errorf("close tailscale instance: %w", err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func normalizeTailscaleOption(option *TailscaleOption) *TailscaleOption {
	option.NameStr = strings.TrimSpace(option.NameStr)
	option.SpecialRules = strings.TrimSpace(option.SpecialRules)
	option.SpecialProxy = strings.TrimSpace(option.SpecialProxy)
	option.AuthKey = strings.TrimSpace(option.AuthKey)
	option.Hostname = strings.TrimSpace(option.Hostname)
	option.ControlURL = strings.TrimSpace(option.ControlURL)
	option.StateDir = strings.TrimSpace(option.StateDir)
	if option.StateDir != "" {
		option.StateDir = filepath.Clean(option.StateDir)
	}
	// accept-routes defaults to true, matching the outbound adapter. The
	// switch is a node-level pref and the typical "use the tailnet for
	// real routing" deployment needs subnet routes accepted.
	if option.AcceptRoutes == nil {
		v := true
		option.AcceptRoutes = &v
	}
	for i := range option.Forwards {
		option.Forwards[i].Target = strings.TrimSpace(option.Forwards[i].Target)
		option.Forwards[i].Network = strings.TrimSpace(option.Forwards[i].Network)
		option.Forwards[i].Proxy = strings.TrimSpace(option.Forwards[i].Proxy)
	}
	return option
}

func validateTailscaleForward(forward TailscaleForward) error {
	if forward.Listen == 0 {
		return errors.New("tailscale forward listen port must be greater than 0")
	}
	target := strings.TrimSpace(forward.Target)
	if target == "" {
		return fmt.Errorf("tailscale forward %d target must not be empty", forward.Listen)
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		return fmt.Errorf("tailscale forward %d target must be host:port, got %q", forward.Listen, target)
	}
	if _, err := forward.networks(); err != nil {
		return err
	}
	return nil
}

var _ C.InboundListener = (*TailscaleListener)(nil)
