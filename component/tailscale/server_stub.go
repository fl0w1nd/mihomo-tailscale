//go:build !with_tailscale

package tailscale

import (
	"regexp"
	"strings"
)

var hostnameRegexp = regexp.MustCompile(`[^a-z0-9-]`)

type ServerOptions struct {
	Name         string
	AuthKey      string
	Hostname     string
	ControlURL   string
	Ephemeral    bool
	StateDir     string
	AcceptRoutes *bool
}

type Instance struct{}

// Acquire is the no-tag stub. The real implementation lives in server.go
// and reference-counts shared tsnet instances; the stub just hands back
// an empty value pair so config parsing and unit tests stay buildable
// without the with_tailscale tag.
func Acquire(opts ServerOptions) (*Instance, func()) {
	return &Instance{}, func() {}
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
