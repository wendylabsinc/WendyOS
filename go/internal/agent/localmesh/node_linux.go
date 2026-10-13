//go:build linux

package localmesh

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/WendyOS/babel"
)

type nodePeer struct {
	id      babel.LinkID
	asset   int32
	cost    uint16
	conn    *quic.Conn
	tun     *os.File
	control chan ControlMessage
	sync    *Synchronizer
}
type nodeEvent struct {
	add     *nodePeer
	id      babel.LinkID
	remove  bool
	packet  []byte
	control *ControlMessage
	uplink  *string
	roam    *bool
	result  chan error
}

// Node serializes all Babel/directory mutations. Radio providers only establish
// authenticated QUIC connections and pass them to Attach; no NAN concepts appear
// in routing or service synchronization. Parallel links have distinct LinkIDs.
type Node struct {
	Credentials     *Credentials
	kernel          *Kernel
	cache           *IdentityCache
	directory       *Directory
	bundleHints     *bundleHintCache
	store           *StateStore
	state           PersistentState
	routing         *Routing
	peers           map[babel.LinkID]*nodePeer
	events          chan nodeEvent
	ctx             context.Context
	cancel          context.CancelFunc
	started         time.Time
	nextLink        babel.LinkID
	advertisement   Manifest
	view            atomic.Pointer[NodeSnapshot]
	gate            sync.RWMutex
	paused, stopped bool
	forwardRoutes   []babel.Route
	gateways        map[babel.RouterID]bool
	exit            string
	roam            bool
}

func NewNode(parent context.Context, dir string, credentials *Credentials, name string, agentPort uint16) (*Node, error) {
	if credentials == nil || agentPort == 0 {
		return nil, errors.New("missing local-mesh credentials or agent port")
	}
	now := time.Now()
	store, state, err := OpenStateStore(filepath.Join(dir, "routing-state.json"), credentials.Org, credentials.Asset)
	if err != nil {
		return nil, err
	}
	cache, err := OpenIdentityCache(filepath.Join(dir, "identity-cache.json"), DefaultCacheLimits(), credentials.Verify, now)
	if err != nil {
		return nil, err
	}
	if _, err = cache.Put(credentials.Certificate.Certificate, now); err != nil {
		return nil, err
	}
	directory, err := NewDirectory(credentials.Org, 1024, cache)
	if err != nil {
		return nil, err
	}
	kernel, err := NewKernel(credentials.Org, credentials.Asset)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	n := &Node{Credentials: credentials, kernel: kernel, cache: cache, directory: directory, bundleHints: newBundleHintCache(), store: store, state: state, peers: map[babel.LinkID]*nodePeer{}, events: make(chan nodeEvent, 256), ctx: ctx, cancel: cancel, started: now,
		advertisement: Manifest{Version: 1, Org: credentials.Org, Asset: credentials.Asset, Name: name, AgentPort: agentPort}, gateways: map[babel.RouterID]bool{}}
	ok := false
	defer func() {
		if !ok {
			cancel()
			_ = kernel.Close()
		}
	}()
	if err = directory.SetPersistence(state.Receipts, func(receipts []DirectoryReceipt) error {
		next := n.state
		next.Receipts = receipts
		if err := n.store.Save(next); err != nil {
			return err
		}
		n.state = next
		return nil
	}, now); err != nil {
		return nil, err
	}
	id, _ := RouterID(credentials.Org, credentials.Asset)
	config := babel.Config{RouterID: id, IPv4ViaIPv6: true, HelloInterval: time.Second, UpdateInterval: 4 * time.Second, AcceptRoute: func(origin babel.RouterID, prefix netip.Prefix) bool {
		// Address-allocation validation is independent of a neighbour's claims.
		// Default gateway admission is added by the host policy, not arbitrary
		// remote prefixes. This initial event loop only originates host routes.
		return prefix.Addr().Is4() && (OwnedPrefix(credentials.Org, origin, prefix) || (prefix.Bits() == 0 && n.gateways[origin]))
	}}
	n.routing, err = NewRouting(config, state.Babel, n)
	if err != nil {
		return nil, err
	}
	if state.Babel != nil {
		n.nextLink = state.Babel.LastLink
	}
	n.view.Store(&NodeSnapshot{})
	ok = true
	return n, nil
}

func (n *Node) Snapshot() NodeSnapshot {
	v := n.view.Load()
	if v == nil {
		return NodeSnapshot{}
	}
	out := *v
	out.Routes = append([]babel.Route(nil), v.Routes...)
	out.Devices = append([]Manifest(nil), v.Devices...)
	return out
}

