package rtps

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"sync"
	"time"
)

// publishEndpoints are the builtin endpoints only a publishing participant
// advertises: it announces publications and detects subscriptions.
const publishEndpoints = 0x04 | 0x20

// entityUserWriterNoKey is the entity kind of a user writer on a topic
// without a key, which is every ROS 2 topic.
const entityUserWriterNoKey = 0x03

const (
	// maxWriters bounds the topics one participant writes.
	maxWriters = 256
	// maxRemoteReaders bounds the readers remembered across a domain. A busy
	// ROS 2 graph has several per node; the bound only stops a hostile peer
	// from growing the table without limit.
	maxRemoteReaders = 4096
	// maxReaderLocators bounds the addresses kept per remote reader or peer.
	maxReaderLocators = 8
	// maxWriterPayload is the largest serialized sample Write accepts. A
	// sample travels in one datagram; there is no fragmentation on this side.
	maxWriterPayload = 60 * 1024
	// maxWithdrawals is how many withdrawals of closed writers stay
	// resendable. A reader that lost one would otherwise keep the writer.
	maxWithdrawals = 64
	// maxAcknackAnswersPerSecond bounds the answers to one reader's
	// ACKNACKs, whatever it sends.
	maxAcknackAnswersPerSecond = 20
	// maxResendsPerAcknack bounds the announcements resent for one ACKNACK.
	// A request is far smaller than what it can ask for, and the answer goes
	// to the address the peer announced, so an unbounded answer would let a
	// forged announcement aim a flood at someone else. The heartbeat that
	// ends the answer makes an honest reader ask for the rest.
	maxResendsPerAcknack = 16
)

// Publication names what a Writer writes, in DDS terms. A ROS 2 topic
// "/a/b" is the DDS topic "rt/a/b", and the ROS 2 type "pkg/msg/Name" is the
// DDS type "pkg::msg::dds_::Name_".
type Publication struct {
	Topic string
	Type  string
}

// remoteReader is a reader another participant announced over SEDP.
type remoteReader struct {
	guid     GUID
	topic    string
	typ      string
	locators []Locator // its own unicast locators; empty means the peer's default
}

// publishState is the part of a Participant that only publishing uses.
type publishState struct {
	// writers holds the open local writers by entity ID.
	writers map[uint32]*Writer
	// readers holds every remote reader on the domain, matched or not, so a
	// writer created later finds the readers that were announced before it.
	readers map[GUID]*remoteReader
	// peerUser maps a peer to the locators its readers take samples at when
	// they do not name their own.
	peerUser map[GUIDPrefix][]Locator
	// hbCount counts the heartbeats sent per local writer entity. A reader
	// ignores a heartbeat whose count is not higher than the last one it saw.
	hbCount map[uint32]uint32
	// subReaders holds, per peer, what has arrived of its subscription
	// announcements.
	subReaders map[GUIDPrefix]*sedpSubReader
	// pubHistory is what the publication announcer can still resend, by
	// sequence number: the announcement of every open writer and the most
	// recent withdrawals.
	pubHistory map[SequenceNumber][]byte
	// withdrawals lists the withdrawals kept in pubHistory, oldest first.
	withdrawals []SequenceNumber
	// acks keeps the answers to each remote reader's ACKNACKs bounded.
	acks map[ackKey]*ackState
}

// ackKey names one remote reader asking one local writer.
type ackKey struct {
	reader GUID
	writer uint32
}

// ackState is what has been seen and answered of one reader's ACKNACKs.
type ackState struct {
	// count is the highest ACKNACK count seen. A reader numbers its
	// ACKNACKs; one that is not newer is a duplicate or arrived late.
	count       uint32
	windowStart time.Time
	answered    int
}

// Writer publishes samples of one topic. It is safe for concurrent use.
//
// The writer offers RELIABLE delivery so that every reader matches it, but it
// keeps no history: a sample is sent once, and each sample carries a heartbeat
// saying that nothing earlier can be resent. A reliable reader that lost a
// sample therefore moves on instead of waiting for it.
type Writer struct {
	p      *Participant
	pub    Publication
	entity uint32

	// sendMu serializes Write so samples leave in sequence-number order.
	sendMu sync.Mutex

	// Guarded by p.mu.
	// sedpSN is the sequence number of this writer's announcement.
	sedpSN SequenceNumber
	// announce is that announcement, kept to replay it to late joiners.
	announce []byte
	// sn is the sequence number of the last sample written.
	sn     SequenceNumber
	closed bool
	// matched maps each matched remote reader to the address its samples go
	// to (nil when it has no usable locator).
	matched map[GUID]*net.UDPAddr
	// dests is the distinct addresses in matched. It is replaced, never
	// changed in place, so Write can send to it after releasing p.mu.
	dests []*net.UDPAddr
}

