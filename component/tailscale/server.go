//go:build with_tailscale

package tailscale

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/atomic"
	_ "github.com/metacubex/mihomo/component/tailscalestamp"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"

	"tailscale.com/ipn"
	"tailscale.com/net/netmon"
	"tailscale.com/net/netns"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
)

// closeGracePeriod is the linger window between an Instance's last
// reference being released and the underlying tsnet.Server being closed.
// Within this window, a new Acquire of the same identity short-circuits
// the close and reuses the live instance. This prevents redundant device
// re-registration on the Tailscale control plane during mihomo config
// reloads, where PatchInboundListeners closes the old listener before
// starting the new one.
//
// Declared as a var (not const) so tests can shrink the window without
// having to sleep five seconds per case.
var closeGracePeriod = 5 * time.Second

type ServerOptions struct {
	Name         string
	AuthKey      string
	Hostname     string
	ControlURL   string
	Ephemeral    bool
	StateDir     string
	AcceptRoutes *bool
}

// Instance wraps a single tsnet.Server and the bookkeeping required to
// share it across multiple mihomo adapters (outbound proxy + inbound
// listener) that target the same Tailscale identity.
//
// Instances must be obtained through Acquire so they participate in the
// reference-counted registry. The returned release function decrements
// the counter; the last release schedules a deferred close after
// closeGracePeriod.
//
// Concurrency model:
//   - `optionMu` guards reads and writes to `option` after construction.
//     Subsequent Acquires of the same identity may mutate the cached
//     option set (specifically: promoting a bootstrap auth-key when the
//     first caller supplied none and tsnet has not yet authenticated).
//     `Start` snapshots `option` under `optionMu` before consuming it so
//     readers never race with promotion writes.
//   - `initMutex` serializes the one-shot tsnet bring-up so concurrent
//     callers of `Start` observe a single `tsnet.Up()` execution.
//   - `regMu` (package level) guards `refs`, `closeTimer`, and registry
//     membership.
type Instance struct {
	optionMu sync.Mutex
	option   ServerOptions

	startInProgress bool

	server *tsnet.Server
	key    string

	initOk    atomic.Bool
	initMutex sync.Mutex

	refs       int
	closeTimer *time.Timer

	resolverDialHeld bool
}

var (
	hostnameRegexp        = regexp.MustCompile(`[^a-z0-9-]`)
	defaultResolverMu     sync.Mutex
	defaultResolverUsers  int
	defaultResolverDialFn func(ctx context.Context, network, address string) (net.Conn, error)

	regMu sync.Mutex
	reg   = map[string]*Instance{}
)

// Acquire returns an Instance keyed by identity (resolved state dir +
// control URL + sanitized hostname-or-name). If a live Instance already
// matches that key, the same pointer is returned and the reference
// counter is incremented; otherwise a new Instance is registered.
//
// The returned release function must be called exactly once when the
// caller is done with the Instance. It is idempotent. When the last
// reference is released, the underlying tsnet server lingers for
// closeGracePeriod before being torn down; a subsequent Acquire with the
// same identity during that window cancels the close and reuses the
// existing instance.
//
// Conflict policy is first-wins: if a follow-up Acquire presents
// different node-level options (Ephemeral / AcceptRoutes / non-empty
// AuthKey), the difference is logged at WARN and the original options
// remain authoritative for the lifetime of the shared Instance.
func Acquire(opts ServerOptions) (*Instance, func()) {
	key := identityKeyFromOpts(opts)

	regMu.Lock()
	defer regMu.Unlock()

	if inst, ok := reg[key]; ok {
		if inst.closeTimer != nil {
			inst.closeTimer.Stop()
			inst.closeTimer = nil
		}
		inst.refs++
		inst.handleReacquire(opts)
		return inst, releaseOnce(inst)
	}

	inst := &Instance{
		option: opts,
		key:    key,
		refs:   1,
	}
	reg[key] = inst
	return inst, releaseOnce(inst)
}

func releaseOnce(inst *Instance) func() {
	var once sync.Once
	return func() {
		once.Do(inst.release)
	}
}

func (i *Instance) release() {
	regMu.Lock()
	defer regMu.Unlock()

	if i.refs <= 0 {
		return
	}
	i.refs--
	if i.refs > 0 {
		return
	}
	// Last reference: defer the actual close so a same-identity Acquire
	// arriving within the grace period can revive this instance.
	i.closeTimer = time.AfterFunc(closeGracePeriod, i.maybeClose)
}

