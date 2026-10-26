//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const (
	LeafALPN            = "wendy-local-mesh-leaf/3"
	LeafProtocolVersion = 3
	// LeafPort is the conventional, explicitly configured listener port.
	LeafPort           = 43024
	maxLeafConnections = 32
	maxLeafHandshakes  = 8
)

// leafNodeLink enforces a connection role, not a hardware/certificate claim.
// An enrolled identity using another enabled carrier follows that carrier's
// policy. All directory authentication and signed-route checks remain active.
type leafNodeLink struct {
	*streamNodeLink
	self, leaf meshid.Device
}

func newLeafSynchronizer(directory *Directory, cache *IdentityCache) *Synchronizer {
	sync := NewSynchronizer(directory, cache)
	sync.leafBundles = true
	return sync
}

func (l *leafNodeLink) ReadControl() (ControlMessage, error) {
	m, err := l.streamNodeLink.ReadControl()
	if err != nil {
		return m, err
	}
	switch m.Kind {
	case "manifest":
		if m.Manifest == nil || m.Manifest.Principal != l.leaf.Principal() {
			return m, errors.New("leaf may announce only its own identity")
		}
	case "bundle":
		if len(m.Bundle) == 0 || len(m.Bundle) > 16 {
			return m, errors.New("invalid leaf identity bundle")
		}
		cert, err := x509.ParseCertificate(m.Bundle[0])
		if err != nil {
			return m, err
		}
		device, err := meshid.DeviceFromCert(cert)
		if err != nil || device != l.leaf {
			return m, errors.New("leaf may supply only its own identity bundle")
		}
	case "identity-request":
		// Requests can read other identities needed for endpoint authentication;
		// they cannot install those identities or their announcements.
		device, err := meshid.DeviceFromPrincipal(m.Principal)
		if err != nil || device.Tenant() != l.leaf.Tenant() {
			return m, errors.New("leaf requested an identity outside its tenant")
		}
	default:
		return m, errors.New("unsupported leaf control message")
	}
	return m, nil
}

func (l *leafNodeLink) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	p, err := l.streamNodeLink.ReceiveDatagram(ctx)
	if err != nil || len(p) == 0 {
		return p, err
	}
	if p[0] == PacketBabel {
		_, peer, err := meshid.LinkLocalAddresses(l.self, l.leaf)
		if err != nil {
			return nil, err
		}
		if err := babel.ValidateLeafPacket(p[1:], peer, netip.PrefixFrom(l.leaf.MeshAddress(), 128)); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// admitsIP runs after bounded reassembly on ingress and before fragmentation
// on egress. Outer packets remain confined to the authenticated leaf's ULA.
// IPv4-in-IPv6 uses the Internet-sharing lease pool. The gateway independently
// authenticates its control session and enforces the assigned address/firewall.
func (l *leafNodeLink) admitsIP(packet []byte, incoming bool) bool {
	if !validIP(packet) || packet[0]>>4 != 6 {
		return false
	}
	source := netip.AddrFrom16([16]byte(packet[8:24]))
	destination := netip.AddrFrom16([16]byte(packet[24:40]))
	if !meshid.InMesh(source) || !meshid.InMesh(destination) ||
		(incoming && source != l.leaf.MeshAddress()) || (!incoming && destination != l.leaf.MeshAddress()) {
		return false
	}
	switch packet[6] {
	case 6, 17, 58: // TCP, UDP, ICMPv6; lwIP endpoint packets need no extensions.
		return true
	case 4:
		inner := packet[40:]
		if len(inner) < 20 || len(inner) > 1280 || inner[0] != 0x45 ||
			int(binary.BigEndian.Uint16(inner[2:4])) != len(inner) ||
			binary.BigEndian.Uint16(inner[6:8])&0xbfff != 0 {
			return false
		}
		// No nested tunnels, fragments or IPv4 options in the leaf profile.
		if inner[9] != 6 && inner[9] != 17 && inner[9] != 1 {
			return false
		}
		address := inner[16:20]
		if incoming {
			address = inner[12:16]
		}
		slot := binary.BigEndian.Uint16(address[2:4])
		return address[0] == 10 && address[1] == 88 && slot != 0 && slot != 65535
	default:
		return false
	}
}

// AttachLeafStream admits a fully authenticated, non-resumed leaf TLS stream.
// It owns the connection until all routing/TUN workers have drained. Neither
// the preface nor the hello substitutes for the full certificate principal pin.
func (n *Node) AttachLeafStream(ctx context.Context, peer meshid.Device, conn net.Conn) error {
	defer conn.Close()
	secure, ok := conn.(*tls.Conn)
	if !ok {
		return errors.New("leaf stream requires TLS")
	}
	state := secure.ConnectionState()
	if !state.HandshakeComplete || state.Version != tls.VersionTLS13 || state.NegotiatedProtocol != LeafALPN || state.DidResume {
		return errors.New("leaf stream requires a fresh TLS 1.3 leaf-protocol handshake")
	}
	if err := n.verifyPeerCertificates(peer, state.PeerCertificates); err != nil {
		return err
	}
	chain := make([][]byte, len(state.PeerCertificates))
	for i, cert := range state.PeerCertificates {
		chain[i] = cert.Raw
	}
	remote, err := n.Credentials.Verify(chain, time.Now())
	if err != nil {
		return err
	}
	local, err := n.Credentials.Verify(n.Credentials.Certificate.Certificate, time.Now())
	if err != nil {
		return err
	}
	expires := remote.NotAfter
	if local.NotAfter.Before(expires) {
		expires = local.NotAfter
	}
	ctx, cancel := context.WithDeadline(ctx, expires)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := OpenStreamHelloWithALPN(ctx, conn, n.Credentials.Device, peer, LeafALPN); err != nil {
		return err
	}
	link := &leafNodeLink{streamNodeLink: newBoundedStreamNodeLink(conn, 8), self: n.Credentials.Device, leaf: peer}
	defer link.Close()
	return n.attachLink(WithCarrier(ctx, carrierName(ctx, "leaf")), peer, 256, link)
}

func readLeafPreface(conn net.Conn, self meshid.Device) (meshid.Device, error) {
	var data [36]byte
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return meshid.Device{}, err
	}
	defer conn.SetReadDeadline(time.Time{})
	if _, err := io.ReadFull(conn, data[:]); err != nil {
		return meshid.Device{}, err
	}
	tenant := self.Tenant().UUID()
	if string(data[:4]) != "WML3" || !bytes.Equal(data[4:20], tenant[:]) {
		return meshid.Device{}, errors.New("invalid leaf preface")
	}
	peer, err := meshid.ParseDevice(self.Tenant(), uuid.UUID(data[20:36]).String())
	if err != nil || peer == self {
		return meshid.Device{}, errors.New("invalid leaf device identity")
	}
	return peer, nil
}

