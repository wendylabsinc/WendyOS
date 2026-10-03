// Package sim provides a deterministic virtual network with atomic forwarding
// commits. It models control delivery independently of the forwarding graph.
package sim

import (
	"fmt"
	"net/netip"
	"time"

	babel "github.com/wendylabsinc/WendyOS/babel"
)

type Endpoint struct {
	Node int
	Link babel.LinkID
}
type Packet struct {
	From, To Endpoint
	Payload  []byte
	At       time.Duration
}
type Network struct {
	Nodes  []*babel.Engine
	Peers  map[Endpoint]Endpoint
	FIB    [][]babel.Route
	Now    time.Duration
	Queue  []Packet
	NextID babel.LinkID
	// Fault may drop, delay, duplicate or reorder by modifying Queue separately.
	Fault                 func(Packet) bool // true drops
	Packets, Bytes, Steps uint64
}

func New(n int) (*Network, error) {
	s := &Network{Peers: map[Endpoint]Endpoint{}, FIB: make([][]babel.Route, n)}
	for i := 0; i < n; i++ {
		e, err := babel.New(babel.Config{RouterID: babel.RouterID(i + 1), Seed: uint64(i + 1), HelloInterval: time.Second, UpdateInterval: 4 * time.Second, IPv4ViaIPv6: true})
		if err != nil {
			return nil, err
		}
		s.Nodes = append(s.Nodes, e)
	}
	return s, nil
}
func (s *Network) Connect(a, b int, cost uint16) (Endpoint, Endpoint, error) {
	s.NextID++
	x := Endpoint{a, s.NextID}
	s.NextID++
	y := Endpoint{b, s.NextID}
	s.Peers[x] = y
	s.Peers[y] = x
	for _, p := range []struct {
		x           Endpoint
		local, peer string
	}{{x, "fe80::1", "fe80::2"}, {y, "fe80::2", "fe80::1"}} {
		err := s.Event(p.x.Node, babel.AddLink{Link: babel.Link{ID: p.x.Link, Local: netip.MustParseAddr(p.local), Peer: netip.MustParseAddr(p.peer), Cost: cost}})
		if err != nil {
			return x, y, err
		}
	}
	return x, y, nil
}
func (s *Network) Disconnect(x Endpoint) error {
	y, ok := s.Peers[x]
	if !ok {
		return nil
	}
	delete(s.Peers, x)
	delete(s.Peers, y)
	if err := s.Event(x.Node, babel.RemoveLink{ID: x.Link}); err != nil {
		return err
	}
	return s.Event(y.Node, babel.RemoveLink{ID: y.Link})
}
func (s *Network) Event(n int, ev babel.Event) error {
	fx, err := s.Nodes[n].Step(s.Now, ev)
	if err != nil {
		return err
	}
	s.Steps++
	if fx.Revision != 0 {
		s.FIB[n] = fx.Routes
		fx, err = s.Nodes[n].Commit(fx.Revision, true)
		if err != nil {
			return err
		}
	}
	for _, d := range fx.Datagrams {
		from := Endpoint{n, d.Link}
		to, ok := s.Peers[from]
		if !ok {
			continue
		}
		p := Packet{from, to, d.Payload, s.Now + time.Millisecond}
		s.Packets++
		s.Bytes += uint64(len(d.Payload))
		if s.Fault == nil || !s.Fault(p) {
			s.Queue = append(s.Queue, p)
		}
	}
	return nil
}
func (s *Network) Run(until time.Duration, check func(*Network) error) error {
	for turns := 0; turns < 10000000; turns++ {
		next := until + 1
		node := -1
		pi := -1
		for i, e := range s.Nodes {
			if d, ok := e.NextDeadline(); ok && d < next {
				next = d
				node = i
			}
		}
		for i, p := range s.Queue {
			if p.At < next {
				next = p.At
				pi = i
				node = -1
			}
		}
		if next > until {
			s.Now = until
			return nil
		}
		s.Now = next
		var err error
		if pi >= 0 {
			p := s.Queue[pi]
			s.Queue = append(s.Queue[:pi], s.Queue[pi+1:]...)
			if peer, ok := s.Peers[p.From]; ok && peer == p.To {
				err = s.Event(p.To.Node, babel.Receive{ID: p.To.Link, Packet: p.Payload})
			}
		} else {
			err = s.Event(node, babel.Tick{})
		}
		if err != nil {
			return err
		}
		if check != nil {
			if err = check(s); err != nil {
				return fmt.Errorf("at %s: %w", s.Now, err)
			}
		}
	}
	return fmt.Errorf("simulation event limit reached")
}

// Walk follows applied longest-prefix routes. A drop is not a route via default.
func (s *Network) Walk(start int, dst netip.Addr) (int, error) {
	seen := map[int]bool{}
	n := start
	for hops := 0; hops <= len(s.Nodes); hops++ {
		if seen[n] {
			return -1, fmt.Errorf("forwarding loop from %d for %s at %d", start, dst, n)
		}
		seen[n] = true
		var best *babel.Route
		for i := range s.FIB[n] {
			r := &s.FIB[n][i]
			if r.Prefix.Contains(dst) && (best == nil || r.Prefix.Bits() > best.Prefix.Bits()) {
				best = r
			}
		}
		if best == nil || best.Unreachable {
			return -1, nil
		}
		if best.Local {
			return n, nil
		}
		p, ok := s.Peers[Endpoint{n, best.Link}]
		if !ok {
			return -1, nil
		}
		n = p.Node
	}
	return -1, fmt.Errorf("hop limit")
}
