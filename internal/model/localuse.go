package model

import (
	"net"
	"net/url"
	"strings"
	"sync/atomic"
)

// LocalUse, when set, hears each request sent to a model server on this
// machine, by its base URL: what keeps a local server Abhed started from
// being stopped as idle while a session uses it.
var LocalUse atomic.Pointer[func(baseURL string)]

// noteLocalUse tells LocalUse of a request to a loopback base URL.
func noteLocalUse(baseURL string) {
	f := LocalUse.Load()
	if f == nil {
		return
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return
	}
	h := u.Hostname()
	if ip := net.ParseIP(h); strings.EqualFold(h, "localhost") || (ip != nil && ip.IsLoopback()) {
		(*f)(baseURL)
	}
}
