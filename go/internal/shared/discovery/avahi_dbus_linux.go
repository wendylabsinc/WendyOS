//go:build linux

package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// errAvahiUnavailable signals that the Avahi D-Bus service could not be
// reached at all — the system bus is unreachable, or nothing answers as
// org.freedesktop.Avahi (daemon not installed/running). mdnsStreamBackend
// treats this, and only this, as a cue to fall back to hashicorp/mdns. Any
// error surfacing once browsing has actually started (daemon crashes
// mid-session, a malformed reply, ...) is returned as-is so the streaming
// engine restarts this backend instead of silently downgrading it.
var errAvahiUnavailable = errors.New("avahi d-bus service unavailable")

// errAvahiConnectionLost signals that avahiBrowse's signal channel closed
// while ctx was still live — godbus closes every channel registered via
// Signal() when the underlying transport dies (a bus/daemon crash calls
// conn.Close() internally, which calls Terminate()), which looks identical
// to "the channel drained" unless ctx.Err() is checked. This is deliberately
// NOT errAvahiUnavailable: the connection existed and worked a moment ago,
// so this is restartable (the engine retries avahiStreamBackend, which
// reconnects), not a cue to fall back to hashicorp/mdns.
var errAvahiConnectionLost = errors.New("avahi d-bus connection lost")

const (
	avahiService      = "org.freedesktop.Avahi"
	avahiServerIface  = "org.freedesktop.Avahi.Server"
	avahiBrowserIface = "org.freedesktop.Avahi.ServiceBrowser"

	avahiGetVersionStringMethod   = avahiServerIface + ".GetVersionString"
	avahiServiceBrowserNewMethod  = avahiServerIface + ".ServiceBrowserNew"
	avahiResolveServiceMethod     = avahiServerIface + ".ResolveService"
	avahiServiceBrowserFreeMethod = avahiBrowserIface + ".Free"

	avahiItemNewSignal     = avahiBrowserIface + ".ItemNew"
	avahiItemRemoveSignal  = avahiBrowserIface + ".ItemRemove"
	avahiBrowserFailureSig = avahiBrowserIface + ".Failure"

	// Avahi's interface/protocol sentinel values (avahi-common/address.h).
	avahiIfUnspec    = int32(-1) // AVAHI_IF_UNSPEC: browse every interface
	avahiProtoUnspec = int32(-1) // AVAHI_PROTO_UNSPEC: either address family
	avahiProtoInet   = int32(0)  // AVAHI_PROTO_INET: IPv4 only
	avahiProtoInet6  = int32(1)  // AVAHI_PROTO_INET6: IPv6 only

	avahiDomainLocal = "local"
)

// avahiServerPath is the object path of the Avahi daemon's singleton Server
// object, which exposes GetVersionString, ServiceBrowserNew and
// ResolveService. A created ServiceBrowser gets its own, separate object path
// (returned by ServiceBrowserNew) that Free is called on.
var avahiServerPath = dbus.ObjectPath("/")

// avahiSignalBuffer bounds pending ServiceBrowser signals queued on the
// channel registered with the bus connection. Generous relative to
// probeWorkers so a burst of ItemNew replies (a network with many devices
// already up) cannot make the connection's dispatch loop block on a slow
// consumer.
const avahiSignalBuffer = 64

// avahiResolveJobBuffer bounds how many decoded ItemNew signals are queued
// for the resolve worker pool, mirroring mdnsStreamJobBuffer's role for the
// darwin backend (mdns_darwin.go).
const avahiResolveJobBuffer = 32

// avahiCallTimeout bounds the one-shot availability/setup calls
// (GetVersionString, ServiceBrowserNew): without a bound, a wedged avahi
// daemon that still owns its bus name (hung, but not gone) would block
// avahiStreamBackend indefinitely — no errAvahiUnavailable fallback for the
// probe, no restart for ServiceBrowserNew. A var, not a const, so tests can
// shrink it.
var avahiCallTimeout = 3 * time.Second

// avahiResolveTimeout bounds a single resolve attempt, including in the case
// of a first attempt that fails and gets a retry — the retry is not billed
// its own separate budget, so a wedged daemon cannot make a resolve worker's
// slot in the pool unavailable for arbitrarily long. A var, not a const, so
// tests can shrink it.
var avahiResolveTimeout = 3 * time.Second

