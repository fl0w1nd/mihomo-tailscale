//go:build !with_tailscale

package inbound

import (
	"errors"

	C "github.com/metacubex/mihomo/constant"
)

type TailscaleForward struct {
	Listen  uint16 `inbound:"listen"`
	Target  string `inbound:"target"`
	Network string `inbound:"network,omitempty"`
	Proxy   string `inbound:"proxy,omitempty"`
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

type TailscaleListener struct{}

func NewTailscale(options *TailscaleOption) (*TailscaleListener, error) {
	return nil, errors.New("tailscale listener support not compiled in, rebuild with -tags with_tailscale")
}

func (t *TailscaleListener) Name() string {
	return ""
}

func (t *TailscaleListener) Listen(tunnel C.Tunnel) error {
	return nil
}

func (t *TailscaleListener) Close() error {
	return nil
}

func (t *TailscaleListener) Address() string {
	return ""
}

func (t *TailscaleListener) RawAddress() string {
	return ""
}

func (t *TailscaleListener) Config() C.InboundConfig {
	return nil
}
