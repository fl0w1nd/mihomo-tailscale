//go:build !with_tailscale

package outbound

import (
	"errors"
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
}

func NewTailscale(option TailscaleOption) (*Tailscale, error) {
	return nil, errors.New("tailscale support not compiled in, rebuild with -tags with_tailscale")
}