func (i *Instance) maybeClose() {
	regMu.Lock()
	if i.refs > 0 || i.closeTimer == nil {
		regMu.Unlock()
		return
	}
	i.optionMu.Lock()
	if i.startInProgress {
		i.closeTimer = time.AfterFunc(closeGracePeriod, i.maybeClose)
		i.optionMu.Unlock()
		regMu.Unlock()
		return
	}
	i.optionMu.Unlock()
	delete(reg, i.key)
	i.closeTimer = nil
	server := i.server
	held := i.resolverDialHeld
	i.server = nil
	i.resolverDialHeld = false
	i.initOk.Store(false)
	regMu.Unlock()

	if server != nil {
		_ = server.Close()
	}
	if held {
		releaseSystemResolverDial()
	}
}

// handleReacquire reconciles the options presented by a follow-up
// Acquire against the cached identity. Node-level disagreements are
// logged as a WARN under first-wins semantics. The one exception is
// auth-key: when the cached identity has no key (e.g. the first Acquire
// is an outbound adapter that did not require a bootstrap secret) and
// the new caller provides one, the key is promoted onto the cached
// option set so the upcoming first Start() can use it to authenticate.
// Promotion is only valid while the shared tsnet has not yet completed
// `Up()`; once authenticated, a late auth-key would be ignored, so we
// surface that as a WARN instead.
//
// Mutation is serialized by optionMu so Start's snapshot path cannot
// observe a torn write.
func (i *Instance) handleReacquire(opts ServerOptions) {
	i.optionMu.Lock()
	defer i.optionMu.Unlock()

	started := i.initOk.Load()
	starting := i.startInProgress

	var deltas []string
	if i.option.Ephemeral != opts.Ephemeral {
		deltas = append(deltas, fmt.Sprintf("ephemeral=%v vs %v", i.option.Ephemeral, opts.Ephemeral))
	}
	if !acceptRoutesEqual(i.option.AcceptRoutes, opts.AcceptRoutes) {
		deltas = append(deltas, fmt.Sprintf("accept-routes=%s vs %s", formatAcceptRoutes(i.option.AcceptRoutes), formatAcceptRoutes(opts.AcceptRoutes)))
	}

	switch {
	case i.option.AuthKey == "" && opts.AuthKey != "":
		if starting {
			deltas = append(deltas, "new auth-key ignored (shared tsnet start already in progress)")
		} else if started {
			deltas = append(deltas, "new auth-key ignored (shared tsnet already up)")
		} else {
			log.Infoln("[TS](%s) adopting auth-key from later Acquire on shared identity", opts.Name)
			i.option.AuthKey = opts.AuthKey
		}
	case i.option.AuthKey != "" && opts.AuthKey != "" && i.option.AuthKey != opts.AuthKey:
		deltas = append(deltas, "auth-key mismatch")
	}

	if len(deltas) == 0 {
		return
	}
	log.Warnln("[TS](%s) reusing existing tsnet identity, ignoring conflicting options: %s", opts.Name, strings.Join(deltas, ", "))
}

func acceptRoutesEqual(a, b *bool) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func formatAcceptRoutes(p *bool) string {
	if p == nil {
		return "<unset>"
	}
	return fmt.Sprintf("%v", *p)
}