// GUID identifies the writer on the domain.
func (w *Writer) GUID() GUID { return GUID{Prefix: w.p.prefix, EntityID: w.entity} }

func (w *Writer) isClosed() bool {
	w.p.mu.Lock()
	defer w.p.mu.Unlock()
	return w.closed
}

// Matched reports how many remote readers currently receive this writer's
// samples.
func (w *Writer) Matched() int {
	w.p.mu.Lock()
	defer w.p.mu.Unlock()
	return len(w.matched)
}

// NewWriter announces a writer for pub. Readers of the same topic and type on
// the domain match it and are sent every sample written from then on.
func (p *Participant) NewWriter(pub Publication) (*Writer, error) {
	if !p.cfg.Publish {
		return nil, errors.New("rtps: participant was not created to publish")
	}
	if pub.Topic == "" || pub.Type == "" {
		return nil, errors.New("rtps: publication needs a topic and a type")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.closed:
		return nil, errors.New("rtps: participant closed")
	default:
	}
	if len(p.writers) >= maxWriters {
		return nil, errors.New("rtps: too many writers on one participant")
	}
	w := &Writer{p: p, pub: pub, entity: (p.nextEntity << 8) | entityUserWriterNoKey}
	p.nextEntity++
	p.seqByWriter[entitySEDPPubWriter]++
	w.sedpSN = p.seqByWriter[entitySEDPPubWriter]
	w.announce = buildMessage(p.prefix, buildData(entityUnknown, entitySEDPPubWriter, w.sedpSN, p.publicationPayload(w)))
	p.writers[w.entity] = w
	p.pubHistory[w.sedpSN] = w.announce
	p.sendDiscoveryLocked(w.announce)
	p.sendDiscoveryLocked(buildMessage(p.prefix, p.publicationHeartbeatLocked(p.resendableLocked())...))
	p.rematchLocked()
	return w, nil
}

// Close withdraws the writer: peers are told it is gone and Write fails from
// then on. It waits for a Write that is under way.
func (w *Writer) Close() error {
	w.sendMu.Lock()
	defer w.sendMu.Unlock()
	p := w.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	w.matched, w.dests = nil, nil
	delete(p.writers, w.entity)
	delete(p.pubHistory, w.sedpSN)
	delete(p.hbCount, w.entity)
	for key := range p.acks {
		if key.writer == w.entity {
			delete(p.acks, key)
		}
	}
	select {
	case <-p.closed:
		return nil // the participant is gone, and its writers with it
	default:
	}
	p.seqByWriter[entitySEDPPubWriter]++
	sn := p.seqByWriter[entitySEDPPubWriter]
	withdrawal := buildMessage(p.prefix, buildDispose(entitySEDPPubWriter, sn, w.GUID()))
	p.pubHistory[sn] = withdrawal
	p.withdrawals = append(p.withdrawals, sn)
	if len(p.withdrawals) > maxWithdrawals {
		delete(p.pubHistory, p.withdrawals[0])
		p.withdrawals = p.withdrawals[1:]
	}
	p.sendDiscoveryLocked(withdrawal)
	p.sendDiscoveryLocked(buildMessage(p.prefix, p.publicationHeartbeatLocked(p.resendableLocked())...))
	return nil
}

// Write sends one serialized sample, including its 4-byte encapsulation
// header, to every matched reader. With no reader matched the sample is
// dropped; it still takes a sequence number, so a reader that matches later
// starts from the stream's current position. An error means the sample did
// not leave for at least one reader; it is not resent.
func (w *Writer) Write(payload []byte) error {
	if len(payload) < 4 {
		return errors.New("rtps: payload has no encapsulation header")
	}
	if len(payload) > maxWriterPayload {
		return fmt.Errorf("rtps: payload of %d bytes exceeds the %d a writer can send", len(payload), maxWriterPayload)
	}
	w.sendMu.Lock()
	defer w.sendMu.Unlock()
	p := w.p
	p.mu.Lock()
	if w.closed {
		p.mu.Unlock()
		return errors.New("rtps: writer closed")
	}
	w.sn++
	dests := w.dests
	var dgram []byte
	if len(dests) > 0 {
		dgram = buildMessage(p.prefix,
			buildInfoTS(time.Now()),
			buildData(entityUnknown, w.entity, w.sn, padPayload(payload)),
			p.heartbeatLocked(w))
	}
	p.mu.Unlock()
	var failed error
	for _, addr := range dests {
		if _, err := p.ucast.WriteToUDP(dgram, addr); err != nil {
			failed = fmt.Errorf("rtps: sending sample %d to %s: %w", w.sn, addr, err)
		}
	}
	return failed
}

