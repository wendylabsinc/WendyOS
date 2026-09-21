package rtps

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

func loopbackInterface(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			return iface.Name
		}
	}
	t.Fatal("no loopback interface")
	return ""
}

func wireParticipant(t *testing.T) *Participant {
	t.Helper()
	p := newParticipant(Config{DomainID: 200, Interface: loopbackInterface(t)})
	p.prefix = GUIDPrefix{1, 15, 42}
	p.local = net.IPv4(127, 0, 0, 1)
	var err error
	p.ucast, err = net.ListenUDP("udp4", &net.UDPAddr{IP: p.local})
	if err != nil {
		t.Fatal(err)
	}
	iface, _ := net.InterfaceByName(p.cfg.Interface)
	if err := ipv4.NewPacketConn(p.ucast).SetMulticastInterface(iface); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func listener(t *testing.T) (*net.UDPConn, Locator) {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, udpv4Locator(net.IPv4(127, 0, 0, 1), c.LocalAddr().(*net.UDPAddr).Port)
}

func spdpData(prefix GUIDPrefix, lease uint32, locators ...Locator) *DataSubmessage {
	b := newPLBuilder()
	b.addGUID(pidParticipantGUID, GUID{Prefix: prefix, EntityID: entityParticipant})
	b.addDuration(pidParticipantLeaseDuration, lease, 0)
	for _, loc := range locators {
		b.addLocator(pidMetatrafficUnicastLocator, loc)
	}
	return &DataSubmessage{WriterID: entitySPDPWriter, WriterSN: 1, Payload: b.finish()}
}

func publicationData(ep Endpoint, sn SequenceNumber) *DataSubmessage {
	b := newPLBuilder()
	b.addGUID(pidEndpointGUID, ep.GUID)
	b.addString(pidTopicName, ep.Topic)
	b.addString(pidTypeName, ep.Type)
	return &DataSubmessage{WriterID: entitySEDPPubWriter, WriterSN: sn, Payload: b.finish()}
}

func TestSPDPCooldownConcurrentAndLocatorRefresh(t *testing.T) {
	p := wireParticipant(t)
	_, a := listener(t)
	_, b := listener(t)
	peer := GUIDPrefix{1, 15, 99} // external Fast DDS remains discoverable
	d := spdpData(peer, 90, a, a, Locator{Kind: 1, Port: 0}, b)
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() { p.handleSPDP(peer, d) })
	}
	wg.Wait()
	stats := p.Stats()
	if stats.SPDPRepliesSent != 2 || stats.SPDPRepliesSuppressed != 99 {
		t.Fatalf("stats = %+v", stats)
	}
	p.handleSPDP(peer, spdpData(peer, 90, b))
	if len(p.peers[peer]) != 1 || p.peers[peer][0] != b {
		t.Fatal("suppressed reply failed to refresh locators")
	}
	p.mu.Lock()
	p.lastReply[peer] = time.Now().Add(-announceInterval)
	p.mu.Unlock()
	p.handleSPDP(peer, spdpData(peer, 90, b))
	if p.Stats().SPDPRepliesSent != 3 {
		t.Fatal("cooldown did not expire")
	}
}

func TestSPDPEchoIsBounded(t *testing.T) {
	p := wireParticipant(t)
	peer, loc := listener(t)
	prefix := GUIDPrefix{1, 16, 99} // CycloneDDS
	d := spdpData(prefix, 90, loc)
	p.handleSPDP(prefix, d)
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	if _, _, err := peer.ReadFromUDP(buf); err != nil {
		t.Fatal(err)
	}
	// The remote answers our reply immediately; that answer must end the exchange.
	p.handleSPDP(prefix, d)
	_ = peer.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, _, err := peer.ReadFromUDP(buf); err == nil {
		t.Fatal("echo provoked another reply")
	}
	if p.Stats().SPDPRepliesSent != 1 {
		t.Fatal("unbounded replies")
	}
}

