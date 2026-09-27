package clitimesync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
	"golang.org/x/net/ipv4"
)

const (
	multicastAddr = "239.255.87.84:5887"
	multicastTTL  = 1
)

// roughtimeQueryFn is indirected for tests.
var roughtimeQueryFn = roughtime.Query

// Process-lifetime cache (no TTL): the CLI is one-shot, so one proof per run.
var (
	proofMu     sync.Mutex
	proofPkt    []byte
	proofResult roughtime.Result
	proofCached bool
	// proofFromDisk records that the proof in use came from the on-disk cache
	// rather than a live query, and proofAge how old it was when read, so callers
	// can report both.
	proofFromDisk bool
	proofAge      time.Duration
)

// ProofFromCache reports whether the proof this run broadcast came from the
// on-disk cache instead of a live Roughtime query, and how old it was when read.
func ProofFromCache() (bool, time.Duration) {
	proofMu.Lock()
	defer proofMu.Unlock()
	return proofFromDisk, proofAge
}

// resetProofCache clears the per-process proof cache (test helper).
func resetProofCache() {
	proofMu.Lock()
	defer proofMu.Unlock()
	proofPkt, proofResult, proofCached, proofFromDisk, proofAge = nil, roughtime.Result{}, false, false, 0
}

// FetchProofPacket queries a Roughtime server and returns the encoded
// WendyDatagram packet plus the raw result. The result is memoized for the
// life of the process so fixing several devices in one CLI run issues at most
// one Roughtime query.
func FetchProofPacket(ctx context.Context) ([]byte, roughtime.Result, error) {
	proofMu.Lock()
	defer proofMu.Unlock()
	if proofCached {
		return proofPkt, proofResult, nil
	}
	result, err := roughtimeQueryFn(ctx, timesync.Servers)
	if err != nil {
		// No route to a Roughtime server. Fall back to the proof kept from a run
		// that did have one: it is signed by the same servers, and the agent only
		// ever advances its clock, so an out-of-date proof cannot make things
		// worse than having none.
		//
		// Not when the caller abandoned the operation: relaying a time nobody asked
		// for any more is not a rescue. A deadline expiring is the opposite case —
		// servers that do not answer consume the whole budget, so this is how "no
		// route" usually presents, and refusing the cache here would disable the
		// fallback exactly where it is needed.
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, roughtime.Result{}, fmt.Errorf("roughtime query: %w", err)
		}
		cached, fetchedAt, cacheErr := loadProof()
		if cacheErr != nil {
			return nil, roughtime.Result{}, fmt.Errorf("roughtime query: %w", err)
		}
		pkt, encErr := encodeProofPacket(cached)
		if encErr != nil {
			return nil, roughtime.Result{}, fmt.Errorf("roughtime query: %w (cached proof unusable: %v)", err, encErr)
		}
		proofPkt, proofResult, proofCached = pkt, cached, true
		proofFromDisk, proofAge = true, time.Since(fetchedAt)
		return proofPkt, proofResult, nil
	}
	pkt, err := encodeProofPacket(result)
	if err != nil {
		return nil, roughtime.Result{}, err
	}
	proofPkt, proofResult, proofCached = pkt, result, true
	// Best-effort: keep it for a future run that cannot reach a server.
	_ = storeProof(result, time.Now())
	return proofPkt, proofResult, nil
}

// cacheProofTimeout bounds the post-login proof fetch. Nothing waits on the
// result, so a host that cannot reach a Roughtime server must not hold up the
// command that triggered it.
const cacheProofTimeout = 5 * time.Second

// CacheProof fetches a proof and stores it for later offline use, discarding any
// error. Called where the host is known to have working cloud access — notably
// straight after issuing a certificate, which pairs the proof with that cert: it
// asserts a time at or after the cert's NotBefore, which is the advance a device
// behind that cert needs.
func CacheProof(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, cacheProofTimeout)
	defer cancel()
	_, _, _ = FetchProofPacket(ctx)
}

// encodeProofPacket builds the WendyDatagram packet the agent verifies. The wire
// format names the server by its index in timesync.Servers, so a result naming a
// server this build does not know cannot be encoded — silently sending index 0
// would have the device verify against the wrong key.
func encodeProofPacket(result roughtime.Result) ([]byte, error) {
	serverIdx := -1
	for i, s := range timesync.Servers {
		if s.Name == result.Server {
			serverIdx = i
			break
		}
	}
	if serverIdx < 0 {
		return nil, fmt.Errorf("unknown roughtime server %q", result.Server)
	}
	payload := roughtime.EncodeRoughtimePayload(roughtime.RoughtimePayload{
		ServerIndex: uint8(serverIdx), //nolint:gosec — bounded by len(timesync.Servers)
		Nonce:       result.Nonce,
		Response:    result.RawResponse,
	})
	return roughtime.Encode(roughtime.Datagram{
		MsgType: roughtime.MsgTypeRoughtime,
		Payload: payload,
	}), nil
}

