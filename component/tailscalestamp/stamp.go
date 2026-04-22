//go:build with_tailscale

package tailscalestamp

import (
	"strings"

	tailscaleroot "tailscale.com"
	_ "unsafe"
)

// tsnet embeds tailscale.com as a library. In a non-Tailscale main module,
// version.Short/Long fall back to "-ERR-BuildInfo" because debug.ReadBuildInfo
// only exposes this repo's VCS metadata. Populate the stamp variables eagerly
// so the control plane sees a stable Tailscale release version.

//go:linkname tailscaleLongStamp tailscale.com/version.longStamp
var tailscaleLongStamp string

//go:linkname tailscaleShortStamp tailscale.com/version.shortStamp
var tailscaleShortStamp string

func init() {
	version := strings.TrimSpace(tailscaleroot.VersionDotTxt)
	if version == "" {
		return
	}
	if tailscaleShortStamp == "" {
		tailscaleShortStamp = version
	}
	if tailscaleLongStamp == "" {
		tailscaleLongStamp = version
	}
}
