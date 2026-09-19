package rtps

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
)

// Standard DDS port mapping (RTPS 9.6.1.1) for the well-known ports we need.
// Wired-interface ports are ephemeral and advertised in SPDP. Loopback uses a
// standard participant port so localhost-only peers can find us by unicast.
const (
	portBase          = 7400
	domainIDGain      = 250
	spdpMulticastAddr = "239.255.0.1"
)

// spdpMulticastPort is the discovery multicast port for a domain.
func spdpMulticastPort(domainID int) int { return portBase + domainIDGain*domainID }

// announceInterval is how often the participant re-announces itself. The lease
// duration advertised is comfortably longer so a dropped packet does not evict
// us from a peer's participant table.
const (
	announceInterval   = 30 * time.Second
	leaseSeconds       = 90
	maxSampleSize      = 16 * 1024 * 1024
	maxSampleFragments = 64 * 1024
	maxFragmentSets    = 8
	maxFragmentBytes   = 64 * 1024 * 1024
	fragmentSetTTL     = 30 * time.Second
)

// Endpoint is a remote writer discovered over SEDP.
type Endpoint struct {
	GUID      GUID
	Topic     string
	Type      string
	Locators  []Locator
	Reliable  bool
	Multicast []Locator
}

// Sample is one received user-data message.
type Sample struct {
	Writer  GUID
	SN      SequenceNumber
	Payload []byte // serialized payload including its encapsulation header
}

type fragmentKey struct {
	writer GUID
	sn     SequenceNumber
}

type fragmentSet struct {
	buf           []byte
	received      []bool
	receivedCount int
	fragmentSize  int
	updated       time.Time
}

// Config parameterises a Participant.
type Config struct {
	DomainID int
	// Interface is the network interface to bind discovery multicast to. Empty
	// selects an eligible wired interface; explicit names override filtering.
	Interface string
	// NetworkNamespacePID creates the participant's sockets in the network
	// namespace of this process. The sockets remain attached after the creating
	// thread returns to the host namespace, allowing the Wendy agent to inspect
	// app-local ROS 2 graphs without weakening their isolation.
	NetworkNamespacePID uint32
	// VerifyNetworkNamespace runs after entering NetworkNamespacePID and before
	// any socket is opened. It lets callers reject a PID that was recycled after
	// they enumerated the target process.
	VerifyNetworkNamespace func() bool
	// Logf, when set, receives progress lines. Discovery failures are usually
	// silent by nature, so this is the only way to see what happened.
	Logf      func(format string, args ...any)
	namespace *networkNamespace // pinned by Pool during construction
}

// Participant is a read-only RTPS participant on one domain.
type Participant struct {
	cfg    Config
	prefix GUIDPrefix

	mcast *net.UDPConn // joined to the SPDP multicast group
	ucast *net.UDPConn // our unicast locator, for both meta and user traffic
	local net.IP

	mu        sync.Mutex
	endpoints map[GUID]*Endpoint
	// subs maps our reader entity ID to the writer GUID it is matched to.
	subs map[uint32]GUID
	// subWriters is the reverse index: writers we have subscribed to, which is
	// what user data is filtered on.
	subWriters map[GUID]struct{}
	// peers maps a discovered participant to its metatraffic unicast locators.
	// SEDP announcements must go here — a writer's data locators are a
	// different endpoint and will not be read as discovery traffic.
	peers      map[GUIDPrefix][]Locator
	lastReply  map[GUIDPrefix]time.Time
	peerExpiry map[GUIDPrefix]time.Time
	// peerLease is the duration a peer advertised (0 = never expires), so any
	// message from it can push peerExpiry out again without waiting for SPDP.
	peerLease  map[GUIDPrefix]time.Duration
	endpointSN map[GUID]SequenceNumber
	leases     map[*Lease]struct{}
	closed     chan struct{}
	closeOnce  sync.Once
	// ackCount tracks per-writer ACKNACK counts, which RTPS requires to
	// increase monotonically or the writer ignores the request as a duplicate.
	ackCount map[writerKey]uint32
	// sedpHighest is the highest SEDP sequence number received per writer. It
	// is what stops an ACKNACK storm: without it every heartbeat re-requests
	// the writer's whole history, the writer replays it, and the replay
	// provokes more heartbeats. Measured on a Go2, that amplified to 2.1M
	// submessages in 25 seconds.
	sedpHighest map[writerKey]SequenceNumber

	samples chan Sample
	discov  chan Endpoint

	nextEntity uint32
	// seqByWriter is the per-writer sequence-number space. RTPS gives every
	// writer its own, each starting at 1.
	seqByWriter map[uint32]SequenceNumber

	// Counters, so a discovery failure says where it broke rather than only
	// that it did. Discovery is silent by nature: nothing errors, packets
	// simply never arrive.
	statMcastPkts         atomic.Int64
	statUcastPkts         atomic.Int64
	statRTPS              atomic.Int64
	statSPDP              atomic.Int64
	statSEDP              atomic.Int64
	statUserData          atomic.Int64
	statPeers             atomic.Int64
	statHeartbeat         atomic.Int64
	statData              atomic.Int64
	statDataErr           atomic.Int64
	statAcksSent          atomic.Int64
	statRepliesSent       atomic.Int64
	statRepliesSuppressed atomic.Int64
	statLocalIgnored      atomic.Int64

	// seenMu guards histograms of which writer entity IDs are actually sending,
	// so a "nothing arrived" result can name what did arrive instead.
	seenMu    sync.Mutex
	seenData  map[uint32]int
	seenHB    map[uint32]int
	unmatched map[GUID]int

	// subAnnounce holds the SEDP subscription datagrams to re-send on the
	// announce tick. A single announcement is one UDP datagram: if it is lost,
	// or arrives before the peer has finished discovering us, the subscription
	// never matches and no data ever flows.
	subAnnounce map[GUID][]byte

	// fragmentsMu isolates potentially large fragment copies from participant
	// bookkeeping such as subscription changes and discovery updates.
	fragmentsMu sync.Mutex
	// fragments holds bounded in-flight DATA_FRAG samples. Camera messages are
	// routinely larger than one UDP datagram; without this reassembly the DDS
	// reader can discover image topics but can never receive a frame from them.
	fragments     map[fragmentKey]*fragmentSet
	fragmentBytes int
}

