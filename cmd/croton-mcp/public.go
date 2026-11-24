package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
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

// After IDNA mapping, a numeric final label selects the URL Standard's IPv4 parser.
var ipv4Suffix = regexp.MustCompile(`(^|\.)([0-9]+|0x[0-9a-f]*)$`)

func parseIPv4Host(host string) (netip.Addr, bool) {
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}

	var addr uint32
	for i, part := range parts {
		base := 10
		switch {
		case strings.HasPrefix(part, "0x"):
			base, part = 16, part[2:]
			if part == "" {
				part = "0"
			}
		case strings.HasPrefix(part, "0"):
			base = 8
		}

		// The final field fills the remaining octets of a shortened address.
		bits := 8
		shift := 24 - 8*i
		if i == len(parts)-1 {
			bits = 32 - 8*i
			shift = 0
		}
		n, err := strconv.ParseUint(part, base, bits)
		if err != nil {
			return netip.Addr{}, false
		}
		addr |= uint32(n) << shift
	}

	var octets [4]byte
	binary.BigEndian.PutUint32(octets[:], addr)

	return netip.AddrFrom4(octets), true
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
	// IDNA length validation expects the name without its root label.
	host = strings.NewReplacer("\u3002", ".", "\uff0e", ".", "\uff61", ".").Replace(host)
	name, rooted := strings.CutSuffix(host, ".")
	ascii, err := dns.ToASCII(name)
	if err != nil || ascii == "" || strings.HasSuffix(ascii, ".") {
		return "", invalid
	}

	if ipv4Suffix.MatchString(ascii) {
		ip, ok := parseIPv4Host(ascii)
		if !ok {
			return "", invalid
		}
		return "https://" + ip.String(), nil
	}
	if rooted {
		ascii += "."
	}

	return "https://" + ascii, nil
}