// avahiFreeTimeout bounds the best-effort ServiceBrowser.Free cleanup call.
// It uses its own deadline, independent of the (already-cancelled) browse
// ctx, mirroring stopDiscoveryTimeout's role in bluetooth_linux.go: without
// this, calling Free with the cancelled ctx would fail instantly without the
// call ever reaching the daemon.
var avahiFreeTimeout = 3 * time.Second

// interfaceByIndexFn resolves a kernel ifindex to its *net.Interface — Avahi
// reports interfaces by the same index the kernel uses. A var, not a direct
// net.InterfaceByIndex call, so decodeResolveReply's tests can pin an
// interface name deterministically instead of depending on the test host's
// real interface table.
var interfaceByIndexFn = net.InterfaceByIndex

// avahiConn is the subset of a D-Bus system-bus connection avahiBrowse needs,
// extracted so tests can drive the whole browse/resolve/cleanup sequence with
// a scripted fake instead of a real system bus (which is unavailable in CI
// and on the darwin machine this backend is cross-compiled from). realAvahiConn
// implements it against a live *dbus.Conn.
type avahiConn interface {
	// Call invokes method ("interface.Member") on the object at path,
	// returning the reply body or an error. Used for every Avahi D-Bus call
	// this backend makes: GetVersionString and ServiceBrowserNew/
	// ResolveService (both on the Server object at avahiServerPath), and
	// ServiceBrowser.Free (on the browser's own path).
	Call(ctx context.Context, path dbus.ObjectPath, method string, args ...any) ([]any, error)

	// AddMatchSignal registers a match rule for broadcast signals on iface.
	AddMatchSignal(ctx context.Context, iface string) error

	// Signal registers ch to receive dispatched signals; RemoveSignal
	// deregisters it. Semantics match *dbus.Conn's methods of the same name.
	Signal(ch chan *dbus.Signal)
	RemoveSignal(ch chan *dbus.Signal)
}

// realAvahiConn implements avahiConn against a live *dbus.Conn.
type realAvahiConn struct {
	conn *dbus.Conn
}

func (r realAvahiConn) Call(ctx context.Context, path dbus.ObjectPath, method string, args ...any) ([]any, error) {
	call := r.conn.Object(avahiService, path).CallWithContext(ctx, method, 0, args...)
	if call.Err != nil {
		return nil, call.Err
	}
	return call.Body, nil
}

func (r realAvahiConn) AddMatchSignal(ctx context.Context, iface string) error {
	return r.conn.AddMatchSignalContext(ctx, dbus.WithMatchInterface(iface))
}

func (r realAvahiConn) Signal(ch chan *dbus.Signal) { r.conn.Signal(ch) }

func (r realAvahiConn) RemoveSignal(ch chan *dbus.Signal) { r.conn.RemoveSignal(ch) }

// connectAvahiSystemBus opens the private system-bus connection
// avahiStreamBackend browses over. A var, not a direct dbus.ConnectSystemBus
// call, so a test can force the errAvahiUnavailable path deterministically
// instead of depending on whether a D-Bus system bus happens to be reachable
// in the environment the tests run in.
var connectAvahiSystemBus = dbus.ConnectSystemBus

// avahiStreamBackend browses via the Avahi daemon's D-Bus API — no
// avahi-browse child process. It returns errAvahiUnavailable when the system
// bus or the Avahi service itself cannot be reached at all; any other error
// means browsing started and then failed, which the streaming engine
// (stream.go) restarts this backend for rather than falling back.
//
// dbus.ConnectSystemBus (a private connection this call owns end to end) is
// used rather than the package-shared dbus.SystemBus: godbus documents that
// Close must not be called on a shared connection, and this function's
// cleanup needs to close its connection deterministically on every return
// path — including the very common one where the caller's ctx never fires
// again for the life of the process.
func avahiStreamBackend(ctx context.Context, serviceType string, emit func(MDNSService)) error {
	conn, err := connectAvahiSystemBus()
	if err != nil {
		return fmt.Errorf("%w: connecting to the system bus: %v", errAvahiUnavailable, err)
	}
	defer conn.Close()

	ac := realAvahiConn{conn: conn}
	if err := probeAvahiAvailable(ctx, ac); err != nil {
		return fmt.Errorf("%w: %v", errAvahiUnavailable, err)
	}

	return avahiBrowse(ctx, ac, serviceType, emit)
}

