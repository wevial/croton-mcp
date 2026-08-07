package bridge_test

import (
	"net/netip"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
)

func TestParseLoopbackEndpointAcceptsOnlyNormalizedLoopbackLiterals(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		host string
		want netip.Addr
	}{
		{name: "IPv4 loopback", host: "127.0.0.1", want: netip.MustParseAddr("127.0.0.1")},
		{name: "IPv6 loopback", host: "::1", want: netip.MustParseAddr("::1")},
		{name: "mapped IPv4 loopback", host: "::ffff:127.0.0.1", want: netip.MustParseAddr("127.0.0.1")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			endpoint, err := bridge.ParseLoopbackEndpoint(test.host, 1143)
			if err != nil {
				t.Fatalf("ParseLoopbackEndpoint(%q): %v", test.host, err)
			}
			if got := endpoint.Addr(); got != test.want {
				t.Fatalf("endpoint address = %s, want %s", got, test.want)
			}
		})
	}

	for _, host := range []string{
		"bridge.test",
		"0.0.0.0",
		"::",
		"192.168.1.2",
		"100.64.0.2",
		"198.51.100.2",
		"169.254.1.2",
		"fe80::1",
		"2001:db8::1",
		"::ffff:198.51.100.2",
	} {
		if _, err := bridge.ParseLoopbackEndpoint(host, 1143); err == nil {
			t.Errorf("ParseLoopbackEndpoint(%q) unexpectedly succeeded", host)
		}
	}
}
