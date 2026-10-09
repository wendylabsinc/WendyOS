// Package linklocal reaches IPv4 link-local devices over the USB link they are
// on. The kernel routes all of 169.254/16 out one interface, which is wrong as
// soon as several links carry link-local addresses (one per tethered device),
// or when the route sits on an interface without one (macOS's primary
// interface), so sockets are pinned to the device's link instead.
// Strict reverse-path filtering (rp_filter=1) still drops the replies.
package linklocal

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"syscall"
)

// link is a host interface carrying an IPv4 link-local address.
type link struct {
	ifi net.Interface
	ip  net.IP
}

// bindError marks a socket that could not be pinned to an interface, as
// opposed to a device that did not answer.
type bindError struct{ err error }

func (e *bindError) Error() string { return "binding socket to interface: " + e.err.Error() }
func (e *bindError) Unwrap() error { return e.err }

// Seams for tests; production never reassigns them.
var (
	linksFn     = systemLinks
	plainDialFn = func(ctx context.Context, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	linkDialFn     = dialOnLink
	linkListenFn   = listenOnLink
	routedSourceFn = routedSource
)

// Dial connects to addr over TCP. An IPv4 link-local literal is dialed over
// every link-local link at once, plus the routing table's own path when that
// leaves through another interface, keeping the first to reach the device. It
// stays a plain dial when the routing table already uses the only link.
func Dial(ctx context.Context, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return plainDialFn(ctx, addr)
	}
	links, routed := linksFor(host)
	if links == nil {
		return plainDialFn(ctx, addr)
	}
	conn, _, err := dialLinks(ctx, addr, links, routed)
	return conn, err
}

// SourceIP returns this host's address on the link where host answers on the
// first of ports, connecting or refusing. ok is false when the routing table's
// own path is the right one, or nothing reaches host.
func SourceIP(ctx context.Context, host string, ports ...int) (ip string, ok bool) {
	links, routed := linksFor(host)
	if links == nil {
		return "", false
	}
	for _, port := range ports {
		conn, on, err := dialLinks(ctx, net.JoinHostPort(host, strconv.Itoa(port)), links, routed)
		if conn != nil {
			conn.Close()
		}
		if on != nil {
			return on.ip.String(), true
		}
		// The routing table's own path answered, or no path reaches host at
		// all, which no other port can change.
		if reached(err) || !informative(err) {
			return "", false
		}
	}
	return "", false
}

// dialLinks dials addr over every link at once, plus the routing table's own
// path when routed is nil because it leaves through another interface. The
// first to reach the device wins; on is nil when that was the routing table's
// path or nothing did.
func dialLinks(ctx context.Context, addr string, links []link, routed *link) (conn net.Conn, on *link, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		on   *link
		conn net.Conn
		err  error
	}
	paths := len(links)
	if routed == nil {
		paths++ // the routing table's own path
	}
	results := make(chan result, paths)
	dialOn, dialPlain := linkDialFn, plainDialFn // read once: dials may outlive dialLinks
	for _, l := range links {
		go func() {
			conn, err := dialOn(ctx, l, addr)
			results <- result{&l, conn, err}
		}()
	}
	if routed == nil {
		go func() {
			conn, err := dialPlain(ctx, addr)
			results <- result{nil, conn, err}
		}()
	}

	var errs []error
	routedUnpinned := false
	for range paths {
		r := <-results
		if reached(r.err) {
			go func(n int) {
				for range n {
					if late := <-results; late.conn != nil {
						late.conn.Close()
					}
				}
			}(paths - len(errs) - 1)
			return r.conn, r.on, r.err
		}
		errs = append(errs, r.err)
		routedUnpinned = routedUnpinned || routed != nil && r.on.ip.Equal(routed.ip) && isBindError(r.err)
	}
	// The routing table has not tried its own link if that could not be pinned.
	if routedUnpinned {
		conn, err := dialPlain(ctx, addr)
		return conn, nil, err
	}
	return nil, nil, dialFailure(errs)
}

// dialFailure picks the most telling error: one about the device, then a
// dialed path's "no route", and a pinning failure only if nothing was dialed.
func dialFailure(errs []error) error {
	for _, telling := range []func(error) bool{informative, func(err error) bool { return !isBindError(err) }} {
		if i := slices.IndexFunc(errs, telling); i >= 0 {
			return errs[i]
		}
	}
	return errs[0]
}

