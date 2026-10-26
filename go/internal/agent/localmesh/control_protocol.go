package localmesh

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Catalogue frames share the neighbour's authenticated control stream. They
// never enter a TUN or open a socket. A physical link incarnation is the scope
// of all delivery and send state; reconnects cannot inherit queued messages.
const MaxCatalogFrame = (1 << 10) + 8
const MaxCatalogMessage = 64 << 10

type ControlPeer struct {
	ID     babel.LinkID
	Device meshid.Device
}
type ControlEvent struct {
	Peer ControlPeer
	Data []byte
}
type controlEndpoint struct {
	peer  ControlPeer
	send  func(context.Context, []byte) error
	close func()
}
type ControlHub struct {
	mu            sync.Mutex
	peers         map[babel.LinkID]controlEndpoint
	events        chan ControlEvent
	closed        bool
	partial       map[babel.LinkID][]byte
	totals        map[babel.LinkID]int
	certificates  *CertificatePool
	announcements *netgossip.Pool
}

func NewControlHub(pools ...*CertificatePool) *ControlHub {
	pool := NewCertificatePool()
	if len(pools) != 0 && pools[0] != nil {
		pool = pools[0]
	}
	return &ControlHub{peers: make(map[babel.LinkID]controlEndpoint), events: make(chan ControlEvent, 128), partial: make(map[babel.LinkID][]byte), totals: make(map[babel.LinkID]int), certificates: pool, announcements: netgossip.NewPool()}
}

// Certificates shares public bytes, not identities or trust, with consumers of
// this node's authenticated neighbour channels. The pool is immutable here.
func (h *ControlHub) Announcements() *netgossip.Pool { return h.announcements }
func (h *ControlHub) Certificates() *CertificatePool { return h.certificates }
func (h *ControlHub) Events() <-chan ControlEvent    { return h.events }
func (h *ControlHub) Add(p ControlPeer, send func(context.Context, []byte) error, closePeer func()) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		closePeer()
		return
	}
	defer h.mu.Unlock()
	h.peers[p.ID] = controlEndpoint{p, send, closePeer}
	select {
	case h.events <- ControlEvent{Peer: p}:
	default:
	} // Peers is authoritative.
}
func (h *ControlHub) Remove(id babel.LinkID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p, ok := h.peers[id]; ok {
		delete(h.peers, id)
		delete(h.partial, id)
		delete(h.totals, id)
		select {
		case h.events <- ControlEvent{Peer: p.peer}:
		default:
		}
	}
}
func (h *ControlHub) Peers() []ControlPeer {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]ControlPeer, 0, len(h.peers))
	for _, p := range h.peers {
		out = append(out, p.peer)
	}
	return out
}
func (h *ControlHub) Deliver(id babel.LinkID, data []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.peers[id]
	if !ok || h.closed {
		return errors.New("catalogue link ended")
	}
	if len(data) <= 8 || len(data) > MaxCatalogFrame {
		return errors.New("invalid catalogue fragment size")
	}
	total, offset := int(binary.BigEndian.Uint32(data)), int(binary.BigEndian.Uint32(data[4:]))
	if total <= 0 || total > MaxCatalogMessage || offset+len(data)-8 > total {
		return errors.New("invalid catalogue message size")
	}
	if offset == 0 {
		if len(h.partial[id]) != 0 {
			return errors.New("interleaved catalogue message")
		}
		h.totals[id] = total
	}
	if total != h.totals[id] || offset != len(h.partial[id]) {
		return errors.New("unordered catalogue fragment")
	}
	h.partial[id] = append(h.partial[id], data[8:]...)
	if len(h.partial[id]) < total {
		return nil
	}
	body := h.partial[id]
	delete(h.partial, id)
	delete(h.totals, id)
	select {
	case h.events <- ControlEvent{Peer: p.peer, Data: body}:
		return nil
	default:
		return errors.New("catalogue receive queue full")
	}
}
func (h *ControlHub) Send(ctx context.Context, id babel.LinkID, data []byte) error {
	if len(data) == 0 || len(data) > MaxCatalogMessage {
		return errors.New("invalid catalogue message size")
	}
	h.mu.Lock()
	p, ok := h.peers[id]
	closed := h.closed
	h.mu.Unlock()
	if !ok || closed {
		return errors.New("catalogue link ended")
	}
	// One catalogue writer owns each endpoint, so fragments cannot interleave.
	for offset := 0; offset < len(data); {
		n := min(1<<10, len(data)-offset)
		frame := make([]byte, 8+n)
		binary.BigEndian.PutUint32(frame, uint32(len(data)))
		binary.BigEndian.PutUint32(frame[4:], uint32(offset))
		copy(frame[8:], data[offset:offset+n])
		if err := p.send(ctx, frame); err != nil {
			return err
		}
		offset += n
	}
	return nil
}
func (h *ControlHub) ClosePeer(id babel.LinkID) {
	h.mu.Lock()
	p, ok := h.peers[id]
	h.mu.Unlock()
	if ok {
		p.close()
	}
}
func (h *ControlHub) Close() {
	h.mu.Lock()
	h.closed = true
	h.peers = make(map[babel.LinkID]controlEndpoint)
	h.mu.Unlock()
}
