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

	"github.com/metacubex/mihomo/common/atomic"
	_ "github.com/metacubex/mihomo/component/tailscalestamp"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"

	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
)

type ServerOptions struct {
	Name       string
	AuthKey    string
	Hostname   string
	ControlURL string
	Ephemeral  bool
	StateDir   string
}

type Instance struct {
	option ServerOptions
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

func NewInstance(opts ServerOptions) *Instance {
	return &Instance{
		option: opts,
	}
}

func (i *Instance) Start(ctx context.Context) (*tsnet.Server, error) {
	if i.initOk.Load() {
		return i.server, nil
	}
	i.initMutex.Lock()
	defer i.initMutex.Unlock()
	if i.initOk.Load() {
		return i.server, nil
	}
	if i.initErr != nil {
		return nil, i.initErr
	}

	stateDir := i.option.StateDir
	stateKey := SanitizeHostname(i.option.Hostname, i.option.Name)
	if stateDir == "" {
		stateDir = filepath.Join(C.Path.HomeDir(), "tailscale", stateKey)
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		i.initErr = fmt.Errorf("create tailscale state dir: %w", err)
		return nil, i.initErr
	}

	acquireSystemResolverDial()
	i.resolverDialHeld = true

	tsLogf := func(format string, args ...any) {
		log.Debugln("[TS](%s) %s", i.option.Name, fmt.Sprintf(format, args...))
	}
	tsUserLogf := func(format string, args ...any) {
		log.Infoln("[TS](%s) %s", i.option.Name, fmt.Sprintf(format, args...))
	}

	i.server = &tsnet.Server{
		Dir:        stateDir,
		AuthKey:    i.option.AuthKey,
		Hostname:   stateKey,
		ControlURL: i.option.ControlURL,
		Ephemeral:  i.option.Ephemeral,
		Logf:       logger.Logf(tsLogf),
		UserLogf:   logger.Logf(tsUserLogf),
	}

	if _, err := i.server.Up(ctx); err != nil {
		_ = i.server.Close()
		i.server = nil
		i.releaseSystemResolverDial()
		return nil, fmt.Errorf("tailscale up: %w", err)
	}

	i.initOk.Store(true)
	return i.server, nil
}

func (i *Instance) Server() *tsnet.Server {
	return i.server
}

func (i *Instance) Close() error {
	defer i.releaseSystemResolverDial()
	if i.server != nil {
		err := i.server.Close()
		i.server = nil
		return err
	}
	return nil
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

func (i *Instance) releaseSystemResolverDial() {
	if !i.resolverDialHeld {
		return
	}
	releaseSystemResolverDial()
	i.resolverDialHeld = false
}
