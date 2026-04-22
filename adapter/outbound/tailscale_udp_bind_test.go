package outbound

import (
	"net/netip"
	"strings"
	"testing"
)

func TestPickTailscaleUDPBind(t *testing.T) {
	tests := []struct {
		name    string
		dst     netip.Addr
		ip4     netip.Addr
		ip6     netip.Addr
		want    string
		wantErr string
	}{
		{
			name: "ipv4 destination",
			dst:  netip.MustParseAddr("100.64.0.10"),
			ip4:  netip.MustParseAddr("100.64.0.1"),
			ip6:  netip.MustParseAddr("fd7a:115c:a1e0::1"),
			want: "100.64.0.1:0",
		},
		{
			name: "ipv6 destination",
			dst:  netip.MustParseAddr("fd7a:115c:a1e0::10"),
			ip4:  netip.MustParseAddr("100.64.0.1"),
			ip6:  netip.MustParseAddr("fd7a:115c:a1e0::1"),
			want: "[fd7a:115c:a1e0::1]:0",
		},
		{
			name:    "missing destination",
			ip4:     netip.MustParseAddr("100.64.0.1"),
			wantErr: "resolved destination ip",
		},
		{
			name:    "missing ipv4 bind",
			dst:     netip.MustParseAddr("100.64.0.10"),
			ip6:     netip.MustParseAddr("fd7a:115c:a1e0::1"),
			wantErr: "no ipv4 address",
		},
		{
			name:    "missing ipv6 bind",
			dst:     netip.MustParseAddr("fd7a:115c:a1e0::10"),
			ip4:     netip.MustParseAddr("100.64.0.1"),
			wantErr: "no ipv6 address",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pickTailscaleUDPBind(tt.dst, tt.ip4, tt.ip6)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("pickTailscaleUDPBind returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("bind = %q, want %q", got, tt.want)
			}
		})
	}
}
