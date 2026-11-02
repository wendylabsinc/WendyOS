package localmesh

import (
	"context"
	"errors"
	"log"
	"sort"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
)

const maxClockRelays = 4

type clockResult struct {
	data []byte
	err  error
}
type clockCall struct {
	id     uint64
	server uint8
	nonce  []byte
	ctx    context.Context
	result chan clockResult
	peers  map[babel.LinkID]bool
}
type clockEvent struct {
	call   *clockCall
	cancel uint64
	peer   babel.LinkID
	reply  *ClockControl
	done   bool
	err    error
}

func (n *Node) wallTime() time.Time {
	if n.wallNow != nil {
		return n.wallNow()
	}
	return time.Now()
}

func (n *Node) clockBlocked(p *nodePeer) bool {
	if n.clockSync == nil {
		return false
	}
	if p.clockWaitingUntil.IsZero() {
		p.clockWaitingUntil = time.Now().Add(clockRecoveryWindow)
		log.Printf("wendy net clock recovery required peer=%q; waiting for authenticated authority proof", p.device.Principal())
	}
	n.startClockRecovery()
	return true
}

func (n *Node) startClockRecovery() {
	if n.clockSync == nil || n.clockRunning || time.Now().Before(n.clockNext) {
		return
	}
	blocked, relay := false, false
	for _, p := range n.peers {
		blocked = blocked || !p.clockWaitingUntil.IsZero()
		relay = relay || p.clockRelay
	}
	if !blocked || !relay {
		return
	}
	n.clockRunning = true
	n.clockNext = time.Now().Add(time.Minute)
	go func() {
		ctx, cancel := context.WithTimeout(n.ctx, clockRecoveryWindow)
		defer cancel()
		err := n.clockSync(ctx, n.relayClock)
		select {
		case n.events <- nodeEvent{clock: &clockEvent{done: true, err: err}}:
		case <-n.ctx.Done():
		}
	}()
}

func (n *Node) relayClock(ctx context.Context, server uint8, nonce []byte) ([]byte, error) {
	if len(nonce) != 32 {
		return nil, errors.New("invalid clock nonce")
	}
	id := n.clockSequence.Add(1)
	if id == 0 {
		return nil, errors.New("clock request counter exhausted")
	}
	call := &clockCall{id: id, server: server, nonce: append([]byte(nil), nonce...), ctx: ctx, result: make(chan clockResult, 1)}
	select {
	case n.events <- nodeEvent{clock: &clockEvent{call: call}}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.ctx.Done():
		return nil, n.ctx.Err()
	}
	select {
	case result := <-call.result:
		return result.data, result.err
	case <-ctx.Done():
		select {
		case n.events <- nodeEvent{clock: &clockEvent{cancel: id}}:
		case <-n.ctx.Done():
		}
		return nil, ctx.Err()
	case <-n.ctx.Done():
		return nil, n.ctx.Err()
	}
}

func (n *Node) handleClockEvent(event *clockEvent) error {
	if event.cancel != 0 {
		delete(n.clockCalls, event.cancel)
		return nil
	}
	if event.done {
		n.clockRunning = false
		if event.err != nil {
			log.Printf("wendy net clock recovery unavailable: %s", diagnosticText(event.err.Error(), 256))
			return nil
		}
		// A forward step can expire our previous local lease. Publish a new
		// ordinary signed lease and retry pending peer proofs at current UTC.
		n.nextPublication = time.Time{}
		if err := n.publish(); err != nil {
			return err
		}
		for _, p := range n.peers {
			if err := n.maintainClockPeer(p); err != nil {
				return err
			}
			n.queueReplies(p, []ControlMessage{{Kind: "clock", Clock: &ClockControl{Version: clockControlVersion, Op: "hello"}}})
		}
		return nil
	}
	if call := event.call; call != nil {
		if err := call.ctx.Err(); err != nil {
			call.result <- clockResult{err: err}
			return nil
		}
		if n.clockCalls == nil {
			n.clockCalls = make(map[uint64]*clockCall)
		}
		if len(n.clockCalls) >= len(roughtime.Servers) {
			call.result <- clockResult{err: errors.New("clock request capacity reached")}
			return nil
		}
		var peers []*nodePeer
		for _, p := range n.peers {
			if p.clockRelay {
				peers = append(peers, p)
			}
		}
		sort.Slice(peers, func(i, j int) bool {
			if peers[i].cost != peers[j].cost {
				return peers[i].cost < peers[j].cost
			}
			return peers[i].device.Principal() < peers[j].device.Principal()
		})
		call.peers = make(map[babel.LinkID]bool)
		principals := make(map[string]bool)
		for _, p := range peers {
			if len(call.peers) >= maxClockRelays {
				break
			}
			principal := p.device.Principal()
			if principals[principal] {
				continue
			}
			select {
			case <-p.done:
				continue
			default:
			}
			message := ControlMessage{Kind: "clock", Clock: &ClockControl{Version: clockControlVersion, Op: "request", ID: call.id, Server: call.server, Nonce: call.nonce}}
			if n.queueReplies(p, []ControlMessage{message}) {
				call.peers[p.id] = true
				principals[principal] = true
			}
		}
		if len(call.peers) == 0 {
			call.result <- clockResult{err: errors.New("no live signed-time relay")}
			return nil
		}
		n.clockCalls[call.id] = call
		return nil
	}
	if event.reply != nil {
		p := n.peers[event.peer]
		if p == nil {
			return nil
		}
		delete(p.clockServing, event.reply.ID)
		if !n.queueReplies(p, []ControlMessage{{Kind: "clock", Clock: event.reply}}) {
			n.closePeer(p, "clock-reply-queue", errors.New("clock reply reserve full"))
		}
	}
	return nil
}