func acceptLeafTLS(ctx context.Context, credentials *Credentials, conn net.Conn) (meshid.Device, *tls.Conn, error) {
	peer, err := readLeafPreface(conn, credentials.Device)
	if err != nil {
		return peer, nil, err
	}
	config, err := credentials.PeerTLS(peer)
	if err != nil {
		return peer, nil, err
	}
	config.NextProtos = []string{LeafALPN}
	config.MinVersion, config.MaxVersion = tls.VersionTLS13, tls.VersionTLS13
	config.ClientSessionCache, config.SessionTicketsDisabled = nil, true
	secure := tls.Server(conn, config)
	if err := secure.HandshakeContext(ctx); err != nil {
		return peer, nil, err
	}
	state := secure.ConnectionState()
	if state.NegotiatedProtocol != LeafALPN || state.DidResume {
		return peer, nil, errors.New("invalid leaf TLS negotiation")
	}
	return peer, secure, nil
}

// runLeafListener owns at most 32 pending/active connections, eight concurrent
// preface/handshake workers and one admitted connection per authenticated leaf.
// Unauthenticated prefaces never reserve an identity or evict a live session.
func runLeafListener(ctx context.Context, node *Node, listener net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer workers.Wait()
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	connections, handshakes := make(chan struct{}, maxLeafConnections), make(chan struct{}, maxLeafHandshakes)
	var mu sync.Mutex
	active := make(map[meshid.Device]bool)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("leaf listener: %w", err)
		}
		select {
		case connections <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		select {
		case handshakes <- struct{}{}:
		default:
			<-connections
			_ = conn.Close()
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-connections }()
			defer conn.Close()
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			defer stop()
			handshake, done := context.WithTimeout(ctx, 8*time.Second)
			peer, secure, err := acceptLeafTLS(handshake, node.Credentials, conn)
			done()
			<-handshakes
			if err != nil {
				return
			}
			mu.Lock()
			duplicate := active[peer]
			if !duplicate {
				active[peer] = true
			}
			mu.Unlock()
			if duplicate {
				return
			}
			defer func() { mu.Lock(); delete(active, peer); mu.Unlock() }()
			_ = node.AttachLeafStream(ctx, peer, secure)
		}()
	}
}
