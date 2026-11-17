package auth

import "testing"

// A server identity can never own what a machine's guest or accounts record.
func TestGuestAndDeviceOwnersAreReserved(t *testing.T) {
	for _, id := range []*Identity{
		{Provider: ProviderProxy, Subject: "guest"},
		{Provider: ProviderProxy, Subject: "device:yuvraj"},
		{Subject: "Guest"},
		{Subject: "device:yuvraj"},
	} {
		if got := id.Owner(); got == Guest || got == "guest" || got == "device:yuvraj" {
			t.Errorf("%+v owns %q", id, got)
		}
	}
}

func TestGuestPrefixIsReserved(t *testing.T) {
	if got := (&Identity{Provider: ProviderProxy, Subject: "guest:abc"}).Owner(); got == "guest:abc" {
		t.Errorf("a server identity owns %q", got)
	}
}
