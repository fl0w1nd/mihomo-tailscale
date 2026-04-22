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

	"github.com/metacubex/mihomo/adapter/inbound"
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

type TailscaleListener struct {
	config        *TailscaleOption
	instance      *ts.Instance
	closeInstance func() error
	server        tailscaleListenServer
	lTCP          []*LT.Listener
	lUDP          []*LT.PacketConn
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

// Address implements constant.InboundListener
func (t *TailscaleListener) Address() string {
	var addrList []string
	for _, l := range t.lTCP {
		addrList = append(addrList, "tcp://"+l.Address())
	}
	for _, l := range t.lUDP {
		addrList = append(addrList, "udp://"+l.Address())
	}
	return strings.Join(addrList, ",")
}

// Listen implements constant.InboundListener
func (t *TailscaleListener) Listen(tunnel C.Tunnel) (err error) {
	cfg := t.config

	t.instance = ts.NewInstance(ts.ServerOptions{
		Name:       cfg.NameStr,
		AuthKey:    cfg.AuthKey,
		Hostname:   cfg.Hostname,
		ControlURL: cfg.ControlURL,
		Ephemeral:  cfg.Ephemeral,
		StateDir:   cfg.StateDir,
	})
	t.closeInstance = t.instance.Close

	server, err := t.instance.Start(context.Background())
	if err != nil {
		err = fmt.Errorf("tailscale listener start: %w", err)
		if closeErr := t.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return err
	}
	t.server = server

	return t.listenOnServer(server, tunnel)
}

func (t *TailscaleListener) listenOnServer(server tailscaleListenServer, tunnel C.Tunnel) (err error) {
	cfg := t.config

	defer func() {
		if err == nil {
			return
		}
		if closeErr := t.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	additions := cfg.additions()
	globalProxy := cfg.globalProxy()

	for _, forward := range cfg.Forwards {
		networks, err := forward.networks()
		if err != nil {
			return err
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
				tcpListener, err := server.Listen("tcp", listenAddr)
				if err != nil {
					return fmt.Errorf("tailscale listen tcp %s: %w", listenAddr, err)
				}
				l, err := LT.NewWithListener(tcpListener, listenAddr, targetAddr, proxyName, tunnel, additions...)
				if err != nil {
					_ = tcpListener.Close()
					return fmt.Errorf("tailscale forward tcp %s -> %s: %w", listenAddr, targetAddr, err)
				}
				t.lTCP = append(t.lTCP, l)
			case "udp":
				udpListeners, err := t.listenForwardUDP(server, listenAddr, targetAddr, proxyName, tunnel, additions...)
				if err != nil {
					return err
				}
				t.lUDP = append(t.lUDP, udpListeners...)
			}
		}
	}

	log.Infoln("Tailscale[%s] forwards listening at: %s", t.Name(), t.Address())
	return nil
}

func (t *TailscaleListener) listenForwardUDP(server tailscaleListenServer, listenAddr, targetAddr, proxyName string, tunnel C.Tunnel, additions ...inbound.Addition) (listeners []*LT.PacketConn, err error) {
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
		pc, err := server.ListenPacket("udp", bindAddr)
		if err != nil {
			err = fmt.Errorf("tailscale listen udp4 %s: %w", bindAddr, err)
			return listeners, err
		}
		l, err := LT.NewUDPWithPacketConn(pc, bindAddr, targetAddr, proxyName, tunnel, additions...)
		if err != nil {
			_ = pc.Close()
			err = fmt.Errorf("tailscale forward udp4 %s -> %s: %w", bindAddr, targetAddr, err)
			return listeners, err
		}
		listeners = append(listeners, l)
	}

	if ip6.IsValid() {
		bindAddr := net.JoinHostPort(ip6.String(), port)
		pc, err := server.ListenPacket("udp", bindAddr)
		if err != nil {
			err = fmt.Errorf("tailscale listen udp6 %s: %w", bindAddr, err)
			return listeners, err
		}
		l, err := LT.NewUDPWithPacketConn(pc, bindAddr, targetAddr, proxyName, tunnel, additions...)
		if err != nil {
			_ = pc.Close()
			err = fmt.Errorf("tailscale forward udp6 %s -> %s: %w", bindAddr, targetAddr, err)
			return listeners, err
		}
		listeners = append(listeners, l)
	}

	return listeners, nil
}

// Close implements constant.InboundListener
func (t *TailscaleListener) Close() error {
	tcpListeners := t.lTCP
	udpListeners := t.lUDP
	closeInstance := t.closeInstance

	t.lTCP = nil
	t.lUDP = nil
	t.instance = nil
	t.closeInstance = nil
	t.server = nil

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