// probeAvahiAvailable reports whether the Avahi daemon answers on the bus,
// via the cheapest call available (GetVersionString takes no arguments and
// every Avahi version has always implemented it). Bounded by avahiCallTimeout
// so a wedged daemon that still owns the bus name falls back to
// hashicorp/mdns instead of hanging avahiStreamBackend forever.
func probeAvahiAvailable(ctx context.Context, conn avahiConn) error {
	probeCtx, cancel := context.WithTimeout(ctx, avahiCallTimeout)
	defer cancel()
	_, err := conn.Call(probeCtx, avahiServerPath, avahiGetVersionStringMethod)
	return err
}

// avahiItemNew is the decoded body of an Avahi ServiceBrowser ItemNew signal:
// int32 iface, int32 proto, string name, string type, string domain (a
// trailing uint32 flags field is also present on the wire but unused here —
// YAGNI, mirroring ItemRemove).
type avahiItemNew struct {
	iface, proto        int32
	name, stype, domain string
}

// decodeItemNew decodes an ItemNew/ItemRemove signal body. Both signals share
// this shape (iface, proto, name, type, domain, flags); ItemRemove's body is
// decoded through the same function so a malformed body cannot panic, even
// though the result is discarded (avahiBrowse ignores ItemRemove — the
// engine's own offline logic owns removals, see BrowseMDNSServicesContinuous's
// doc comment for the equivalent darwin/streaming split).
func decodeItemNew(body []any) (avahiItemNew, bool) {
	if len(body) < 5 {
		return avahiItemNew{}, false
	}
	iface, ok := body[0].(int32)
	if !ok {
		return avahiItemNew{}, false
	}
	proto, ok := body[1].(int32)
	if !ok {
		return avahiItemNew{}, false
	}
	name, ok := body[2].(string)
	if !ok {
		return avahiItemNew{}, false
	}
	stype, ok := body[3].(string)
	if !ok {
		return avahiItemNew{}, false
	}
	domain, ok := body[4].(string)
	if !ok {
		return avahiItemNew{}, false
	}
	return avahiItemNew{iface: iface, proto: proto, name: name, stype: stype, domain: domain}, true
}

// singleObjectPath extracts a lone dbus.ObjectPath reply body, the shape
// ServiceBrowserNew replies with.
func singleObjectPath(body []any) (dbus.ObjectPath, bool) {
	if len(body) != 1 {
		return "", false
	}
	path, ok := body[0].(dbus.ObjectPath)
	return path, ok
}

// resolveAvahiItem resolves one ItemNew sighting into an MDNSService,
// resolving synchronously via the Server's ResolveService method (there is no
// child process, and no separate async ServiceResolver object needed — unlike
// ServiceBrowser, ResolveService blocks the D-Bus call until it has an answer
// or times out on Avahi's side).
//
// Resolve the address family on which this service was sighted. Asking for an
// IPv4 address through an IPv6-only sighting can occupy a worker until timeout;
// enough local veth/mesh interfaces would starve real remote IPv4 discoveries.
// Separate IPv4 sightings still let the stream prefer IPv4 when both arrive.
func resolveAvahiItem(ctx context.Context, conn avahiConn, item avahiItemNew) (MDNSService, bool) {
	resolveCtx, cancel := context.WithTimeout(ctx, avahiResolveTimeout)
	defer cancel()

	aprotocol := item.proto
	if aprotocol != avahiProtoInet && aprotocol != avahiProtoInet6 {
		aprotocol = avahiProtoUnspec
	}
	reply, err := conn.Call(resolveCtx, avahiServerPath, avahiResolveServiceMethod,
		item.iface, item.proto, item.name, item.stype, item.domain, aprotocol, uint32(0))
	if err != nil {
		logMDNSQueryErr(fmt.Sprintf("avahi-resolve %s interface=%d protocol=%d", item.name, item.iface, item.proto), err)
		if resolveCtx.Err() != nil || aprotocol == avahiProtoUnspec {
			return MDNSService{}, false
		}
		reply, err = conn.Call(resolveCtx, avahiServerPath, avahiResolveServiceMethod,
			item.iface, item.proto, item.name, item.stype, item.domain, avahiProtoUnspec, uint32(0))
		if err != nil {
			return MDNSService{}, false
		}
	}
	return decodeResolveReply(reply)
}