func (n *Node) refresh() {
	devices := n.directory.Snapshot(time.Now())
	n.gateways = map[babel.RouterID]bool{}
	for _, m := range devices {
		if m.Internet {
			id, _ := RouterID(m.Org, m.Asset)
			n.gateways[id] = true
		}
	}
	n.view.Store(&NodeSnapshot{Peers: len(n.peers), Devices: devices, Routes: n.routing.Snapshot().Routes})
}

func (n *Node) Run() error {
	defer n.cancel()
	defer func() {
		_ = n.Stop()
		for _, p := range n.peers {
			_ = p.conn.CloseWithError(0, "local-mesh stopping")
		}
		_ = n.kernel.Close()
		_ = n.cache.Flush(time.Now())
	}()
	// NetworkManager can race dummy creation before its unmanaged-device policy
	// takes effect. Verify the base state once before originating Babel routes,
	// then keep repairing it as the agent runs.
	if err := n.kernel.ReconcileBase(); err != nil {
		return err
	}
	v4, _, _ := Addresses(n.Credentials.Org, n.Credentials.Asset)
	if err := n.step(babel.Originate{Prefix: netip.PrefixFrom(v4, 32)}); err != nil {
		return err
	}
	if err := n.publish(); err != nil {
		return err
	}
	timer := time.NewTimer(time.Millisecond)
	defer timer.Stop()
	maintenance := time.NewTicker(time.Second)
	defer maintenance.Stop()
	renew := time.NewTicker(20 * time.Second)
	defer renew.Stop()
	reconcile := time.NewTicker(30 * time.Second)
	defer reconcile.Stop()
	for {
		delay := time.Second
		if deadline, ok := n.routing.NextDeadline(); ok {
			delay = max(time.Millisecond, deadline-time.Since(n.started))
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(delay)
		select {
		case <-n.ctx.Done():
			return n.ctx.Err()
		case <-timer.C:
			if err := n.step(babel.Tick{}); err != nil {
				return err
			}
		case <-maintenance.C:
			n.gate.Lock()
			err := n.kernel.ReconcileBase()
			n.gate.Unlock()
			if err != nil {
				return err
			}
			n.refresh()
			if err := n.step(babel.Tick{}); err != nil {
				return err
			}
		case <-renew.C:
			if err := n.publish(); err != nil {
				return err
			}
		case <-reconcile.C:
			for _, p := range n.peers {
				n.queue(p, p.sync.Reconcile(time.Now()))
			}
		case event := <-n.events:
			err := n.handle(event)
			if event.result != nil {
				event.result <- err
			}
			if err != nil && event.result == nil {
				return err
			}
			if errors.Is(err, babel.ErrStopped) {
				return err
			}
		}
	}
}

func (n *Node) step(event babel.Event) error {
	if err := n.routing.Step(time.Since(n.started), event); err != nil {
		return errors.Join(babel.ErrStopped, err)
	}
	n.refresh()
	return nil
}

func (n *Node) handle(e nodeEvent) error {
	if e.roam != nil {
		n.gate.Lock()
		n.roam = *e.roam
		err := n.applyRoam()
		n.gate.Unlock()
		return err
	}
	if e.uplink != nil {
		n.gate.Lock()
		err := n.kernel.SetExit(*e.uplink)
		n.gate.Unlock()
		if err != nil {
			return err
		}
		if n.exit == *e.uplink {
			return nil
		}
		n.exit = *e.uplink
		n.advertisement.Internet = n.exit != ""
		var event babel.Event = babel.Withdraw{Prefix: netip.MustParsePrefix("0.0.0.0/0")}
		if n.exit != "" {
			event = babel.Originate{Prefix: netip.MustParsePrefix("0.0.0.0/0")}
		}
		if err = n.step(event); err != nil {
			return err
		}
		return n.publish()
	}
	if e.add != nil {
		if len(n.peers) >= 64 {
			return errors.New("local-mesh link capacity reached")
		}
		n.nextLink++
		e.add.id = n.nextLink
		tun, err := n.kernel.AddPeer(e.add.id, e.add.asset)
		if err != nil {
			return err
		}
		e.add.tun = tun
		e.add.sync = NewSynchronizer(n.directory, n.cache)
		e.add.sync.SeedKnownBundles(n.bundleHints.Known(e.add.asset, time.Now()))
		n.peers[e.add.id] = e.add
		local, peer, _ := LinkAddresses(n.Credentials.Asset, e.add.asset)
		if err = n.step(babel.AddLink{Link: babel.Link{ID: e.add.id, Local: local, Peer: peer, Cost: e.add.cost, MaxPacket: DatagramLimit - 1}}); err != nil {
			return err
		}
		n.queue(e.add, e.add.sync.Reconcile(time.Now()))
		return nil
	}
	p := n.peers[e.id]
	if p == nil {
		return nil
	}
	if e.remove {
		if err := n.step(babel.RemoveLink{ID: e.id}); err != nil {
			return err
		}
		delete(n.peers, e.id)
		_ = n.kernel.RemovePeer(e.id)
		n.refresh()
		return nil
	}
	if e.packet != nil {
		return n.step(babel.Receive{ID: e.id, Packet: e.packet})
	}
	if e.control != nil {
		replies, changed, err := p.sync.Receive(*e.control, time.Now())
		if err != nil {
			_ = p.conn.CloseWithError(1, "invalid directory message")
			return nil
		}
		n.queue(p, replies)
		for _, record := range changed {
			for id, dest := range n.peers {
				if id != p.id {
					n.queue(dest, dest.sync.Record(record, time.Now()))
				}
			}
		}
		n.refresh()
	}
	return nil
}

func (n *Node) queue(p *nodePeer, messages []ControlMessage) {
	for _, m := range messages {
		select {
		case p.control <- m:
		default: /* periodic reconciliation repairs congestion */
		}
	}
}

func (n *Node) publish() error {
	if _, _, ok := n.cache.Get(Fingerprint(n.Credentials.Certificate.Certificate), time.Now()); !ok {
		if _, err := n.cache.Put(n.Credentials.Certificate.Certificate, time.Now()); err != nil {
			return err
		}
	}
	if n.state.Revision == ^uint64(0) {
		return errors.New("origin revision exhausted")
	}
	n.state.Revision++
	if err := n.store.Save(n.state); err != nil {
		return err
	}
	m := n.advertisement
	now := time.Now()
	m.Revision = n.state.Revision
	m.Issued = now.UnixMilli()
	m.Expires = now.Add(time.Minute).UnixMilli()
	w, err := SignManifest(m, n.Credentials.Certificate.Certificate, n.Credentials.Signer, now)
	if err != nil {
		return err
	}
	if _, err = n.directory.Accept(w, now); err != nil {
		return err
	}
	for _, p := range n.peers {
		n.queue(p, p.sync.Record(w, now))
	}
	n.refresh()
	return nil
}

// SetUplink follows successful preparation of the selected sharing NAT/DNS.
func (n *Node) SetUplink(ctx context.Context, iface string) error {
	return n.request(ctx, nodeEvent{uplink: &iface})
}

func (n *Node) SetRoaming(ctx context.Context, enabled bool) error {
	return n.request(ctx, nodeEvent{roam: &enabled})
}

func (n *Node) applyRoam() error {
	if n.roam {
		for _, r := range n.forwardRoutes {
			if r.Prefix.Bits() == 0 && !r.Local && !r.Unreachable {
				return n.kernel.SetRoam(&r)
			}
		}
	}
	return n.kernel.SetRoam(nil)
}

func (n *Node) request(ctx context.Context, e nodeEvent) error {
	e.result = make(chan error, 1)
	select {
	case n.events <- e:
	case <-ctx.Done():
		return ctx.Err()
	case <-n.ctx.Done():
		return n.ctx.Err()
	}
	select {
	case err := <-e.result:
		return err
	case <-n.ctx.Done():
		return n.ctx.Err()
	}
}

// Attach owns the authenticated connection until return. No kernel interface is
// created until protocol negotiation succeeds. Caller must use Credentials.PeerTLS.
func (n *Node) Attach(ctx context.Context, asset int32, conn *quic.Conn) error {
	return n.AttachWithCost(ctx, asset, conn, 256)
}

// AttachWithCost adds an authenticated carrier link with its Babel metric.
// TCP uses 256, NAN 512, and slower carriers may supply a higher cost.
func (n *Node) AttachWithCost(ctx context.Context, asset int32, conn *quic.Conn, cost uint16) error {
	if cost == 0 || cost == ^uint16(0) {
		return errors.New("invalid local-mesh link cost")
	}
	defer conn.CloseWithError(0, "local-mesh link ended")
	stream, err := OpenControl(ctx, conn, n.Credentials.Org, n.Credentials.Asset, asset)
	if err != nil {
		return err
	}
	p := &nodePeer{asset: asset, cost: cost, conn: conn, control: make(chan ControlMessage, 128)}
	if err = n.request(ctx, nodeEvent{add: p}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-n.ctx.Done():
			cancel()
		case <-ctx.Done():
		}
		_ = conn.CloseWithError(0, "link stopped")
	}()
	var wg sync.WaitGroup
	done := make(chan error, 4)
	worker := func(f func() error) { wg.Add(1); go func() { defer wg.Done(); done <- f() }() }
	worker(func() error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case m := <-p.control:
				if err := WriteControl(stream, m); err != nil {
					return err
				}
				if m.Kind == "bundle" {
					n.bundleHints.Written(p.asset, m.Bundle, time.Now())
				}
			}
		}
	})
	worker(func() error {
		for {
			m, err := ReadControl(stream)
			if err != nil {
				return err
			}
			select {
			case n.events <- nodeEvent{id: p.id, control: &m}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
	worker(func() error { return n.readTUN(ctx, p) })
	worker(func() error { return n.readQUIC(ctx, p) })
	err = <-done
	// Keep the TUN alive while Babel withdraws its routes. Closing its last
	// file descriptor first destroys the kernel link, making route deletion
	// fail with ENODEV and stopping the shared routing engine.
	removeErr := n.request(n.ctx, nodeEvent{id: p.id, remove: true})
	cancel()
	_ = p.tun.Close()
	wg.Wait()
	return errors.Join(err, removeErr)
}

func (n *Node) readTUN(ctx context.Context, p *nodePeer) error {
	buf := make([]byte, TunnelMTU+1)
	var sequence uint32
	for {
		size, err := p.tun.Read(buf)
		if err != nil {
			return err
		}
		packet := buf[:size]
		if !validIP(packet) || packet[0]>>4 != 4 {
			continue
		}
		n.gate.RLock()
		allowed := !n.stopped && forwardLink(n.forwardRoutes, netip.AddrFrom4([4]byte(packet[16:20]))) == p.id
		if allowed {
			sequence++
			fragments, _ := EncodeIP(sequence, packet)
			for _, d := range fragments {
				_ = p.conn.SendDatagram(d)
			}
		}
		n.gate.RUnlock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func (n *Node) readQUIC(ctx context.Context, p *nodePeer) error {
	var assembler IPAssembler
	for {
		d, err := p.conn.ReceiveDatagram(ctx)
		if err != nil {
			return err
		}
		if len(d) == 0 || len(d) > DatagramLimit {
			return errors.New("invalid local-mesh datagram")
		}
		switch d[0] {
		case PacketBabel:
			select {
			case n.events <- nodeEvent{id: p.id, packet: append([]byte(nil), d[1:]...)}:
			case <-ctx.Done():
				return ctx.Err()
			}
		case PacketIP:
			packet, err := assembler.Receive(d, time.Now())
			if err != nil {
				return err
			}
			if packet == nil {
				continue
			}
			if packet[0]>>4 != 4 {
				continue
			}
			n.gate.RLock()
			if !n.stopped {
				_, err = p.tun.Write(packet)
			}
			n.gate.RUnlock()
			if err != nil {
				return err
			}
		default:
			return errors.New("unknown local-mesh datagram kind")
		}
	}
}

func forwardLink(routes []babel.Route, dst netip.Addr) babel.LinkID {
	best := -1
	var link babel.LinkID
	for _, r := range routes {
		if r.Prefix.Contains(dst) && r.Prefix.Bits() > best {
			best = r.Prefix.Bits()
			link = r.Link
			if r.Local || r.Unreachable {
				link = 0
			}
		}
	}
	return link
}

func (n *Node) Apply(routes []babel.Route) error {
	n.gate.Lock()
	n.paused = true
	if err := n.kernel.Apply(routes); err != nil {
		return err
	}
	n.forwardRoutes = append([]babel.Route(nil), routes...)
	return n.applyRoam()
}
func (n *Node) Persist(cp babel.Checkpoint) error { n.state.Babel = &cp; return n.store.Save(n.state) }
func (n *Node) Resume() error {
	if n.paused {
		n.paused = false
		n.gate.Unlock()
	}
	return nil
}
func (n *Node) Stop() error {
	if !n.paused {
		n.gate.Lock()
	}
	n.stopped = true
	err := n.kernel.Stop()
	n.paused = false
	n.gate.Unlock()
	return err
}
func (n *Node) Send(d babel.Datagram) error {
	p := n.peers[d.Link]
	if p == nil {
		return fmt.Errorf("missing link %d", d.Link)
	}
	return p.conn.SendDatagram(append([]byte{PacketBabel}, d.Payload...))
}
