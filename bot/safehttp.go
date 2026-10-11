package bot

// Outbound HTTP that refuses to touch the local network.
//
// The bot downloads whatever URL a message in an allowed channel names —
// links.go's directRegexp accepts ANY host with a media extension — so without
// this a channel member could point it at loopback, RFC1918 or link-local
// addresses (the PocketBase API itself, a router admin page, cloud metadata
// endpoints) and have the response ingested as content. The check runs in the
// dialer's Control hook, i.e. against the RESOLVED address of every connection
// including redirect hops, so a public hostname that resolves — or rebinds —
// to a private IP is refused at the socket, not at the URL.

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

var errPrivateAddress = fmt.Errorf("destination resolves to a private or local address")

// nonPublicRanges are reserved ranges the net.IP predicates below don't cover:
// carrier-grade NAT (also where Tailscale and similar VPNs hand out addresses),
// benchmarking, and the old class E block. None is reachable public internet.
var nonPublicRanges = func() []*net.IPNet {
	var out []*net.IPNet
	for _, cidr := range []string{"100.64.0.0/10", "198.18.0.0/15", "240.0.0.0/4"} {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}()

// blockNonPublic rejects dials to anything that isn't a public unicast address.
func blockNonPublic(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("unparseable dial address %q", host)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("%w: %s", errPrivateAddress, ip)
	}
	for _, n := range nonPublicRanges {
		if n.Contains(ip) {
			return fmt.Errorf("%w: %s", errPrivateAddress, ip)
		}
	}
	return nil
}

// publicOnlyTransport is a stock transport whose every dial goes through
// blockNonPublic. Both outbound clients (media downloads, imgur album pages)
// share the shape; each keeps its own client for its own timeout.
func publicOnlyTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   blockNonPublic,
	}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}