// WriterHistogram reports how many DATA and HEARTBEAT submessages arrived per
// writer entity ID. Diagnostic only.
func (p *Participant) WriterHistogram() (data, heartbeat map[uint32]int) {
	p.seenMu.Lock()
	defer p.seenMu.Unlock()
	data = make(map[uint32]int, len(p.seenData))
	heartbeat = make(map[uint32]int, len(p.seenHB))
	for k, v := range p.seenData {
		data[k] = v
	}
	for k, v := range p.seenHB {
		heartbeat[k] = v
	}
	return data, heartbeat
}

// Stats is a snapshot of what the participant has actually seen on the wire.
type Stats struct {
	MulticastPackets      int64
	UnicastPackets        int64
	RTPSMessages          int64
	SPDPAnnounces         int64
	SEDPAnnounces         int64
	UserDataMessages      int64
	PeersSeen             int64
	Heartbeats            int64
	AcksSent              int64
	DataSubmessages       int64
	DataParseErrors       int64
	SPDPRepliesSent       int64
	SPDPRepliesSuppressed int64
	LocalMessagesIgnored  int64
}

// Stats reports receive counters.
func (p *Participant) Stats() Stats {
	return Stats{
		MulticastPackets:      p.statMcastPkts.Load(),
		UnicastPackets:        p.statUcastPkts.Load(),
		RTPSMessages:          p.statRTPS.Load(),
		SPDPAnnounces:         p.statSPDP.Load(),
		SEDPAnnounces:         p.statSEDP.Load(),
		UserDataMessages:      p.statUserData.Load(),
		PeersSeen:             p.statPeers.Load(),
		Heartbeats:            p.statHeartbeat.Load(),
		AcksSent:              p.statAcksSent.Load(),
		DataSubmessages:       p.statData.Load(),
		DataParseErrors:       p.statDataErr.Load(),
		SPDPRepliesSent:       p.statRepliesSent.Load(),
		SPDPRepliesSuppressed: p.statRepliesSuppressed.Load(),
		LocalMessagesIgnored:  p.statLocalIgnored.Load(),
	}
}

func (p *Participant) logf(format string, args ...any) {
	if p.cfg.Logf != nil {
		p.cfg.Logf(format, args...)
	}
}

func newParticipant(cfg Config) *Participant {
	return &Participant{
		cfg:         cfg,
		endpoints:   map[GUID]*Endpoint{},
		subs:        map[uint32]GUID{},
		peers:       map[GUIDPrefix][]Locator{},
		lastReply:   map[GUIDPrefix]time.Time{},
		peerExpiry:  map[GUIDPrefix]time.Time{},
		peerLease:   map[GUIDPrefix]time.Duration{},
		endpointSN:  map[GUID]SequenceNumber{},
		leases:      map[*Lease]struct{}{},
		closed:      make(chan struct{}),
		subAnnounce: map[GUID][]byte{},
		ackCount:    map[writerKey]uint32{},
		sedpHighest: map[writerKey]SequenceNumber{},
		subWriters:  map[GUID]struct{}{},
		seenData:    map[uint32]int{},
		seenHB:      map[uint32]int{},
		unmatched:   map[GUID]int{},
		// Four maximum-sized image samples cap queued payload memory at 64 MiB;
		// the channel is lossy by design, so a slow consumer gets newer frames.
		samples:     make(chan Sample, 4),
		discov:      make(chan Endpoint, 32),
		nextEntity:  1,
		seqByWriter: map[uint32]SequenceNumber{},
		fragments:   map[fragmentKey]*fragmentSet{},
	}
}

