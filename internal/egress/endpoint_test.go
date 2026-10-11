package egress

import (
	"context"
	"net/netip"
	"strings"
	"testing"
)

func TestInternalEndpoint(t *testing.T) {
	lookup := func(_ context.Context, h string) ([]netip.Addr, error) {
		if h == "meta.internal" {
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	for _, tc := range []struct{ url, want string }{
		{"http://169.254.169.254/v1", "link-local"},
		{"http://127.0.0.1:4000/v1", "loopback"},
		{"https://meta.internal/v1", "resolves to 169.254.169.254"},
		{"https://api.example.com/v1", ""},
		{"", ""},
	} {
		got := InternalEndpoint(context.Background(), tc.url, lookup)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q", tc.url, got)
		}
	}
}
