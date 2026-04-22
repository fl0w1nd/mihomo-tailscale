//go:build !with_tailscale

package tailscale

import (
	"regexp"
	"strings"
)

var hostnameRegexp = regexp.MustCompile(`[^a-z0-9-]`)

type ServerOptions struct {
	Name       string
	AuthKey    string
	Hostname   string
	ControlURL string
	Ephemeral  bool
	StateDir   string
}

type Instance struct{}

func NewInstance(opts ServerOptions) *Instance {
	return &Instance{}
}

func (i *Instance) Close() error {
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