// NewParticipant binds sockets and joins discovery. Run drives announcements and
// receive loops; Close releases the sockets. Pool manages both for its leases.
func NewParticipant(cfg Config) (*Participant, error) {
	if cfg.DomainID < 0 || cfg.DomainID > 232 {
		return nil, fmt.Errorf("rtps: invalid domain %d", cfg.DomainID)
	}
	p := newParticipant(cfg)
	if _, err := rand.Read(p.prefix[:]); err != nil {
		return nil, fmt.Errorf("rtps: generating GUID prefix: %w", err)
	}
	// The first two octets of a GUID prefix are conventionally the vendor ID.
	p.prefix[0], p.prefix[1] = 0x01, 0x0f

	localParticipants.Store(p.prefix, struct{}{})
	open := func() error {
		return withNetworkNamespace(cfg.NetworkNamespacePID, cfg.VerifyNetworkNamespace, p.openSockets)
	}
	if cfg.namespace != nil {
		open = func() error { return cfg.namespace.run(p.openSockets) }
	}
	if err := open(); err != nil {
		_ = p.Close()
		return nil, err
	}
	return p, nil
}

func verifyNamespaceTarget(pid uint32, verify func() bool) error {
	if verify != nil && !verify() {
		return fmt.Errorf("rtps: network namespace process %d changed", pid)
	}
	return nil
}

func combineNamespaceRestoreError(operationErr, restoreErr error) error {
	if restoreErr == nil {
		return operationErr
	}
	return errors.Join(operationErr, fmt.Errorf("rtps: restoring host network namespace: %w", restoreErr))
}

func (p *Participant) openSockets() error {
	iface, err := p.resolveInterface()
	if err != nil {
		return err
	}

	mport := spdpMulticastPort(p.cfg.DomainID)
	group := &net.UDPAddr{IP: net.ParseIP(spdpMulticastAddr), Port: mport}
	mc, err := net.ListenMulticastUDP("udp4", iface, group)
	if err != nil {
		return fmt.Errorf("rtps: joining %s:%d: %w", spdpMulticastAddr, mport, err)
	}
	if err := restrictMulticastToJoinedInterfaces(mc); err != nil {
		_ = mc.Close()
		return fmt.Errorf("rtps: restricting multicast memberships: %w", err)
	}
	_ = mc.SetReadBuffer(2 << 20)
	p.mcast = mc

	uc, err := p.listenUnicast(iface.Flags&net.FlagLoopback != 0)
	if err != nil {
		mc.Close()
		return fmt.Errorf("rtps: binding unicast socket: %w", err)
	}
	_ = uc.SetReadBuffer(4 << 20)
	p.ucast = uc

	if err := ipv4.NewPacketConn(uc).SetMulticastInterface(iface); err != nil {
		p.logf("warning: pinning multicast egress to %s failed: %v", iface.Name, err)
	} else {
		p.logf("multicast egress pinned to %s", iface.Name)
	}
	if err := ipv4.NewPacketConn(uc).SetMulticastTTL(4); err != nil {
		p.logf("warning: setting multicast TTL failed: %v", err)
	}
	p.logf("participant %x on domain %d, unicast %s, multicast %s:%d",
		p.prefix, p.cfg.DomainID, uc.LocalAddr(), spdpMulticastAddr, mport)
	return nil
}

// CycloneDDS localhost-only discovery probes the standard metatraffic ports
// instead of multicast. Listening on one of those ports makes us discoverable
// without adding a unicast probe burst of our own. Bind atomically to skip ports
// already owned by other DDS participants.
func (p *Participant) listenUnicast(loopback bool) (*net.UDPConn, error) {
	if !loopback {
		return net.ListenUDP("udp4", &net.UDPAddr{IP: p.local})
	}
	for index := 0; index < 100; index++ {
		port := spdpMulticastPort(p.cfg.DomainID) + 10 + 2*index
		if port > 65535 {
			break
		}
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: p.local, Port: port})
		if err == nil {
			return conn, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("rtps: no available loopback discovery port on domain %d", p.cfg.DomainID)
}

// resolveInterface picks the interface to bind multicast to and records its
// IPv4 address, which is what we advertise as our locator.
func (p *Participant) resolveInterface() (*net.Interface, error) {
	if p.cfg.Interface != "" {
		iface, err := net.InterfaceByName(p.cfg.Interface)
		if err != nil {
			return nil, fmt.Errorf("rtps: interface %q: %w", p.cfg.Interface, err)
		}
		ip, err := firstIPv4(iface)
		if err != nil {
			return nil, err
		}
		p.local = ip
		return iface, nil
	}
	ifaces, err := HostInterfaces()
	if err != nil {
		return nil, err
	}
	if len(ifaces) == 0 {
		return nil, fmt.Errorf("rtps: no wired multicast-capable IPv4 interface")
	}
	p.cfg.Interface = ifaces[0]
	return p.resolveInterface()
}

func firstIPv4(iface *net.Interface) (net.IP, error) {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("rtps: addresses of %s: %w", iface.Name, err)
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if v4 := ipnet.IP.To4(); v4 != nil {
				return v4, nil
			}
		}
	}
	return nil, fmt.Errorf("rtps: %s has no IPv4 address", iface.Name)
}

// Close releases the participant's sockets.
// localParticipants contains exact active GUID prefixes, never vendor IDs.
var localParticipants sync.Map

