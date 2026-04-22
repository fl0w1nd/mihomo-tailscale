//go:build with_tailscale

package tailscalestamp

import (
	"strings"
	"testing"

	tailscaleroot "tailscale.com"
	tsversion "tailscale.com/version"
)

func TestTailscaleVersionStamp(t *testing.T) {
	want := strings.TrimSpace(tailscaleroot.VersionDotTxt)
	if got := tsversion.Short(); got != want {
		t.Fatalf("version.Short() = %q, want %q", got, want)
	}
	if got := tsversion.Long(); got != want {
		t.Fatalf("version.Long() = %q, want %q", got, want)
	}
}
