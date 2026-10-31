package mailbox

import "testing"

func TestPublicAddressRejectsPrivateSharedAndReservedRanges(t *testing.T) {
	for _, ip := range []string{"10.1.2.3", "127.0.0.1", "100.64.1.2", "192.0.2.3", "198.51.100.2", "203.0.113.5", "169.254.1.1", "198.18.1.1", "240.1.1.1", "0.1.1.1", "::1", "2001:db8::1", "invalid"} {
		if publicV4(ip) {
			t.Fatal("not a public origin", ip)
		}
	}
	if !publicV4("8.8.8.8") {
		t.Fatal("public IPv4 rejected")
	}
}