func (p *Participant) Close() error {
	p.closeOnce.Do(func() {
		if p.closed != nil {
			close(p.closed)
		}
		if p.mcast != nil {
			_ = p.mcast.Close()
		}
		if p.ucast != nil {
			_ = p.ucast.Close()
		}
		localParticipants.Delete(p.prefix)
	})
	return nil
}

// Endpoints returns a fresh snapshot, including removal of expired peers.
func (p *Participant) Endpoints() []Endpoint {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expirePeersLocked(time.Now())
	out := make([]Endpoint, 0, len(p.endpoints))
	for _, ep := range p.endpoints {
		copy := *ep
		copy.Locators = append([]Locator(nil), ep.Locators...)
		copy.Multicast = append([]Locator(nil), ep.Multicast...)
		out = append(out, copy)
	}
	return out
}

func (p *Participant) discoveryChangedLocked() {
	for l := range p.leases {
		select {
		case l.changed <- struct{}{}:
		default:
		}
	}
}

func (p *Participant) expirePeersLocked(now time.Time) {
	for prefix, expiry := range p.peerExpiry {
		if !expiry.IsZero() && !now.Before(expiry) {
			p.removePeerLocked(prefix)
		}
	}
	// Keep a recent reservation even if the peer expires or disposes itself.
	// A short advertised lease must not bypass the reply cooldown on rejoin.
	for prefix, last := range p.lastReply {
		if _, present := p.peers[prefix]; !present && now.Sub(last) >= announceInterval {
			delete(p.lastReply, prefix)
		}
	}
}

func (p *Participant) removePeerLocked(prefix GUIDPrefix) {
	delete(p.peers, prefix)
	delete(p.peerExpiry, prefix)
	delete(p.peerLease, prefix)
	for key := range p.sedpHighest {
		if key.prefix == prefix {
			delete(p.sedpHighest, key)
			delete(p.ackCount, key)
		}
	}
	for guid := range p.endpointSN {
		if guid.Prefix == prefix {
			delete(p.endpointSN, guid)
		}
	}
	for guid := range p.endpoints {
		if guid.Prefix == prefix {
			delete(p.endpoints, guid)
		}
	}
	p.discoveryChangedLocked()
}

// Discovered returns the channel of writers found over SEDP.
func (p *Participant) Discovered() <-chan Endpoint { return p.discov }

// Samples returns the channel of received user data.
func (p *Participant) Samples() <-chan Sample { return p.samples }

// Run drives the participant until ctx is cancelled: it reads both sockets and
// re-announces on a timer.
func (p *Participant) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); p.readLoop(ctx, p.mcast, true) }()
	go func() { defer wg.Done(); p.readLoop(ctx, p.ucast, false) }()
	go func() { defer wg.Done(); p.announceLoop(ctx) }()
	go func() { defer wg.Done(); p.fragmentCleanupLoop(ctx) }()

	select {
	case <-ctx.Done():
	case <-p.closed:
	}
	cancel()
	p.Close()
	wg.Wait()
	p.fragmentsMu.Lock()
	p.fragments = make(map[fragmentKey]*fragmentSet)
	p.fragmentBytes = 0
	p.fragmentsMu.Unlock()
}

func (p *Participant) fragmentCleanupLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			p.expireFragmentSets(now)
			p.mu.Lock()
			p.expirePeersLocked(now)
			p.mu.Unlock()
		}
	}
}

func (p *Participant) expireFragmentSets(now time.Time) {
	p.fragmentsMu.Lock()
	p.dropExpiredFragmentSetsLocked(now)
	p.fragmentsMu.Unlock()
}

func (p *Participant) dropExpiredFragmentSetsLocked(now time.Time) {
	for key, set := range p.fragments {
		if now.Sub(set.updated) > fragmentSetTTL {
			p.dropFragmentSetLocked(key)
		}
	}
}

func (p *Participant) announceLoop(ctx context.Context) {
	t := time.NewTicker(announceInterval)
	defer t.Stop()
	p.announce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.announce()
		}
	}
}