// identityKeyFromOpts encodes the identity tuple as a single string.
// Equal keys denote the same tsnet identity and therefore the same
// device on the Tailscale control plane. The auth-key is deliberately
// not part of the key: it is a one-shot bootstrap secret and the
// persistent identity lives in the state directory.
func identityKeyFromOpts(opts ServerOptions) string {
	stateDir := opts.StateDir
	stateKey := SanitizeHostname(opts.Hostname, opts.Name)
	if stateDir == "" {
		stateDir = filepath.Join(C.Path.HomeDir(), "tailscale", stateKey)
	}
	if abs, err := filepath.Abs(stateDir); err == nil {
		stateDir = abs
	}
	return strings.Join([]string{stateDir, opts.ControlURL, stateKey}, "\x00")
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

	ticker := time.NewTicker(10 * time.Millisecond)
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

func (i *Instance) Start(ctx context.Context) (*tsnet.Server, error) {
	if i.initOk.Load() {
		return i.server, nil
	}
	if err := lockWithContext(ctx, &i.initMutex); err != nil {
		return nil, err
	}
	defer i.initMutex.Unlock()
	if i.initOk.Load() {
		return i.server, nil
	}

	// Snapshot the option set under optionMu so a concurrent Acquire
	// promoting a bootstrap auth-key cannot tear the read we are about to
	// hand to tsnet. Once the snapshot is taken, any further promotion
	// only affects subsequent Start cycles (which only happen on failure
	// retries, since success flips initOk and short-circuits the path).
	opt := i.beginStartAttempt()
	defer i.endStartAttempt()

	stateDir := opt.StateDir
	stateKey := SanitizeHostname(opt.Hostname, opt.Name)
	if stateDir == "" {
		stateDir = filepath.Join(C.Path.HomeDir(), "tailscale", stateKey)
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, fmt.Errorf("create tailscale state dir: %w", err)
	}

	acquireSystemResolverDial()
	i.resolverDialHeld = true

	tsLogf := func(format string, args ...any) {
		log.Debugln("[TS](%s) %s", opt.Name, fmt.Sprintf(format, args...))
	}
	tsUserLogf := func(format string, args ...any) {
		log.Infoln("[TS](%s) %s", opt.Name, fmt.Sprintf(format, args...))
	}

	server := &tsnet.Server{
		Dir:        stateDir,
		AuthKey:    opt.AuthKey,
		Hostname:   stateKey,
		ControlURL: opt.ControlURL,
		Ephemeral:  opt.Ephemeral,
		Logf:       logger.Logf(tsLogf),
		UserLogf:   logger.Logf(tsUserLogf),
	}

	if _, err := server.Up(ctx); err != nil {
		_ = server.Close()
		i.releaseSystemResolverDial()
		return nil, fmt.Errorf("tailscale up: %w", err)
	}

	i.server = server

	if err := i.applyNodePrefs(ctx, opt); err != nil {
		log.Warnln("[TS](%s) failed to apply node prefs: %v", opt.Name, err)
	}

	i.initOk.Store(true)
	return i.server, nil
}

func (i *Instance) beginStartAttempt() ServerOptions {
	i.optionMu.Lock()
	defer i.optionMu.Unlock()
	i.startInProgress = true
	return i.option
}

func (i *Instance) endStartAttempt() {
	i.optionMu.Lock()
	i.startInProgress = false
	i.optionMu.Unlock()
}

// applyNodePrefs syncs node-identity preferences (currently only
// accept-routes) right after the tsnet server comes up. It runs under
// initMutex, so concurrent Start calls observe a single application.
// Takes the snapshot as a parameter so the caller controls which option
// set is authoritative for this Start cycle.
//
// accept-routes is treated as a node-level preference: a subnet route
// advertised by a tailnet peer is either accepted by this node or not,
// regardless of whether traffic ends up arriving through the outbound or
// the inbound listener.
func (i *Instance) applyNodePrefs(ctx context.Context, opt ServerOptions) error {
	if opt.AcceptRoutes == nil {
		return nil
	}
	desired := *opt.AcceptRoutes

	lc, err := i.server.LocalClient()
	if err != nil {
		return fmt.Errorf("get local client: %w", err)
	}
	prefs, err := lc.GetPrefs(ctx)
	if err != nil {
		return fmt.Errorf("get prefs: %w", err)
	}
	if prefs.RouteAll == desired {
		return nil
	}
	if _, err := lc.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs:       ipn.Prefs{RouteAll: desired},
		RouteAllSet: true,
	}); err != nil {
		return fmt.Errorf("set route-all prefs: %w", err)
	}
	log.Infoln("[TS](%s) accept-routes set to %v", opt.Name, desired)
	return nil
}

func (i *Instance) Server() *tsnet.Server {
	return i.server
}

func SanitizeHostname(hostname, fallback string) string {
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

func acquireSystemResolverDial() {
	defaultResolverMu.Lock()
	defer defaultResolverMu.Unlock()

	if defaultResolverUsers == 0 {
		defaultResolverDialFn = net.DefaultResolver.Dial
		dialer := netns.NewDialer(logger.Discard, netmon.NewStatic())
		net.DefaultResolver.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
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

func (i *Instance) releaseSystemResolverDial() {
	if !i.resolverDialHeld {
		return
	}
	releaseSystemResolverDial()
	i.resolverDialHeld = false
}