// padPayload returns a copy of payload padded to a multiple of four bytes,
// which RTPS requires of every submessage but the last. The number of padding
// bytes goes in the low bits of the encapsulation options, where a reader
// that cares about the exact length looks for it.
func padPayload(payload []byte) []byte {
	pad := (4 - len(payload)%4) % 4
	out := make([]byte, len(payload), len(payload)+pad)
	copy(out, payload)
	if pad == 0 {
		return out
	}
	out[3] = out[3]&^0x03 | byte(pad)
	return append(out, make([]byte, pad)...)
}

// heartbeatLocked frames a writer's heartbeat: nothing up to and including
// the last sample can be resent. It is addressed to no reader in particular
// and marked final, so a reader answers only if it believes it is missing
// something, and the answer to that is this same heartbeat.
func (p *Participant) heartbeatLocked(w *Writer) []byte {
	p.hbCount[w.entity]++
	return buildHeartbeat(entityUnknown, w.entity, w.sn+1, w.sn, p.hbCount[w.entity], true)
}

// publicationPayload builds the SEDP PublicationBuiltinTopicData describing a
// writer. It offers RELIABLE so that both reliable readers (the ROS 2
// default) and best-effort readers match, and VOLATILE because nothing
// written before a reader matched is kept for it.
func (p *Participant) publicationPayload(w *Writer) []byte {
	b := newPLBuilder()
	b.addGUID(pidParticipantGUID, GUID{Prefix: p.prefix, EntityID: entityParticipant})
	b.addGUID(pidEndpointGUID, w.GUID())
	b.addString(pidTopicName, w.pub.Topic)
	b.addString(pidTypeName, w.pub.Type)
	b.addReliability(reliabilityReliable)
	b.addUint32(pidDurability, 0) // VOLATILE
	port := p.ucast.LocalAddr().(*net.UDPAddr).Port
	b.addLocator(pidUnicastLocator, udpv4Locator(p.local, port))
	return b.finish()
}

// announcedWritersLocked returns the open writers in announcement order.
func (p *Participant) announcedWritersLocked() []*Writer {
	out := make([]*Writer, 0, len(p.writers))
	for _, w := range p.writers {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].sedpSN < out[j].sedpSN })
	return out
}

// resendableLocked lists, in order, the publication sequence numbers that can
// still be resent.
func (p *Participant) resendableLocked() []SequenceNumber {
	out := make([]SequenceNumber, 0, len(p.pubHistory))
	for sn := range p.pubHistory {
		out = append(out, sn)
	}
	slices.Sort(out)
	return out
}

// publicationHeartbeatLocked builds the submessages that tell a reader of our
// publication announcements which of them it can ask for: a HEARTBEAT from
// the first available sequence number to the newest one used, after a GAP
// for each run in between that is not available. available must be ascending.
func (p *Participant) publicationHeartbeatLocked(available []SequenceNumber) [][]byte {
	last := p.seqByWriter[entitySEDPPubWriter]
	first := last + 1
	if len(available) > 0 {
		first = available[0]
	}
	var subs [][]byte
	next := first
	for _, sn := range available {
		if sn > next {
			subs = append(subs, buildGap(entityUnknown, entitySEDPPubWriter, next, sn-1))
		}
		next = sn + 1
	}
	if next <= last {
		subs = append(subs, buildGap(entityUnknown, entitySEDPPubWriter, next, last))
	}
	p.hbCount[entitySEDPPubWriter]++
	return append(subs, buildHeartbeat(entityUnknown, entitySEDPPubWriter, first, last, p.hbCount[entitySEDPPubWriter], true))
}

