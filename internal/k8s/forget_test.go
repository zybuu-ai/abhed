package k8s

import (
	"testing"
	"time"
)

// A fork drops the logins made after its step and keeps the earlier ones.
func TestLoginsForgetAfter(t *testing.T) {
	t0 := time.Now()
	l := &logins{creds: map[string]sessionCred{
		"old": {token: "a", at: t0.Add(-time.Minute)},
		"new": {token: "b", at: t0.Add(time.Minute)},
	}, clusters: map[string]*Cluster{"new": {}}}
	l.ForgetAfter(t0)
	if _, ok := l.creds["old"]; !ok {
		t.Fatal("an earlier login was dropped")
	}
	if _, ok := l.creds["new"]; ok {
		t.Fatal("a later login was kept")
	}
	if _, ok := l.clusters["new"]; ok {
		t.Fatal("a later login's client was kept")
	}
}
