package rtps

import (
	"net"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

const (
	testTopic = "rt/sensorlink/porch/temperature"
	testType  = "sensor_msgs::msg::dds_::Temperature_"
)

// publishingParticipant is wireParticipant with publishing switched on.
func publishingParticipant(t *testing.T) *Participant {
	t.Helper()
	p := newParticipant(Config{DomainID: 200, Interface: loopbackInterface(t), Publish: true})
	p.prefix = GUIDPrefix{1, 15, 43}
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

// received is one submessage read from a test socket, already parsed.
type received struct {
	sub  Submessage
	data *DataSubmessage // set for DATA
}

// readSubmessages reads datagrams from conn until it has been quiet for a
// moment, and returns every submessage in arrival order.
func readSubmessages(t *testing.T, conn *net.UDPConn) []received {
	t.Helper()
	var out []received
	buf := make([]byte, 65536)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return out
		}
		msg, err := ParseMessage(append([]byte(nil), buf[:n]...))
		if err != nil {
			t.Fatalf("unparseable datagram: %v", err)
		}
		for _, s := range msg.Submessages {
			r := received{sub: s}
			if s.Kind == subDATA {
				if r.data, err = ParseData(s); err != nil {
					t.Fatalf("unparseable DATA: %v", err)
				}
			}
			out = append(out, r)
		}
	}
}

// ofKind keeps the submessages of one kind sent by one writer entity.
func ofKind(subs []received, kind uint8, writer uint32) []received {
	var out []received
	for _, r := range subs {
		if r.sub.Kind != kind {
			continue
		}
		switch kind {
		case subDATA:
			if r.data.WriterID == writer {
				out = append(out, r)
			}
		case subHEARTBEAT:
			if hb, err := ParseHeartbeat(r.sub); err == nil && hb.WriterID == writer {
				out = append(out, r)
			}
		default:
			out = append(out, r)
		}
	}
	return out
}

// addPeer makes prefix a known participant whose discovery traffic goes to
// meta and whose readers, unless they say otherwise, receive data at user.
func addPeer(t *testing.T, p *Participant, prefix GUIDPrefix, meta *net.UDPConn, metaLoc Locator, user ...Locator) {
	t.Helper()
	b := newPLBuilder()
	b.addGUID(pidParticipantGUID, GUID{Prefix: prefix, EntityID: entityParticipant})
	b.addDuration(pidParticipantLeaseDuration, 90, 0)
	b.addLocator(pidMetatrafficUnicastLocator, metaLoc)
	for _, loc := range user {
		b.addLocator(pidDefaultUnicastLocator, loc)
	}
	p.handleSPDP(prefix, &DataSubmessage{WriterID: entitySPDPWriter, WriterSN: 1, Payload: b.finish()})
}

