package localmesh

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/tsarelay"
)

type timestampReply struct {
	id      uint64
	data    []byte
	offset  int
	expires time.Time
	err     error
	pending bool
}

func (n *Node) timestampControl(p *nodePeer, m *TimestampControl) {
	now := time.Now()
	if p.timestamp != nil && !p.timestamp.expires.After(now) {
		p.timestamp = nil
	}
	if m.Op == "next" {
		if x := p.timestamp; x != nil && x.id == m.ID && x.data != nil && x.offset == int(m.Offset) {
			x.pending = true
			n.pumpTimestamp(p)
		}
		return
	}
	if m.Op != "request" || p.timestamp != nil {
		return
	}
	if n.timestampWorkers == nil {
		n.timestampWorkers = make(chan struct{}, 4)
	}
	select {
	case n.timestampWorkers <- struct{}{}:
	default:
		return
	}
	x := &timestampReply{id: m.ID, expires: now.Add(30 * time.Second)}
	p.timestamp = x
	query := append([]byte(nil), m.Data...)
	go func() {
		defer func() { <-n.timestampWorkers }()
		ctx, cancel := context.WithTimeout(n.ctx, 25*time.Second)
		defer cancel()
		go func() {
			select {
			case <-p.done:
				cancel()
			case <-ctx.Done():
			}
		}()
		data, err := tsarelay.Query(ctx, query)
		result := &timestampReply{id: m.ID, data: data, err: err, expires: x.expires, pending: true}
		select {
		case n.events <- nodeEvent{id: p.id, timestamp: result}:
		case <-ctx.Done():
		}
	}()
}
func (n *Node) pumpTimestamp(p *nodePeer) {
	x := p.timestamp
	if x == nil {
		return
	}
	if !x.expires.After(time.Now()) {
		p.timestamp = nil
		return
	}
	if !x.pending {
		return
	}
	m := &TimestampControl{ID: x.id, Op: "error"}
	end := x.offset
	if x.err == nil {
		end = min(len(x.data), x.offset+1024)
		m.Op = "reply"
		m.Offset = uint32(x.offset)
		m.Total = uint32(len(x.data))
		m.Data = x.data[x.offset:end]
	}
	if !n.queueReplies(p, []ControlMessage{{Kind: "time", Time: m}}) {
		return
	}
	x.offset = end
	x.pending = false
	if x.err != nil || end == len(x.data) {
		p.timestamp = nil
	}
}
