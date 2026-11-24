package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

// Browsers serialize IPv4-mapped IPv6 addresses as hexadecimal groups.
func httpHostname(ip netip.Addr) string {
	if ip.Is4In6() {
		v4 := ip.As4()
		return fmt.Sprintf("::ffff:%x:%x", binary.BigEndian.Uint16(v4[:2]), binary.BigEndian.Uint16(v4[2:]))
	}

	return ip.String()
}

func parsePublicOrigin(raw string) (string, error) {
	invalid := errors.New("invalid public HTTPS origin")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.ContainsAny(raw, "?#") {
		return "", invalid
	}
	if path := u.EscapedPath(); path != "" && path != "/" {
		return "", invalid
	}

	port := u.Port()
	host, hasPort := strings.CutSuffix(u.Host, ":"+port)
	if hasPort && strings.TrimLeft(port, "0") != "443" {
		return "", invalid
	}

	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		ip, err := netip.ParseAddr(host[1 : len(host)-1])
		if err != nil || !ip.Is6() || ip.Zone() != "" {
			return "", invalid
		}

		return "https://[" + httpHostname(ip) + "]", nil
	}

	dns := idna.New(idna.MapForLookup(), idna.BidiRule(), idna.VerifyDNSLength(true))
	// Preserve the root dot in a fully qualified DNS name and its HTTP Host.
	name, rooted := strings.CutSuffix(host, ".")
	ascii, err := dns.ToASCII(name)
	if err != nil || ascii == "" || strings.HasSuffix(ascii, ".") {
		return "", invalid
	}

	if rooted {
		ascii += "."
	}

	return "https://" + ascii, nil
}