func (p *Participant) readLoop(ctx context.Context, conn *net.UDPConn, multicast bool) {
	buf := make([]byte, 65536)
	for {
		if ctx.Err() != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		if multicast {
			p.statMcastPkts.Add(1)
		} else {
			p.statUcastPkts.Add(1)
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		p.handle(pkt, src)
	}
}

// handle dispatches every submessage in one datagram.
func (p *Participant) handle(pkt []byte, src *net.UDPAddr) {
	msg, err := ParseMessage(pkt)
	if err != nil {
		return
	}
	if _, local := localParticipants.Load(msg.Prefix); local || msg.Prefix == p.prefix {
		p.statLocalIgnored.Add(1)
		return
	}
	p.statRTPS.Add(1)
	p.renewPeer(msg.Prefix)
	for _, s := range msg.Submessages {
		switch s.Kind {
		case subHEARTBEAT:
			p.statHeartbeat.Add(1)
			p.handleHeartbeat(msg.Prefix, s, src)
		case subDATA:
			p.statData.Add(1)
			d, err := ParseData(s)
			if err != nil {
				p.statDataErr.Add(1)
				continue
			}
			p.seenMu.Lock()
			p.seenData[d.WriterID]++
			p.seenMu.Unlock()
			switch d.WriterID {
			case entitySPDPWriter:
				p.statSPDP.Add(1)
				p.handleSPDP(msg.Prefix, d)
			case entitySEDPPubWriter:
				p.statSEDP.Add(1)
				p.noteSEDPReceived(msg.Prefix, d.WriterID, d.WriterSN)
				p.handleSEDPPublication(d)
			default:
				if len(d.Payload) != 0 {
					p.statUserData.Add(1)
					p.handleUserData(msg.Prefix, d)
				}
			}
		case subDATAFRAG:
			p.statData.Add(1)
			f, err := ParseDataFrag(s)
			if err != nil {
				p.statDataErr.Add(1)
				continue
			}
			// Builtin discovery traffic is small DATA, never DATA_FRAG. Treat a
			// fragmented builtin writer as malformed instead of feeding it to the
			// user-data reassembler.
			if f.WriterID == entitySPDPWriter || f.WriterID == entitySEDPPubWriter || f.WriterID == entitySEDPSubWriter {
				p.statDataErr.Add(1)
				continue
			}
			p.statUserData.Add(1)
			p.handleUserDataFrag(msg.Prefix, f)
		}
	}
}

// renewPeer extends a known peer's lease on any message from it. CycloneDDS
// renews a proxy participant the same way, so a lost SPDP datagram from a
// short-lease peer does not tear down a stream that is still delivering.
func (p *Participant) renewPeer(prefix GUIDPrefix) {
	p.mu.Lock()
	if lease, ok := p.peerLease[prefix]; ok && lease > 0 {
		p.peerExpiry[prefix] = time.Now().Add(lease)
	}
	p.mu.Unlock()
}

// handleHeartbeat answers a SEDP publications writer so it replays its history.
//
// This is the only reliability we implement, and only for discovery: SEDP
// builtin endpoints are RELIABLE + TRANSIENT_LOCAL, so a late joiner is
// announced the history via HEARTBEAT and must ACKNACK to receive it. User data
// needs none of this — our reader is BEST_EFFORT and the writer simply sends.
func (p *Participant) handleHeartbeat(prefix GUIDPrefix, s Submessage, src *net.UDPAddr) {
	hb, err := ParseHeartbeat(s)
	if err != nil {
		return
	}
	p.seenMu.Lock()
	p.seenHB[hb.WriterID]++
	p.seenMu.Unlock()
	if hb.WriterID != entitySEDPPubWriter {
		return
	}

	key := writerKey{prefix: prefix, entity: hb.WriterID}
	p.mu.Lock()
	have := p.sedpHighest[key]
	base := hb.FirstSN
	if have+1 > base {
		base = have + 1
	}
	if base > hb.LastSN {
		// Nothing outstanding. Staying silent here is the difference between a
		// steady trickle of heartbeats and a self-sustaining replay storm.
		p.mu.Unlock()
		return
	}
	p.ackCount[key]++
	count := p.ackCount[key]
	p.mu.Unlock()

	ack := buildAcknack(entitySEDPPubReader, entitySEDPPubWriter, base, hb.LastSN, count)
	dgram := buildMessage(p.prefix, buildInfoDst(prefix), ack)
	if src != nil {
		p.sendTo(src, dgram)
		p.statAcksSent.Add(1)
	}
}

// writerKey identifies a remote writer for per-writer ACKNACK counters, which
// RTPS requires to increase monotonically.
type writerKey struct {
	prefix GUIDPrefix
	entity uint32
}

// noteSEDPReceived advances the high-water mark used to decide what an ACKNACK
// still needs to ask for.
func (p *Participant) noteSEDPReceived(prefix GUIDPrefix, entity uint32, sn SequenceNumber) {
	key := writerKey{prefix: prefix, entity: entity}
	p.mu.Lock()
	if sn > p.sedpHighest[key] {
		p.sedpHighest[key] = sn
	}
	p.mu.Unlock()
}

// handleSPDP records a peer and immediately unicasts our own announcement back
// to its metatraffic locator, so it learns about us without waiting a full
// announce interval.
func (p *Participant) handleSPDP(prefix GUIDPrefix, d *DataSubmessage) {
	if disposed, _ := discoveryDisposal(d); disposed {
		p.mu.Lock()
		p.removePeerLocked(prefix)
		p.mu.Unlock()
		return
	}
	params, order, err := parseParameterList(d.Payload)
	if err != nil {
		return
	}
	if _, err := parameterListLength(d.Payload[4:], order); err != nil {
		return
	}
	var metaUnicast []Locator
	seenLocators := map[string]bool{}
	now := time.Now()
	lease := time.Duration(leaseSeconds) * time.Second
	for _, prm := range params {
		switch prm.id {
		case pidParticipantGUID:
			g, ok := paramGUID(prm.value, order)
			if !ok || g.Prefix != prefix {
				return
			}
		case pidParticipantLeaseDuration:
			if len(prm.value) >= 8 {
				sec, frac := order.Uint32(prm.value[:4]), order.Uint32(prm.value[4:8])
				if sec == 0x7fffffff && frac == 0xffffffff {
					lease = 0
				} else if sec <= 0x7fffffff {
					lease = time.Duration(sec)*time.Second + time.Duration((uint64(frac)*uint64(time.Second))>>32)
				}
			}
		case pidMetatrafficUnicastLocator:
			if l, ok := paramLocator(prm.value, order); ok {
				if addr, valid := l.UDPAddr(); valid && !addr.IP.IsMulticast() && !seenLocators[addr.String()] {
					seenLocators[addr.String()] = true
					metaUnicast = append(metaUnicast, l)
				}
			}
		}
	}
	if len(metaUnicast) == 0 {
		return
	}
	expiry := time.Time{}
	if lease > 0 {
		expiry = now.Add(lease)
	}
	p.mu.Lock()
	_, seen := p.peers[prefix]
	p.peers[prefix] = metaUnicast
	p.peerExpiry[prefix] = expiry
	p.peerLease[prefix] = lease
	last := p.lastReply[prefix]
	reply := last.IsZero() || now.Sub(last) >= announceInterval
	// Reserve while locked: multicast and unicast receive loops race here.
	if reply {
		p.lastReply[prefix] = now
	}
	p.mu.Unlock()
	if !seen {
		p.statPeers.Add(1)
		p.logf("peer %x metatraffic=%v", prefix, metaUnicast)
	}
	if !reply {
		p.statRepliesSuppressed.Add(1)
		return
	}
	dgram := p.spdpDatagram()
	for _, l := range metaUnicast {
		if addr, ok := l.UDPAddr(); ok && p.sendTo(addr, dgram) {
			p.statRepliesSent.Add(1)
		}
	}
}

// handleSEDPPublication turns a remote publication announcement into an
// Endpoint, or removes the endpoint named by a disposal key.
func (p *Participant) handleSEDPPublication(d *DataSubmessage) {
	if disposed, guid := discoveryDisposal(d); disposed {
		p.mu.Lock()
		if guid != (GUID{}) && d.WriterSN > p.endpointSN[guid] {
			p.endpointSN[guid] = d.WriterSN
			delete(p.endpoints, guid)
			p.discoveryChangedLocked()
		}
		p.mu.Unlock()
		return
	}
	params, order, err := parseParameterList(d.Payload)
	if err != nil {
		return
	}
	ep := &Endpoint{}
	for _, prm := range params {
		switch prm.id {
		case pidTopicName:
			ep.Topic, _ = paramString(prm.value, order)
		case pidTypeName:
			ep.Type, _ = paramString(prm.value, order)
		case pidEndpointGUID:
			ep.GUID, _ = paramGUID(prm.value, order)
		case pidUnicastLocator:
			if l, ok := paramLocator(prm.value, order); ok {
				ep.Locators = append(ep.Locators, l)
			}
		case pidMulticastLocator:
			if l, ok := paramLocator(prm.value, order); ok {
				ep.Multicast = append(ep.Multicast, l)
			}
		case pidReliability:
			if len(prm.value) >= 4 {
				ep.Reliable = order.Uint32(prm.value[0:4]) == reliabilityReliable
			}
		}
	}
	if ep.Topic == "" || ep.Type == "" || ep.GUID == (GUID{}) {
		return
	}
	p.mu.Lock()
	if sn, ok := p.endpointSN[ep.GUID]; ok && d.WriterSN <= sn {
		p.mu.Unlock()
		return
	}
	p.endpointSN[ep.GUID] = d.WriterSN
	_, seen := p.endpoints[ep.GUID]
	p.endpoints[ep.GUID] = ep
	if _, ok := p.peerExpiry[ep.GUID.Prefix]; !ok {
		p.peerLease[ep.GUID.Prefix] = time.Duration(leaseSeconds) * time.Second
		p.peerExpiry[ep.GUID.Prefix] = time.Now().Add(p.peerLease[ep.GUID.Prefix])
	}
	p.discoveryChangedLocked()
	p.mu.Unlock()
	if seen {
		return
	}
	p.logf("discovered writer %s topic=%s type=%s reliable=%v locators=%d",
		ep.GUID, ep.Topic, ep.Type, ep.Reliable, len(ep.Locators))
	select {
	case p.discov <- *ep:
	default:
	}
}

// handleUserData forwards a sample from a writer we subscribed to.
//
// Matching is by writer GUID, not reader ID. A writer commonly addresses
// ENTITYID_UNKNOWN, so trusting the reader ID alone would funnel every
// unaddressed sample on the domain into whichever decoder we happen to be
// running — which shows up as spurious "payload too short" errors from small
// messages of entirely unrelated topics.
func (p *Participant) handleUserData(prefix GUIDPrefix, d *DataSubmessage) {
	p.deliverUserData(GUID{Prefix: prefix, EntityID: d.WriterID}, d.WriterSN, d.Payload)
}

func (p *Participant) deliverUserData(writer GUID, sn SequenceNumber, payload []byte) {
	p.mu.Lock()
	_, subscribed := p.subWriters[writer]
	p.mu.Unlock()
	if !subscribed {
		p.seenMu.Lock()
		p.unmatched[writer]++
		n := p.unmatched[writer]
		p.seenMu.Unlock()
		if n == 1 {
			p.logf("user data from unsubscribed writer %s (ignored)", writer)
		}
		return
	}
	s := Sample{
		Writer:  writer,
		SN:      sn,
		Payload: payload,
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.subWriters[writer]; !ok {
		return
	}
	if len(p.leases) == 0 {
		enqueueSample(p.samples, s)
	}
	for l := range p.leases {
		if _, ok := l.subs[writer]; ok {
			enqueueSample(l.samples, s)
		}
	}
}

// handleUserDataFrag reassembles one bounded serialized sample and delivers it
// through the same subscription filter as ordinary DATA. Old/incomplete sets
// are discarded so a publisher disappearing mid-frame cannot retain memory.
func (p *Participant) handleUserDataFrag(prefix GUIDPrefix, f *DataFragSubmessage) {
	if f.SampleSize == 0 || f.FragmentStartingNum == 0 || f.FragmentSize == 0 || f.FragmentsInSubmessage == 0 || f.SampleSize > maxSampleSize {
		p.statDataErr.Add(1)
		return
	}
	writer := GUID{Prefix: prefix, EntityID: f.WriterID}
	p.mu.Lock()
	if _, subscribed := p.subWriters[writer]; !subscribed {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	payload, complete := p.addUserDataFrag(writer, f)
	if complete {
		p.deliverUserData(writer, f.WriterSN, payload)
	}
}

func (p *Participant) addUserDataFrag(writer GUID, f *DataFragSubmessage) ([]byte, bool) {
	now := time.Now()
	p.fragmentsMu.Lock()
	defer p.fragmentsMu.Unlock()
	p.dropExpiredFragmentSetsLocked(now)
	key := fragmentKey{writer: writer, sn: f.WriterSN}
	set := p.fragments[key]
	fragmentCount := (int(f.SampleSize) + int(f.FragmentSize) - 1) / int(f.FragmentSize)
	if fragmentCount > maxSampleFragments {
		p.statDataErr.Add(1)
		return nil, false
	}
	if set == nil {
		for len(p.fragments) >= maxFragmentSets || p.fragmentBytes+int(f.SampleSize) > maxFragmentBytes {
			var oldestKey fragmentKey
			var oldest time.Time
			for candidate, existing := range p.fragments {
				if oldest.IsZero() || existing.updated.Before(oldest) {
					oldestKey, oldest = candidate, existing.updated
				}
			}
			p.dropFragmentSetLocked(oldestKey)
		}
		set = &fragmentSet{
			buf:          make([]byte, int(f.SampleSize)),
			received:     make([]bool, fragmentCount),
			fragmentSize: int(f.FragmentSize),
			updated:      now,
		}
		p.fragments[key] = set
		p.fragmentBytes += len(set.buf)
	} else if len(set.buf) != int(f.SampleSize) || set.fragmentSize != int(f.FragmentSize) {
		p.dropFragmentSetLocked(key)
		p.statDataErr.Add(1)
		return nil, false
	}

	start := int(f.FragmentStartingNum) - 1
	payloadPos := 0
	valid := true
	for i := 0; i < int(f.FragmentsInSubmessage); i++ {
		fragmentIndex := start + i
		if fragmentIndex < 0 || fragmentIndex >= len(set.received) {
			valid = false
			break
		}
		offset := fragmentIndex * set.fragmentSize
		n := set.fragmentSize
		if remaining := len(set.buf) - offset; n > remaining {
			n = remaining
		}
		if n < 0 || payloadPos+n > len(f.Payload) {
			valid = false
			break
		}
		copy(set.buf[offset:offset+n], f.Payload[payloadPos:payloadPos+n])
		payloadPos += n
		if !set.received[fragmentIndex] {
			set.received[fragmentIndex] = true
			set.receivedCount++
		}
	}
	set.updated = now
	if !valid {
		p.dropFragmentSetLocked(key)
		p.statDataErr.Add(1)
		return nil, false
	}
	if set.receivedCount != len(set.received) {
		return nil, false
	}
	payload := set.buf
	p.dropFragmentSetLocked(key)
	return payload, true
}

// dropFragmentSetLocked requires fragmentsMu.
func (p *Participant) dropFragmentSetLocked(key fragmentKey) {
	if set := p.fragments[key]; set != nil {
		p.fragmentBytes -= len(set.buf)
		delete(p.fragments, key)
	}
}

// Subscribe announces a BEST_EFFORT reader for a writer's topic and type, so
// that writer starts sending us data. The reader is announced both to the
// discovery multicast group and directly to the writer's participant.
func (p *Participant) Subscribe(ep Endpoint) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.closed:
		return errors.New("rtps: participant closed")
	default:
	}
	if _, ok := p.subWriters[ep.GUID]; ok {
		return nil
	}
	entity := (p.nextEntity << 8) | entityUserReaderNoKey
	p.nextEntity++
	p.subs[entity] = ep.GUID
	p.subWriters[ep.GUID] = struct{}{}
	reader := GUID{Prefix: p.prefix, EntityID: entity}
	payload := p.subscriptionPayload(reader, ep)
	p.seqByWriter[entitySEDPSubWriter]++
	dgram := buildMessage(p.prefix, buildData(entityUnknown, entitySEDPSubWriter, p.seqByWriter[entitySEDPSubWriter], payload))
	p.subAnnounce[ep.GUID] = dgram
	p.sendDiscoveryLocked(dgram)
	return nil
}

// Unsubscribe removes delivery and recurring announcements and disposes the reader.
func (p *Participant) Unsubscribe(writer GUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.subWriters[writer]; !ok {
		return
	}
	delete(p.subWriters, writer)
	delete(p.subAnnounce, writer)
	for entity, guid := range p.subs {
		if guid != writer {
			continue
		}
		delete(p.subs, entity)
		p.seqByWriter[entitySEDPSubWriter]++
		dgram := buildMessage(p.prefix, buildDispose(entitySEDPSubWriter, p.seqByWriter[entitySEDPSubWriter], GUID{Prefix: p.prefix, EntityID: entity}))
		p.sendDiscoveryLocked(dgram)
	}
}

func (p *Participant) sendDiscoveryLocked(dgram []byte) {
	p.sendTo(&net.UDPAddr{IP: net.ParseIP(spdpMulticastAddr), Port: spdpMulticastPort(p.cfg.DomainID)}, dgram)
	seen := map[string]bool{}
	for _, locators := range p.peers {
		for _, loc := range locators {
			if addr, ok := loc.UDPAddr(); ok && !seen[addr.String()] {
				seen[addr.String()] = true
				p.sendTo(addr, dgram)
			}
		}
	}
}

// Callers serialize producers. Never wait for a consumer, and keep the newest four.
func enqueueSample(queue chan Sample, sample Sample) {
	select {
	case queue <- sample:
		return
	default:
	}
	select {
	case <-queue:
	default:
	}
	select {
	case queue <- sample:
	default:
	}
}

// nextSeq returns the next sequence number for one of our writers.
//
// Every RTPS writer has its own sequence-number space starting at 1. Sharing a
// counter across the SPDP and SEDP writers means the SEDP subscription is
// announced at whatever number SPDP has already reached — a reliable reader
// then sees samples 1..n-1 missing, waits for a replay that never comes, and
// never matches the subscription. The symptom is silence: discovery succeeds,
// the subscription is sent, and no user data ever arrives.
func (p *Participant) nextSeq(writer uint32) SequenceNumber {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seqByWriter[writer]++
	return p.seqByWriter[writer]
}

// subscriptionPayload builds the SEDP SubscriptionBuiltinTopicData describing
// our reader: BEST_EFFORT and VOLATILE, which matches a RELIABLE writer under
// the RxO rule without obliging us to run a reliable-reader state machine.
func (p *Participant) subscriptionPayload(reader GUID, ep Endpoint) []byte {
	b := newPLBuilder()
	b.addGUID(pidParticipantGUID, GUID{Prefix: p.prefix, EntityID: entityParticipant})
	b.addGUID(pidEndpointGUID, reader)
	b.addString(pidTopicName, ep.Topic)
	b.addString(pidTypeName, ep.Type)
	b.addReliability(reliabilityBestEffort)
	b.addUint32(pidDurability, 0) // VOLATILE
	port := p.ucast.LocalAddr().(*net.UDPAddr).Port
	b.addLocator(pidUnicastLocator, udpv4Locator(p.local, port))
	return b.finish()
}

// spdpDatagram builds our SPDP ParticipantBuiltinTopicData announcement.
func (p *Participant) spdpDatagram() []byte {
	port := p.ucast.LocalAddr().(*net.UDPAddr).Port
	loc := udpv4Locator(p.local, port)

	b := newPLBuilder()
	ver := make([]byte, 4)
	ver[0], ver[1] = 2, 2
	b.add(pidProtocolVersion, ver)
	vendor := make([]byte, 4)
	vendor[0], vendor[1] = 0x01, 0x0f
	b.add(pidVendorID, vendor)
	b.addGUID(pidParticipantGUID, GUID{Prefix: p.prefix, EntityID: entityParticipant})
	b.addLocator(pidMetatrafficUnicastLocator, loc)
	b.addLocator(pidDefaultUnicastLocator, loc)
	b.addUint32(pidBuiltinEndpointSet, builtinEndpoints)
	b.addDuration(pidParticipantLeaseDuration, leaseSeconds, 0)
	payload := b.finish()

	data := buildData(entitySPDPReader, entitySPDPWriter, p.nextSeq(entitySPDPWriter), payload)
	return buildMessage(p.prefix, data)
}

func (p *Participant) announce() {
	addr := &net.UDPAddr{
		IP:   net.ParseIP(spdpMulticastAddr),
		Port: spdpMulticastPort(p.cfg.DomainID),
	}
	p.sendTo(addr, p.spdpDatagram())

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, dgram := range p.subAnnounce {
		p.sendDiscoveryLocked(dgram)
	}
}

func (p *Participant) sendTo(addr *net.UDPAddr, pkt []byte) bool {
	if p.ucast == nil {
		return false
	}
	_, err := p.ucast.WriteToUDP(pkt, addr)
	return err == nil
}