func builtinEndpointSet(t *testing.T, p *Participant) uint32 {
	t.Helper()
	msg, err := ParseMessage(p.spdpDatagram())
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseData(msg.Submessages[0])
	if err != nil {
		t.Fatal(err)
	}
	params, order, err := parseParameterList(d.Payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, prm := range params {
		if prm.id == pidBuiltinEndpointSet {
			return order.Uint32(prm.value)
		}
	}
	t.Fatal("no builtin endpoint set in SPDP announcement")
	return 0
}

func TestOnlyAPublishingParticipantAdvertisesWriterEndpoints(t *testing.T) {
	const publicationAnnouncer, subscriptionDetector = 0x04, 0x20
	if got := builtinEndpointSet(t, wireParticipant(t)); got&(publicationAnnouncer|subscriptionDetector) != 0 {
		t.Fatalf("read-only participant advertises %#x: peers would send it subscription traffic", got)
	}
	got := builtinEndpointSet(t, publishingParticipant(t))
	if got&publicationAnnouncer == 0 || got&subscriptionDetector == 0 {
		t.Fatalf("publishing participant advertises %#x: peers would never match its writers", got)
	}
	if got&0x1b != 0x1b {
		t.Fatalf("publishing participant advertises %#x and lost its reading endpoints", got)
	}
}

// publicationFields decodes the fields of a publication announcement that a
// remote participant needs to match a reader to the writer.
type publicationFields struct {
	endpoint, participant GUID
	topic, typ            string
	reliability           uint32
	locators              []Locator
}

func parsePublication(t *testing.T, d *DataSubmessage) publicationFields {
	t.Helper()
	params, order, err := parseParameterList(d.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var f publicationFields
	for _, prm := range params {
		switch prm.id {
		case pidEndpointGUID:
			f.endpoint, _ = paramGUID(prm.value, order)
		case pidParticipantGUID:
			f.participant, _ = paramGUID(prm.value, order)
		case pidTopicName:
			f.topic, _ = paramString(prm.value, order)
		case pidTypeName:
			f.typ, _ = paramString(prm.value, order)
		case pidReliability:
			f.reliability = order.Uint32(prm.value)
		case pidUnicastLocator:
			if l, ok := paramLocator(prm.value, order); ok {
				f.locators = append(f.locators, l)
			}
		}
	}
	return f
}

func TestNewWriterIsAnnouncedToKnownPeers(t *testing.T) {
	p := publishingParticipant(t)
	meta, metaLoc := listener(t)
	addPeer(t, p, GUIDPrefix{1, 16, 99}, meta, metaLoc)
	readSubmessages(t, meta) // our SPDP reply

	w, err := p.NewWriter(Publication{Topic: testTopic, Type: testType})
	if err != nil {
		t.Fatal(err)
	}
	subs := readSubmessages(t, meta)
	announcements := ofKind(subs, subDATA, entitySEDPPubWriter)
	if len(announcements) != 1 {
		t.Fatalf("peer received %d publication announcements, want 1", len(announcements))
	}
	if announcements[0].data.WriterSN != 1 {
		t.Fatalf("first announcement has sequence number %d, want 1", announcements[0].data.WriterSN)
	}
	got := parsePublication(t, announcements[0].data)
	port := p.ucast.LocalAddr().(*net.UDPAddr).Port
	if got.endpoint != w.GUID() || got.endpoint.Prefix != p.prefix || got.endpoint.EntityID&0xff != 0x03 {
		t.Fatalf("endpoint = %s, writer = %s (entity kind must be a user writer without key)", got.endpoint, w.GUID())
	}
	if got.participant != (GUID{Prefix: p.prefix, EntityID: entityParticipant}) {
		t.Fatalf("participant = %s", got.participant)
	}
	if got.topic != testTopic || got.typ != testType {
		t.Fatalf("topic %q type %q", got.topic, got.typ)
	}
	if got.reliability != reliabilityReliable {
		t.Fatalf("reliability = %d: a default (reliable) ROS 2 subscription would not match", got.reliability)
	}
	if len(got.locators) != 1 || got.locators[0] != udpv4Locator(p.local, port) {
		t.Fatalf("locators = %+v", got.locators)
	}

	heartbeats := ofKind(subs, subHEARTBEAT, entitySEDPPubWriter)
	if len(heartbeats) == 0 {
		t.Fatal("no heartbeat for the publication: a reader that lost the announcement would never ask for it")
	}
	hb, _ := ParseHeartbeat(heartbeats[len(heartbeats)-1].sub)
	if hb.FirstSN != 1 || hb.LastSN != 1 {
		t.Fatalf("heartbeat covers %d..%d, want 1..1", hb.FirstSN, hb.LastSN)
	}
}

func TestParticipantFoundLaterIsToldAboutExistingWriters(t *testing.T) {
	p := publishingParticipant(t)
	first, err := p.NewWriter(Publication{Topic: testTopic, Type: testType})
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.NewWriter(Publication{Topic: "rt/sensorlink/porch/raw", Type: "std_msgs::msg::dds_::UInt8MultiArray_"})
	if err != nil {
		t.Fatal(err)
	}
	if first.GUID() == second.GUID() {
		t.Fatal("two writers share one identity")
	}

	meta, metaLoc := listener(t)
	addPeer(t, p, GUIDPrefix{1, 15, 77}, meta, metaLoc)
	subs := readSubmessages(t, meta)
	announcements := ofKind(subs, subDATA, entitySEDPPubWriter)
	if len(announcements) != 2 {
		t.Fatalf("new peer received %d publication announcements, want 2", len(announcements))
	}
	seen := map[GUID]SequenceNumber{}
	for _, a := range announcements {
		seen[parsePublication(t, a.data).endpoint] = a.data.WriterSN
	}
	if seen[first.GUID()] != 1 || seen[second.GUID()] != 2 {
		t.Fatalf("announcements = %v", seen)
	}

	// Seeing the same participant again must not repeat the announcements.
	addPeer(t, p, GUIDPrefix{1, 15, 77}, meta, metaLoc)
	if again := ofKind(readSubmessages(t, meta), subDATA, entitySEDPPubWriter); len(again) != 0 {
		t.Fatalf("known peer was sent %d announcements again", len(again))
	}
}

func TestReadOnlyParticipantRefusesWriters(t *testing.T) {
	if _, err := wireParticipant(t).NewWriter(Publication{Topic: testTopic, Type: testType}); err == nil {
		t.Fatal("a participant that never told its peers it publishes created a writer")
	}
}