// BroadcastTime fetches a Roughtime proof and sends it as a WendyDatagram on
// every path an agent may be listening on: IPv4 multicast and IPv6 link-local
// multicast on all active multicast-capable interfaces, plus unicast to the
// well-known USB device address on each USB link. Individual send errors are
// skipped, but an error is returned when nothing was sent so callers never
// report a broadcast that did not happen.
func BroadcastTime(ctx context.Context) (roughtime.Result, error) {
	pkt, result, err := FetchProofPacket(ctx)
	if err != nil {
		return roughtime.Result{}, err
	}
	v4Err := sendMulticast(pkt)
	v6Err := sendUDP6(pkt)
	if v4Err != nil && v6Err != nil {
		return roughtime.Result{}, errors.Join(v4Err, v6Err)
	}
	return result, nil
}

type multicastPacketConn interface {
	SetMulticastInterface(*net.Interface) error
	SetMulticastTTL(int) error
	WriteTo([]byte, *ipv4.ControlMessage, net.Addr) (int, error)
	Close() error
}

var (
	listMulticastInterfaces = net.Interfaces
	newMulticastPacketConn  = func() (multicastPacketConn, error) {
		conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
		if err != nil {
			return nil, err
		}
		return ipv4.NewPacketConn(conn), nil
	}
)

func sendMulticast(pkt []byte) error {
	dst, err := net.ResolveUDPAddr("udp4", multicastAddr)
	if err != nil {
		return fmt.Errorf("resolving multicast destination: %w", err)
	}
	ifaces, err := listMulticastInterfaces()
	if err != nil {
		return fmt.Errorf("listing network interfaces: %w", err)
	}

	sent := 0
	var failures []error
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 ||
			iface.Flags&net.FlagLoopback != 0 ||
			iface.Flags&net.FlagMulticast == 0 {
			continue
		}

		conn, err := newMulticastPacketConn()
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: opening UDP socket: %w", iface.Name, err))
			continue
		}

		// Setting the outgoing interface is essential on multi-homed hosts. A
		// wildcard UDP dial follows only the default multicast route, causing
		// every iteration to leave through the same interface (for example Wi-Fi)
		// even when the Wendy device is attached over USB Ethernet.
		if err := conn.SetMulticastInterface(&iface); err != nil {
			failures = append(failures, fmt.Errorf("%s: selecting multicast interface: %w", iface.Name, err))
			_ = conn.Close()
			continue
		}
		if err := conn.SetMulticastTTL(multicastTTL); err != nil {
			failures = append(failures, fmt.Errorf("%s: setting multicast TTL: %w", iface.Name, err))
			_ = conn.Close()
			continue
		}
		if _, err := conn.WriteTo(pkt, nil, dst); err != nil {
			failures = append(failures, fmt.Errorf("%s: sending multicast packet: %w", iface.Name, err))
			_ = conn.Close()
			continue
		}
		sent++
		_ = conn.Close()
	}

	if sent > 0 {
		return nil
	}
	if len(failures) > 0 {
		return fmt.Errorf("broadcasting time proof: %w", errors.Join(failures...))
	}
	return errors.New("broadcasting time proof: no active multicast-capable network interfaces")
}

// udp6Writer is the subset of a UDP6 socket sendUDP6 needs; an interface so
// tests can record sends without real sockets.
type udp6Writer interface {
	WriteTo([]byte, net.Addr) (int, error)
	Close() error
}

var (
	newUDP6Conn = func() (udp6Writer, error) {
		return net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified})
	}
	usbDirectCandidatesFn = discovery.USBDirectCandidates
)

// sendUDP6 sends pkt to the IPv6 link-local time group on every active
// multicast interface and unicast to the well-known device address on every
// USB link. IPv4 multicast alone is unreliable over USB: an agent that joined
// the group only on its default uplink never sees it, and a host source
// address that is off the device's subnet can be dropped by reverse-path
// filtering. Every USB gadget link has IPv6 link-local addressing, so these
// sends reach the device regardless. Link-local destinations carry an
// explicit zone, which also selects the outgoing interface.
func sendUDP6(pkt []byte) error {
	var dsts []*net.UDPAddr
	group := net.ParseIP(timesync.LinkLocalGroupV6)
	if ifaces, err := listMulticastInterfaces(); err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 ||
				iface.Flags&net.FlagLoopback != 0 ||
				iface.Flags&net.FlagMulticast == 0 {
				continue
			}
			dsts = append(dsts, &net.UDPAddr{IP: group, Port: timesync.DatagramPort, Zone: iface.Name})
		}
	}
	for _, cand := range usbDirectCandidatesFn() {
		dsts = append(dsts, &net.UDPAddr{IP: net.ParseIP(discovery.WellKnownUSBAddr), Port: timesync.DatagramPort, Zone: cand.Zone})
	}
	if len(dsts) == 0 {
		return errors.New("sending time proof over IPv6: no active interfaces")
	}

	conn, err := newUDP6Conn()
	if err != nil {
		return fmt.Errorf("sending time proof over IPv6: opening UDP socket: %w", err)
	}
	defer conn.Close()

	sent := 0
	var failures []error
	for _, dst := range dsts {
		if _, err := conn.WriteTo(pkt, dst); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", dst, err))
			continue
		}
		sent++
	}
	if sent > 0 {
		return nil
	}
	return fmt.Errorf("sending time proof over IPv6: %w", errors.Join(failures...))
}