// reached reports whether a dial got through to the device: a connection or a
// refusal both prove it is on that path.
func reached(err error) bool {
	return err == nil || isAny(err, refusedErrnos)
}

// informative reports whether err says something about the device, rather than
// that it is not on that path or that the path could not be pinned.
func informative(err error) bool {
	return !isBindError(err) && !isAny(err, noRouteErrnos)
}

func isAny(err error, errnos []syscall.Errno) bool {
	return slices.ContainsFunc(errnos, func(errno syscall.Errno) bool { return errors.Is(err, errno) })
}

// Listen listens on address. A listener on a link's own address is pinned to
// that link, so replies leave through it rather than through whichever
// interface holds the 169.254/16 route.
func Listen(ctx context.Context, address string) (net.Listener, error) {
	if host, _, err := net.SplitHostPort(address); err == nil && IsIPv4LinkLocal(host) {
		ip := net.ParseIP(host)
		for _, l := range linksFn() {
			if l.ip.Equal(ip) {
				return listenPinned(ctx, l, address)
			}
		}
	}
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", address)
}

// listenPinned listens on address pinned to l, or unpinned where l cannot be
// pinned.
func listenPinned(ctx context.Context, l link, address string) (net.Listener, error) {
	ln, err := linkListenFn(ctx, l, address)
	if isBindError(err) {
		var lc net.ListenConfig
		return lc.Listen(ctx, "tcp", address)
	}
	return ln, err
}

// IsIPv4LinkLocal reports whether host is an IPv4 link-local literal, the only
// kind of address Dial and Listen treat specially.
func IsIPv4LinkLocal(host string) bool {
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Is4() && ip.IsLinkLocalUnicast()
}

// linksFor returns the links to dial host over, and the one the routing table
// sends host out of; routed is nil when that is some other interface. links is
// nil when host is not link-local, or the routing table already uses the only
// link.
func linksFor(host string) (links []link, routed *link) {
	if !IsIPv4LinkLocal(host) {
		return nil, nil
	}
	links = linksFn()
	source := routedSourceFn(host)
	if i := slices.IndexFunc(links, func(l link) bool { return l.ip.Equal(source) }); i >= 0 {
		routed = &links[i]
	}
	if len(links) == 0 || len(links) == 1 && routed != nil {
		return nil, nil
	}
	return links, routed
}

// routedSource returns the source address the routing table picks for host.
// Connecting a UDP socket sends nothing.
func routedSource(host string) net.IP {
	conn, err := net.Dial("udp4", net.JoinHostPort(host, "9"))
	if err != nil {
		return nil
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP
}

func isBindError(err error) bool {
	var be *bindError
	return errors.As(err, &be)
}

func systemLinks() []link {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	return linkLocalLinksFrom(ifaces, (*net.Interface).Addrs)
}

// linkLocalLinksFrom keeps the up, non-loopback interfaces that carry an IPv4
// link-local address on a link, not as a /32 host address (k8s node-local DNS).
func linkLocalLinksFrom(ifaces []net.Interface, addrsOf func(*net.Interface) ([]net.Addr, error)) []link {
	var out []link
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := addrsOf(&ifi)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ones, bits := ipn.Mask.Size(); ones == bits {
					continue
				}
				if ip4 := ipn.IP.To4(); ip4 != nil && ip4.IsLinkLocalUnicast() {
					out = append(out, link{ifi: ifi, ip: ip4})
					break
				}
			}
		}
	}
	return out
}

func dialOnLink(ctx context.Context, l link, addr string) (net.Conn, error) {
	d := net.Dialer{Control: pinTo(l)}
	return d.DialContext(ctx, "tcp4", addr)
}

func listenOnLink(ctx context.Context, l link, address string) (net.Listener, error) {
	lc := net.ListenConfig{Control: pinTo(l)}
	return lc.Listen(ctx, "tcp4", address)
}

// pinTo returns a socket Control hook that binds the socket to l's interface.
func pinTo(l link) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var bindErr error
		if err := c.Control(func(fd uintptr) { bindErr = bindToInterface(fd, l.ifi) }); err != nil {
			return &bindError{err: err}
		}
		if bindErr != nil {
			return &bindError{err: bindErr}
		}
		return nil
	}
}