func (n *Node) clockControl(p *nodePeer, m *ClockControl) error {
	if !m.supported() {
		return nil
	}
	if m.Op == "hello" {
		p.clockRelay = true
		n.reconcilePeer(p, n.wallTime())
		n.startClockRecovery()
		return nil
	}
	if m.Op == "reply" || m.Op == "error" {
		call := n.clockCalls[m.ID]
		if call == nil || call.server != m.Server || !call.peers[p.id] {
			return nil
		}
		delete(call.peers, p.id)
		if m.Op == "reply" {
			delete(n.clockCalls, m.ID)
			call.result <- clockResult{data: append([]byte(nil), m.Data...)}
		} else if len(call.peers) == 0 {
			delete(n.clockCalls, m.ID)
			call.result <- clockResult{err: errors.New("signed-time relay unavailable")}
		}
		return nil
	}
	if m.Op != "request" {
		return nil
	}
	if int(m.Server) >= len(roughtime.Servers) {
		if !n.queueReplies(p, []ControlMessage{{Kind: "clock", Clock: &ClockControl{Version: clockControlVersion, Op: "error", ID: m.ID, Server: m.Server}}}) {
			n.closePeer(p, "clock-reply-queue", errors.New("clock reply reserve full"))
		}
		return nil
	}
	if p.clockServing == nil {
		p.clockServing = make(map[uint64]bool)
	}
	if p.clockServing[m.ID] {
		return nil
	}
	if n.clockWorkers == nil {
		n.clockWorkers = make(chan struct{}, 4)
	}
	if len(p.clockServing) >= len(roughtime.Servers) {
		return nil
	}
	select {
	case n.clockWorkers <- struct{}{}:
	default:
		return nil
	}
	p.clockServing[m.ID] = true
	id, index, nonce := m.ID, m.Server, append([]byte(nil), m.Nonce...)
	go func() {
		defer func() { <-n.clockWorkers }()
		ctx, cancel := context.WithTimeout(n.ctx, 5*time.Second)
		defer cancel()
		go func() {
			select {
			case <-p.done:
				cancel()
			case <-ctx.Done():
			}
		}()
		result, err := n.queryClockAuthority(ctx, index, nonce)
		reply := &ClockControl{Version: clockControlVersion, Op: "error", ID: id, Server: index}
		if err == nil && len(result) > 0 && len(result) <= 4096 {
			reply.Op = "reply"
			reply.Data = result
		}
		select {
		case n.events <- nodeEvent{clock: &clockEvent{peer: p.id, reply: reply}}:
		case <-n.ctx.Done():
		}
	}()
	return nil
}

func (n *Node) queryClockAuthority(ctx context.Context, index uint8, nonce []byte) ([]byte, error) {
	if n.clockQuery != nil {
		return n.clockQuery(ctx, index, nonce)
	}
	result, err := roughtime.QueryServerWithNonce(ctx, roughtime.Servers[index], nonce)
	return result.RawResponse, err
}

func (n *Node) maintainClockPeer(p *nodePeer) error {
	if p.sync == nil {
		return nil
	}
	if !p.clockWaitingUntil.IsZero() && !p.clockWaitingUntil.After(time.Now()) {
		n.closePeer(p, "clock-recovery", errors.New("authenticated time synchronization required; no verified authority quorum within recovery window"))
		return nil
	}
	replies, changed, err := p.sync.ContinueClockPending(n.wallTime(), directoryResponseSlots(p))
	if err != nil {
		n.closePeer(p, "clock-recovery-admission", err)
		return nil
	}
	if !n.queueReplies(p, replies) {
		n.closePeer(p, "clock-recovery-queue", errors.New("directory reply reserve full"))
		return nil
	}
	if len(p.sync.pendingClock) == 0 {
		p.clockWaitingUntil = time.Time{}
	}
	for _, record := range changed {
		for id, dest := range n.peers {
			if id != p.id && dest.sync != nil {
				n.queue(dest, dest.sync.Record(record, n.wallTime()))
			}
		}
	}
	if len(changed) > 0 {
		return n.step(babel.Tick{})
	}
	return nil
}
