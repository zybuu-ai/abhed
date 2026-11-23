package profiles

import "testing"

// An endpoint on another host needs https, unless it and the server are
// both on this machine.
func TestResolveEndpointHosts(t *testing.T) {
	for _, c := range []struct {
		base, endpoint string
		ok             bool
	}{
		{"https://abhed.corp", "/v1/cli/token", true},
		{"https://abhed.corp", "https://idp.corp/device", true},
		{"https://abhed.corp", "http://idp.corp/device", false},
		{"http://127.0.0.1:8080", "http://127.0.0.1:5556/dex/device", true},
		{"http://localhost:8080", "http://127.0.0.1:5556/dex/device", true},
		{"http://127.0.0.1:8080", "http://idp.lan/device", false},
		{"http://abhed.lan", "http://127.0.0.1:5556/dex/device", false},
	} {
		_, err := Resolve(c.base, c.endpoint)
		if (err == nil) != c.ok {
			t.Errorf("%s against %s: %v", c.endpoint, c.base, err)
		}
	}
}