// publicationSyncLocked returns the datagrams that bring peers' view of our
// writers up to date, or nothing when this participant has never had one:
// the announcement of every open writer, then the heartbeat. A participant we
// have just met has no use for the withdrawals of writers it never saw, so
// for it they are written off rather than offered.
func (p *Participant) publicationSyncLocked(newPeer bool) [][]byte {
	if !p.cfg.Publish || p.seqByWriter[entitySEDPPubWriter] == 0 {
		return nil
	}
	writers := p.announcedWritersLocked()
	out := make([][]byte, 0, len(writers)+1)
	available := p.resendableLocked()
	if newPeer {
		available = available[:0]
	}
	for _, w := range writers {
		out = append(out, w.announce)
		if newPeer {
			available = append(available, w.sedpSN)
		}
	}
	return append(out, buildMessage(p.prefix, p.publicationHeartbeatLocked(available)...))
}

// handleAcknack answers a remote reader that reports what it has of one of
// our writers. Every answer ends in a final, undirected heartbeat, which a
// reader does not answer unless it is still missing something, so the
// exchange stops by itself.
func (p *Participant) handleAcknack(prefix GUIDPrefix, s Submessage) {
	ack, err := ParseAcknack(s)
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	meta, known := p.peers[prefix]
	if !known {
		return
	}
	key := ackKey{reader: GUID{Prefix: prefix, EntityID: ack.ReaderID}, writer: ack.WriterID}
	var w *Writer
	var addr *net.UDPAddr
	if ack.WriterID == entitySEDPPubWriter {
		if ack.ReaderID != entitySEDPPubReader {
			return
		}
	} else {
		if w = p.writers[ack.WriterID]; w == nil {
			return
		}
		if addr = w.matched[key.reader]; addr == nil {
			return
		}
	}
	st := p.acks[key]
	if st == nil {
		st = &ackState{}
		p.acks[key] = st
	} else if ack.Count <= st.count {
		return
	}
	st.count = ack.Count
	now := time.Now()
	if now.Sub(st.windowStart) >= time.Second {
		st.windowStart, st.answered = now, 0
	}
	if st.answered >= maxAcknackAnswersPerSecond {
		return
	}

	if w != nil {
		if len(ack.Missing) == 0 && ack.Base > w.sn {
			return // up to date
		}
		subs := [][]byte{buildInfoDst(prefix)}
		if len(ack.Missing) > 0 && ack.Missing[0] <= w.sn {
			// No sample is kept, so everything asked for is gone.
			subs = append(subs, buildGap(ack.ReaderID, w.entity, ack.Missing[0], w.sn))
		}
		st.answered++
		p.sendTo(addr, buildMessage(p.prefix, append(subs, p.heartbeatLocked(w))...))
		return
	}

	last := p.seqByWriter[entitySEDPPubWriter]
	if len(ack.Missing) == 0 && ack.Base > last {
		return // up to date
	}
	st.answered++
	send := func(dgram []byte) {
		for _, l := range meta {
			if a, ok := l.UDPAddr(); ok {
				p.sendTo(a, dgram)
			}
		}
	}
	resent := 0
	for _, sn := range ack.Missing {
		if resent == maxResendsPerAcknack {
			break
		}
		if dgram := p.pubHistory[sn]; dgram != nil {
			send(dgram)
			resent++
		}
	}
	send(buildMessage(p.prefix, append([][]byte{buildInfoDst(prefix)}, p.publicationHeartbeatLocked(p.resendableLocked())...)...))
}

// setPeerUserLocatorsLocked records where a peer's readers take samples.
func (p *Participant) setPeerUserLocatorsLocked(prefix GUIDPrefix, locators []Locator) {
	if slices.Equal(p.peerUser[prefix], locators) {
		return
	}
	p.peerUser[prefix] = locators
	p.rematchLocked()
}

// removePeerReadersLocked forgets a departed peer's readers and locators.
func (p *Participant) removePeerReadersLocked(prefix GUIDPrefix) {
	if !p.cfg.Publish {
		return
	}
	delete(p.peerUser, prefix)
	delete(p.subReaders, prefix)
	for key := range p.acks {
		if key.reader.Prefix == prefix {
			delete(p.acks, key)
		}
	}
	for guid := range p.readers {
		if guid.Prefix == prefix {
			delete(p.readers, guid)
		}
	}
	p.rematchLocked()
}

