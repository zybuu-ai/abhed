package model

import (
	"net"
	"net/url"
	"strings"
	"sync/atomic"
)

// LocalUse, when set, hears each request sent to a model server on this
// machine, by its base URL, and the func it returns is called when the
// request ends: what keeps a local server Abhed started from being stopped
// as idle while a session, a completion or an embedding uses it.
var LocalUse atomic.Pointer[func(baseURL string) (end func())]

// LocalRequest tells LocalUse a request to baseURL starts, when baseURL is on
// this machine, and returns what to call when it ends.
func LocalRequest(baseURL string) (end func()) {
	f := LocalUse.Load()
	if f == nil || !loopbackURL(baseURL) {
		return func() {}
	}
	return (*f)(baseURL)
}

func loopbackURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	h := u.Hostname()
	ip := net.ParseIP(h)
	return strings.EqualFold(h, "localhost") || (ip != nil && ip.IsLoopback())
}
