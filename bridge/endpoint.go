package bridge

import "net/netip"

// ParseLoopbackEndpoint validates an IP-literal Bridge endpoint and returns its normalized address.
func ParseLoopbackEndpoint(host string, port int) (netip.AddrPort, error) {
	address, err := netip.ParseAddr(host)
	if err != nil {
		return netip.AddrPort{}, errorCode(CodeInvalidEndpoint)
	}

	address = address.Unmap()
	if !address.IsLoopback() {
		return netip.AddrPort{}, errorCode(CodeInvalidEndpoint)
	}

	if port < 1 || port > 65535 {
		return netip.AddrPort{}, errorCode(CodeInvalidEndpoint)
	}

	return netip.AddrPortFrom(address, uint16(port)), nil
}