// handleSEDPSubscription records or removes a remote reader. A participant
// may only speak for its own readers: the locators in an announcement decide
// where samples are sent.
func (p *Participant) handleSEDPSubscription(prefix GUIDPrefix, d *DataSubmessage) {
	if disposed, guid := discoveryDisposal(d); disposed {
		p.mu.Lock()
		if guid.Prefix == prefix && d.WriterSN > p.endpointSN[guid] {
			p.endpointSN[guid] = d.WriterSN
			if _, ok := p.readers[guid]; ok {
				delete(p.readers, guid)
				p.rematchLocked()
			}
		}
		p.mu.Unlock()
		return
	}
	params, order, err := parseParameterList(d.Payload)
	if err != nil {
		return
	}
	r := &remoteReader{}
	for _, prm := range params {
		switch prm.id {
		case pidTopicName:
			r.topic, _ = paramString(prm.value, order)
		case pidTypeName:
			r.typ, _ = paramString(prm.value, order)
		case pidEndpointGUID:
			r.guid, _ = paramGUID(prm.value, order)
		case pidUnicastLocator:
			if l, ok := paramLocator(prm.value, order); ok && len(r.locators) < maxReaderLocators {
				r.locators = append(r.locators, l)
			}
		}
	}
	if r.topic == "" || r.typ == "" || r.guid.Prefix != prefix {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if sn, ok := p.endpointSN[r.guid]; ok && d.WriterSN <= sn {
		return
	}
	if _, known := p.readers[r.guid]; !known && len(p.readers) >= maxRemoteReaders {
		return
	}
	p.endpointSN[r.guid] = d.WriterSN
	p.readers[r.guid] = r
	if _, ok := p.peerExpiry[prefix]; !ok {
		p.peerLease[prefix] = time.Duration(leaseSeconds) * time.Second
		p.peerExpiry[prefix] = time.Now().Add(p.peerLease[prefix])
	}
	p.rematchLocked()
}

// rematchLocked recomputes which remote readers each writer sends to. A
// reader that is new to a writer is sent a heartbeat straight away: a
// reliable reader otherwise waits for sample 1 of a stream that may be far
// past it.
func (p *Participant) rematchLocked() {
	for _, w := range p.writers {
		matched := make(map[GUID]*net.UDPAddr)
		var dests, fresh []*net.UDPAddr
		seen, told := map[string]bool{}, map[string]bool{}
		for guid, r := range p.readers {
			if r.topic != w.pub.Topic || r.typ != w.pub.Type {
				continue
			}
			addr := p.readerAddrLocked(r)
			matched[guid] = addr
			if addr == nil {
				continue
			}
			key := addr.String()
			if !seen[key] {
				seen[key] = true
				dests = append(dests, addr)
			}
			if _, was := w.matched[guid]; !was {
				p.logf("writer %s on %s matched reader %s at %s", w.GUID(), w.pub.Topic, guid, addr)
				if !told[key] {
					told[key] = true
					fresh = append(fresh, addr)
				}
			}
		}
		w.matched, w.dests = matched, dests
		for key := range p.acks {
			if _, still := matched[key.reader]; key.writer == w.entity && !still {
				delete(p.acks, key)
			}
		}
		if len(fresh) > 0 {
			dgram := buildMessage(p.prefix, p.heartbeatLocked(w))
			for _, addr := range fresh {
				p.sendTo(addr, dgram)
			}
		}
	}
}

// readerAddrLocked picks the one address a reader's samples are sent to: its
// own locators if it named any, otherwise its participant's default. Sending
// to every locator would deliver each sample once per network the reader's
// host is on, so prefer an address on the network this participant is bound
// to and fall back to the first usable one.
func (p *Participant) readerAddrLocked(r *remoteReader) *net.UDPAddr {
	locators := r.locators
	if len(locators) == 0 {
		locators = p.peerUser[r.guid.Prefix]
	}
	var first *net.UDPAddr
	for _, l := range locators {
		addr, ok := l.UDPAddr()
		if !ok || addr.IP.IsMulticast() {
			continue
		}
		if p.onLocalNetwork(addr.IP) {
			return addr
		}
		if first == nil {
			first = addr
		}
	}
	return first
}

// onLocalNetwork reports whether ip is reached directly over the interface
// this participant is bound to.
func (p *Participant) onLocalNetwork(ip net.IP) bool {
	if p.local.IsLoopback() {
		return ip.IsLoopback()
	}
	return p.localNet != nil && p.localNet.Contains(ip)
}
