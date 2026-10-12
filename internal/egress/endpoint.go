package egress

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"time"
)

// InternalEndpoint says why a model endpoint is on an internal address, or
// "" when it is not or cannot be told. The model client is Abhed's own and
// skips the internal-address check the allowlist makes of commands, since a
// local model or gateway is ordinary; only trusted configuration can set
// base_url, so doctor and serve -check warn rather than refuse.
func InternalEndpoint(ctx context.Context, baseURL string, lookup func(context.Context, string) ([]netip.Addr, error)) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := u.Hostname()
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		if lookup == nil {
			lookup = func(ctx context.Context, h string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
			}
		}
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if addrs, err = lookup(ctx, host); err != nil {
			return ""
		}
	}
	for _, a := range addrs {
		if why := AddrRefusal(a); why != "" {
			if host == a.Unmap().String() {
				return fmt.Sprintf("the model's base_url host %s is %s", host, why)
			}
			return fmt.Sprintf("the model's base_url host %s resolves to %s, %s", host, a.Unmap(), why)
		}
	}
	return ""
}