// decodeResolveReply decodes a Server.ResolveService reply:
// (i interface, i protocol, s name, s type, s domain, s host, i aprotocol,
// s address, q port, aay txt, u flags).
func decodeResolveReply(body []any) (MDNSService, bool) {
	if len(body) < 11 {
		return MDNSService{}, false
	}
	ifaceIdx, ok := body[0].(int32)
	if !ok {
		return MDNSService{}, false
	}
	name, ok := body[2].(string)
	if !ok {
		return MDNSService{}, false
	}
	host, ok := body[5].(string)
	if !ok {
		return MDNSService{}, false
	}
	address, ok := body[7].(string)
	if !ok {
		return MDNSService{}, false
	}
	port, ok := body[8].(uint16)
	if !ok {
		return MDNSService{}, false
	}
	txt, ok := body[9].([][]byte)
	if !ok {
		return MDNSService{}, false
	}

	ifaceName := ""
	if netIface, err := interfaceByIndexFn(int(ifaceIdx)); err == nil {
		ifaceName = netIface.Name
	}

	// IPv6 link-local addresses need a zone ID (%iface) to be routable.
	ipAddr := address
	if addr, err := netip.ParseAddr(ipAddr); err == nil && addr.Is6() && addr.IsLinkLocalUnicast() && ifaceName != "" {
		ipAddr = ipAddr + "%" + ifaceName
	}

	return MDNSService{
		InstanceName:  name,
		Hostname:      strings.TrimSuffix(host, "."),
		IPAddress:     ipAddr,
		Port:          int(port),
		TXTRecords:    txtFromByteSlices(txt),
		InterfaceName: ifaceName,
	}, true
}

// txtFromByteSlices decodes Avahi's [][]byte TXT record shape (one []byte per
// entry, already split by the daemon — unlike the raw length-prefixed wire
// format parseTXTRecord (mdns.go:26) decodes) into a key→value map, following
// the same RFC 6763 §6.4 rule: each entry is "key=value", or a bare key for a
// boolean attribute (mapped to an empty value); the first occurrence of a
// repeated key wins.
func txtFromByteSlices(txt [][]byte) map[string]string {
	records := make(map[string]string)
	for _, entry := range txt {
		key, value, _ := strings.Cut(string(entry), "=")
		if key == "" {
			continue
		}
		if _, exists := records[key]; !exists {
			records[key] = value
		}
	}
	return records
}

// avahiBrowse subscribes to one Avahi ServiceBrowser's ItemNew/ItemRemove
// signals and resolves each new sighting on a bounded worker pool
// (probeWorkers, the same knob stream.go uses for its own resolve/probe
// pools), emitting every resolved service until ctx is done, the daemon
// reports a Failure, or the signal channel itself closes out from under it
// (the underlying D-Bus connection died). Only ctx.Done() returns nil; both
// of the other two return a non-nil, restartable error so the streaming
// engine (stream.go) retries instead of silently going quiet for the rest of
// the session. Every resolve worker has exited and the browser has been torn
// down (best-effort) before this returns, on every path, satisfying the
// streaming backend contract (stream.go's lanBackendFn doc comment).
func avahiBrowse(ctx context.Context, conn avahiConn, serviceType string, emit func(MDNSService)) error {
	// Subscribed before the browser exists: an ItemNew for a device that is
	// already up can arrive as soon as ServiceBrowserNew's reply lands, and a
	// match rule/channel registered afterwards would race that signal.
	if err := conn.AddMatchSignal(ctx, avahiBrowserIface); err != nil {
		return fmt.Errorf("avahi: subscribing to %s signals: %w", avahiBrowserIface, err)
	}

	sigCh := make(chan *dbus.Signal, avahiSignalBuffer)
	conn.Signal(sigCh)

	browserCtx, browserCancel := context.WithTimeout(ctx, avahiCallTimeout)
	reply, err := conn.Call(browserCtx, avahiServerPath, avahiServiceBrowserNewMethod,
		avahiIfUnspec, avahiProtoUnspec, serviceType, avahiDomainLocal, uint32(0))
	browserCancel()
	if err != nil {
		conn.RemoveSignal(sigCh)
		return fmt.Errorf("avahi: creating service browser for %s: %w", serviceType, err)
	}
	// Logged here rather than at the call site in mdns_linux.go: only a browser
	// the daemon actually created proves this is the backend that ran.
	logMDNSBackend("avahi-dbus") // WENDY_MDNS_DEBUG: which backend is running

	browserPath, ok := singleObjectPath(reply)
	if !ok {
		conn.RemoveSignal(sigCh)
		return fmt.Errorf("avahi: ServiceBrowserNew for %s returned an unexpected reply: %v", serviceType, reply)
	}

	jobs := make(chan avahiItemNew, avahiResolveJobBuffer)
	var wg sync.WaitGroup
	for i := 0; i < probeWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				if svc, ok := resolveAvahiItem(ctx, conn, item); ok {
					emit(svc)
				}
			}
		}()
	}

	var browseErr error
