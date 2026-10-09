// Package mobile is a small binding-friendly facade. JSON is used only at the
// language boundary; the routing core uses typed events. No sockets are opened.
package mobile

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"strconv"
	"sync"
	"time"

	babel "github.com/wendylabsinc/WendyOS/babel"
)

type Engine struct {
	mu sync.Mutex
	e  *babel.Engine
}

// New takes an unsigned decimal RouterID, avoiding signed-64-bit mobile ABI loss.
func New(routerID string, initialSeqno int) (*Engine, error) {
	id, err := strconv.ParseUint(routerID, 10, 64)
	if err != nil {
		return nil, err
	}
	if initialSeqno < 0 || initialSeqno > 65535 {
		return nil, babel.ErrConfig
	}
	e, err := babel.New(babel.Config{RouterID: babel.RouterID(id), InitialSeqno: uint16(initialSeqno), IPv4ViaIPv6: true})
	if err != nil {
		return nil, err
	}
	return &Engine{e: e}, nil
}

type input struct {
	Type         string
	Link         babel.Link
	ID           babel.LinkID
	Prefix       netip.Prefix
	Metric, Cost uint16
	Packet       []byte
}

// Step accepts elapsed monotonic milliseconds and one JSON event. Event Type is
// tick/add-link/remove-link/cost/receive/originate/withdraw. Packet is base64 JSON.
// Outputs are JSON Effects; apply Revision/Routes then Commit before the next Step.
func (e *Engine) Step(nowMS int64, data []byte) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(data) > 100000 || nowMS < 0 || nowMS > (1<<63-1)/int64(time.Millisecond) {
		return nil, babel.ErrConfig
	}
	var v input
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	var ev babel.Event
	switch v.Type {
	case "tick":
		ev = babel.Tick{}
	case "add-link":
		ev = babel.AddLink{Link: v.Link}
	case "remove-link":
		ev = babel.RemoveLink{ID: v.ID}
	case "cost":
		ev = babel.SetCost{ID: v.ID, Cost: v.Cost}
	case "receive":
		ev = babel.Receive{ID: v.ID, Packet: v.Packet}
	case "originate":
		ev = babel.Originate{Prefix: v.Prefix, Metric: v.Metric}
	case "withdraw":
		ev = babel.Withdraw{Prefix: v.Prefix}
	default:
		return nil, babel.ErrConfig
	}
	fx, err := e.e.Step(time.Duration(nowMS)*time.Millisecond, ev)
	if err != nil {
		return nil, err
	}
	return json.Marshal(fx)
}
func (e *Engine) Commit(revision int64, success bool) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if revision <= 0 {
		return nil, babel.ErrRevision
	}
	fx, err := e.e.Commit(uint64(revision), success)
	if err != nil {
		return nil, err
	}
	return json.Marshal(fx)
}
func (e *Engine) Snapshot() ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return json.Marshal(e.e.Snapshot())
}

// Checkpoint must be durably saved before outgoing packets are sent when crash
// recovery matters. Treat this JSON as opaque (RouterIDs can exceed 53 bits).
func (e *Engine) Checkpoint() ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return json.Marshal(e.e.Checkpoint())
}

func Restore(data []byte) (*Engine, error) {
	if len(data) > 8<<20 {
		return nil, babel.ErrLimit
	}
	var state babel.Checkpoint
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	e, err := babel.Restore(babel.Config{RouterID: state.RouterID, IPv4ViaIPv6: true}, state)
	if err != nil {
		return nil, err
	}
	return &Engine{e: e}, nil
}

// NextDeadlineMS returns -1 when idle or awaiting Commit. Round up, never early.
func (e *Engine) NextDeadlineMS() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, ok := e.e.NextDeadline()
	if !ok {
		return -1
	}
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}