func TestLocalGUIDRegistryDoesNotFilterVendors(t *testing.T) {
	p := wireParticipant(t)
	local, err := NewParticipant(Config{DomainID: 201, Interface: loopbackInterface(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	_, loc := listener(t)
	for _, prefix := range []GUIDPrefix{p.prefix, local.prefix, {1, 15, 89}, {1, 16, 90}} {
		d := spdpData(prefix, 90, loc)
		p.handle(buildMessage(prefix, buildData(entitySPDPReader, entitySPDPWriter, 1, d.Payload)), nil)
	}
	if stats := p.Stats(); stats.LocalMessagesIgnored != 2 || stats.PeersSeen != 2 {
		t.Fatalf("stats = %+v", stats)
	}
	if _, ok := localParticipants.Load(local.prefix); !ok {
		t.Fatal("participant was not registered")
	}
	_ = local.Close()
	if _, ok := localParticipants.Load(local.prefix); ok {
		t.Fatal("closed participant retained its GUID")
	}
}

func TestEndpointExpiryDisposalAndLeaseAdvertisement(t *testing.T) {
	p := wireParticipant(t)
	_, loc := listener(t)
	prefix := GUIDPrefix{9}
	ep := Endpoint{GUID: GUID{Prefix: prefix, EntityID: 0x102}, Topic: "rt/battery", Type: "BatteryState"}
	p.handleSPDP(prefix, spdpData(prefix, 1, loc))
	p.handleSEDPPublication(publicationData(ep, 1))
	if len(p.Endpoints()) != 1 {
		t.Fatal("endpoint missing")
	}
	p.mu.Lock()
	p.expirePeersLocked(time.Now().Add(2 * time.Second))
	p.mu.Unlock()
	if len(p.Endpoints()) != 0 {
		t.Fatal("expired endpoint retained")
	}
	p.handleSEDPPublication(publicationData(ep, 2))
	msg, _ := ParseMessage(buildMessage(prefix, buildDispose(entitySEDPPubWriter, 3, ep.GUID)))
	p.handle(buildMessage(prefix, buildDispose(entitySEDPPubWriter, 3, ep.GUID)), nil)
	d, _ := ParseData(msg.Submessages[0])
	if disposed, g := discoveryDisposal(d); !disposed || g != ep.GUID {
		t.Fatalf("invalid disposal: %v %v", disposed, g)
	}
	p.handleSEDPPublication(publicationData(ep, 2))
	if len(p.Endpoints()) != 0 {
		t.Fatal("disposed endpoint replay resurrected publisher")
	}
	p.handleSEDPPublication(publicationData(ep, 4))
	if len(p.Endpoints()) != 1 {
		t.Fatal("newer publication not accepted")
	}
	msg, _ = ParseMessage(p.spdpDatagram())
	d, _ = ParseData(msg.Submessages[0])
	params, order, _ := parseParameterList(d.Payload)
	for _, prm := range params {
		if prm.id == pidParticipantLeaseDuration && order.Uint32(prm.value[:4]) == 90 {
			return
		}
	}
	t.Fatal("SPDP must advertise a 90 second lease")
}

func TestDisposalSerializedKeyAndBigEndianInline(t *testing.T) {
	guid := GUID{Prefix: GUIDPrefix{1, 16, 8}, EntityID: 0x102}
	key := newPLBuilder()
	key.addGUID(pidEndpointGUID, guid)
	qos := []byte{0, 0x71, 0, 4, 0, 0, 0, 1, 0, 1, 0, 0}
	if disposed, g := discoveryDisposal(&DataSubmessage{Endian: binary.BigEndian, InlineQoS: qos, Payload: key.finish()}); !disposed || g != guid {
		t.Fatalf("got %v %v", disposed, g)
	}
}

func TestLoopbackOnlyPeerFindsStandardUnicastPort(t *testing.T) {
	const domain = 210
	first := spdpMulticastPort(domain) + 10
	occupied, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: first})
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	p, err := NewParticipant(Config{DomainID: domain, Interface: loopbackInterface(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	addr := p.ucast.LocalAddr().(*net.UDPAddr)
	if addr.Port <= first || addr.Port >= first+200 || (addr.Port-first)%2 != 0 {
		t.Fatalf("not a standard discovery port: %s", addr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	remote, loc := listener(t)
	prefix := GUIDPrefix{1, 16, 11}
	d := spdpData(prefix, 90, loc)
	packet := buildMessage(prefix, buildData(entitySPDPReader, entitySPDPWriter, 1, d.Payload))
	if _, err := remote.WriteToUDP(packet, addr); err != nil {
		t.Fatal(err)
	}
	_ = remote.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 2048)
	n, _, err := remote.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := ParseMessage(buf[:n])
	if err != nil || msg.Prefix != p.prefix {
		t.Fatalf("loopback discovery response: %v %+v", err, msg)
	}
}

func TestSPDPShortLeaseCannotBypassCooldown(t *testing.T) {
	p := wireParticipant(t)
	_, loc := listener(t)
	prefix := GUIDPrefix{1, 16, 12}
	d := spdpData(prefix, 1, loc)
	p.handleSPDP(prefix, d)
	p.mu.Lock()
	p.expirePeersLocked(time.Now().Add(2 * time.Second))
	p.mu.Unlock()
	p.handleSPDP(prefix, d)
	if st := p.Stats(); st.SPDPRepliesSent != 1 || st.SPDPRepliesSuppressed != 1 {
		t.Fatalf("short lease bypassed cooldown: %+v", st)
	}
	invalid := *d
	invalid.Payload = d.Payload[:len(d.Payload)-4]
	p.handleSPDP(GUIDPrefix{9}, &invalid)
	if p.Stats().SPDPRepliesSent != 1 {
		t.Fatal("malformed announcement got a reply")
	}
}

func TestPeerLeaseIsRenewedByTrafficFromThePeer(t *testing.T) {
	// CycloneDDS advertises a 10 s lease and re-announces every few seconds.
	// Two lost SPDP datagrams must not tear down a stream whose frames are
	// still arriving: like Cyclone, any message from the peer renews it.
	p := wireParticipant(t)
	_, loc := listener(t)
	prefix := GUIDPrefix{9}
	ep := Endpoint{GUID: GUID{Prefix: prefix, EntityID: 0x102}, Topic: "rt/camera", Type: "Image"}
	p.handleSPDP(prefix, spdpData(prefix, 10, loc))
	p.handleSEDPPublication(publicationData(ep, 1))
	// The last announcement has lapsed.
	p.mu.Lock()
	p.peerExpiry[prefix] = time.Now().Add(-time.Second)
	p.mu.Unlock()
	p.handle(buildMessage(prefix, buildData(entityUnknown, ep.GUID.EntityID, 1, []byte{0, 1, 0, 0, 42})), nil)
	if len(p.Endpoints()) != 1 {
		t.Fatal("user data from a live peer did not renew its lease")
	}
	p.mu.Lock()
	expiry := p.peerExpiry[prefix]
	p.mu.Unlock()
	if remaining := time.Until(expiry); remaining < 9*time.Second || remaining > 10*time.Second {
		t.Fatalf("renewal must use the advertised 10 s lease, got %v", remaining)
	}
}