readLoop:
	for {
		select {
		case <-ctx.Done():
			break readLoop
		case sig, chOK := <-sigCh:
			if !chOK {
				// godbus closes every channel registered via Signal() when
				// the connection's transport dies (a bus/daemon crash calls
				// conn.Close() internally, which calls Terminate()) — this
				// can happen with ctx still live. Only ctx.Done() is a clean,
				// caller-requested stop; the channel closing on its own means
				// the D-Bus connection itself is gone and must be surfaced as
				// a restartable error, or the streaming engine would treat
				// this as a normal end-of-session and never retry.
				if ctx.Err() == nil {
					browseErr = errAvahiConnectionLost
				}
				break readLoop
			}
			// Signals from any ServiceBrowser on the bus arrive here — the
			// match rule above filters only by interface, not by object path
			// (Avahi does not expose the browser's path as a match key) — so
			// anything not from this browse's own object is not ours.
			if sig.Path != browserPath {
				continue
			}
			switch sig.Name {
			case avahiItemNewSignal:
				item, decOK := decodeItemNew(sig.Body)
				if !decOK {
					continue
				}
				select {
				case jobs <- item:
				case <-ctx.Done():
					break readLoop
				}
			case avahiItemRemoveSignal:
				// Decoded so a malformed body cannot panic downstream, but
				// otherwise ignored: the engine's own offline logic owns
				// removals (a future enhancement could plumb this through,
				// YAGNI for now).
				decodeItemNew(sig.Body)
			case avahiBrowserFailureSig:
				// The avahi daemon itself died/restarted while the bus
				// connection stayed up (a systemd bounce, a package upgrade):
				// Avahi reports this to every ServiceBrowser via Failure
				// rather than dropping the connection, so without this case
				// the read loop would sit blocked on sigCh forever, silently
				// producing nothing. Restartable — the daemon is expected to
				// come back, and the engine's restart loop reconnects fresh.
				browseErr = fmt.Errorf("avahi: service browser failure: %s", avahiFailureMessage(sig.Body))
				break readLoop
			}
		}
	}

	close(jobs)
	wg.Wait()

	conn.RemoveSignal(sigCh)

	freeCtx, cancel := context.WithTimeout(context.Background(), avahiFreeTimeout)
	defer cancel()
	_, freeErr := conn.Call(freeCtx, browserPath, avahiServiceBrowserFreeMethod)
	logMDNSQueryErr("avahi-dbus-free", freeErr)

	return browseErr
}

// avahiFailureMessage extracts the error string from an Avahi ServiceBrowser
// Failure signal body. Failure's documented shape is a single string (the
// D-Bus error name/message), but a malformed or empty body must not panic —
// this is decoded off a live signal from a daemon that is, by definition,
// already in a bad state.
func avahiFailureMessage(body []any) string {
	if len(body) > 0 {
		if s, ok := body[0].(string); ok {
			return s
		}
	}
	return "unknown"
}
