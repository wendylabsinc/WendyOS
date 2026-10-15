package sim

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	babel "github.com/wendylabsinc/WendyOS/babel"
)

func prefix(n int) netip.Prefix { return netip.MustParsePrefix(fmt.Sprintf("fd00::%x/128", n+1)) }
func noLoops(s *Network) error {
	for n := range s.Nodes {
		for d := range s.Nodes {
			if _, err := s.Walk(n, prefix(d).Addr()); err != nil {
				return err
			}
		}
	}
	return nil
}
func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func checkReachable(t testing.TB, s *Network) {
	t.Helper()
	for n := range s.Nodes {
		for d := range s.Nodes {
			got, err := s.Walk(n, prefix(d).Addr())
			if err != nil || got != d {
				t.Fatalf("%d -> %d got %d: %v, state=%+v", n, d, got, err, s.Nodes[n].Snapshot())
			}
		}
	}
}
func TestTopologies(t *testing.T) {
	for _, edges := range [][][2]int{{{0, 1}, {1, 2}}, {{0, 1}, {1, 2}, {2, 0}}, {{0, 1}, {0, 2}, {1, 3}, {2, 3}}, {{0, 1}, {1, 2}, {2, 3}, {3, 0}}} {
		t.Run(fmt.Sprint(edges), func(t *testing.T) {
			n := 0
			for _, e := range edges {
				n = max(n, max(e[0], e[1])+1)
			}
			s, err := New(n)
			must(t, err)
			var first Endpoint
			for i, e := range edges {
				x, _, err := s.Connect(e[0], e[1], 96)
				must(t, err)
				if i == 0 {
					first = x
				}
			}
			for i := range s.Nodes {
				must(t, s.Event(i, babel.Originate{Prefix: prefix(i)}))
			}
			must(t, s.Run(30*time.Second, noLoops))
			checkReachable(t, s)
			must(t, s.Disconnect(first))
			must(t, s.Run(60*time.Second, noLoops))
			if len(edges) >= n {
				checkReachable(t, s)
			}
		})
	}
}
func TestLossPartitionRejoin(t *testing.T) {
	s, err := New(4)
	must(t, err)
	for i := 0; i < 4; i++ {
		_, _, err := s.Connect(i, (i+1)%4, 96)
		must(t, err)
		must(t, s.Event(i, babel.Originate{Prefix: prefix(i)}))
	}
	count := 0
	s.Fault = func(p Packet) bool { count++; return count%5 == 0 }
	must(t, s.Run(40*time.Second, noLoops))
	checkReachable(t, s)
	s.Fault = func(p Packet) bool { return (p.From.Node < 2) != (p.To.Node < 2) }
	must(t, s.Run(65*time.Second, noLoops))
	s.Fault = nil
	must(t, s.Run(100*time.Second, noLoops))
	checkReachable(t, s)
}
func TestIPv4AndMultipleDefaults(t *testing.T) {
	s, err := New(4)
	must(t, err)
	for i := 0; i < 3; i++ {
		_, _, err := s.Connect(i, i+1, 96)
		must(t, err)
	}
	p := netip.MustParsePrefix("0.0.0.0/0")
	must(t, s.Event(0, babel.Originate{Prefix: p}))
	must(t, s.Event(3, babel.Originate{Prefix: p}))
	must(t, s.Run(30*time.Second, nil))
	for i := range s.Nodes {
		n, err := s.Walk(i, netip.MustParseAddr("1.1.1.1"))
		if err != nil || (n != 0 && n != 3) {
			t.Fatal(i, n, err)
		}
	}
	must(t, s.Event(0, babel.Withdraw{Prefix: p}))
	must(t, s.Event(3, babel.Withdraw{Prefix: p}))
	must(t, s.Run(60*time.Second, nil))
	for i := range s.Nodes {
		n, err := s.Walk(i, netip.MustParseAddr("1.1.1.1"))
		if err != nil || n != -1 {
			t.Fatal(i, n, err)
		}
	}
}
func TestExhaustiveFourNodeGraphs(t *testing.T) {
	edges := [][2]int{{0, 1}, {0, 2}, {0, 3}, {1, 2}, {1, 3}, {2, 3}}
	for mask := 1; mask < 64; mask++ {
		t.Run(fmt.Sprint(mask), func(t *testing.T) {
			s, err := New(4)
			must(t, err)
			var links []Endpoint
			for i, e := range edges {
				if mask&(1<<i) != 0 {
					x, _, err := s.Connect(e[0], e[1], uint16(96+i))
					must(t, err)
					links = append(links, x)
				}
			}
			for i := range s.Nodes {
				must(t, s.Event(i, babel.Originate{Prefix: prefix(i)}))
			}
			must(t, s.Run(25*time.Second, noLoops))
			for _, x := range links {
				must(t, s.Disconnect(x))
				must(t, s.Run(s.Now+5*time.Second, noLoops))
			}
		})
	}
}
func BenchmarkHundredNodeRing(b *testing.B) {
	for n := 0; n < b.N; n++ {
		s, err := New(100)
		must(b, err)
		for i := 0; i < 100; i++ {
			_, _, err := s.Connect(i, (i+1)%100, 96)
			must(b, err)
			must(b, s.Event(i, babel.Originate{Prefix: prefix(i)}))
		}
		must(b, s.Run(30*time.Second, nil))
		checkReachable(b, s)
		b.ReportMetric(float64(s.Bytes)/100/30, "control-B/node/s")
	}
}

func TestOverlappingDefaultHoldDown(t *testing.T) {
	s, err := New(3)
	must(t, err)
	_, _, err = s.Connect(0, 1, 96)
	must(t, err)
	x, _, err := s.Connect(1, 2, 96)
	must(t, err)
	must(t, s.Event(0, babel.Originate{Prefix: netip.MustParsePrefix("::/0")}))
	must(t, s.Event(2, babel.Originate{Prefix: prefix(2)}))
	must(t, s.Run(20*time.Second, nil))
	must(t, s.Disconnect(x))
	// Destination formerly behind node 1 must not follow node 1's default
	// back to node 0 while node 0 still points at node 1 for the specific.
	for _, n := range []int{0, 1} {
		_, err = s.Walk(n, prefix(2).Addr())
		must(t, err)
	}
	must(t, s.Run(25*time.Second, func(s *Network) error {
		for _, n := range []int{0, 1} {
			if _, err := s.Walk(n, prefix(2).Addr()); err != nil {
				return err
			}
		}
		return nil
	}))
}

func TestShortestPathsAllFourNodeGraphs(t *testing.T) {
	edges := [][2]int{{0, 1}, {0, 2}, {0, 3}, {1, 2}, {1, 3}, {2, 3}}
	for mask := 1; mask < 64; mask++ {
		s, err := New(4)
		must(t, err)
		var dist [4][4]int
		for i := 0; i < 4; i++ {
			for j := 0; j < 4; j++ {
				if i != j {
					dist[i][j] = 1 << 20
				}
			}
		}
		for i, p := range edges {
			if mask&(1<<i) != 0 {
				cost := 96 + i*13
				_, _, err := s.Connect(p[0], p[1], uint16(cost))
				must(t, err)
				dist[p[0]][p[1]] = cost
				dist[p[1]][p[0]] = cost
			}
		}
		for k := 0; k < 4; k++ {
			for i := 0; i < 4; i++ {
				for j := 0; j < 4; j++ {
					dist[i][j] = min(dist[i][j], dist[i][k]+dist[k][j])
				}
			}
		}
		for i := range s.Nodes {
			must(t, s.Event(i, babel.Originate{Prefix: prefix(i)}))
		}
		must(t, s.Run(25*time.Second, noLoops))
		for i := range s.Nodes {
			for j := range s.Nodes {
				actual := -1
				for _, r := range s.FIB[i] {
					if r.Prefix == prefix(j) && !r.Unreachable {
						actual = int(r.Metric)
					}
				}
				expected := dist[i][j]
				if expected == 1<<20 {
					expected = -1
				}
				if actual != expected {
					t.Fatalf("graph=%d %d->%d metric=%d want=%d", mask, i, j, actual, expected)
				}
			}
		}
	}
}

func FuzzChurn(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9})
	f.Add([]byte{255, 200, 100, 50, 25, 12})
	f.Fuzz(func(t *testing.T, events []byte) {
		if len(events) > 64 {
			return
		}
		s, err := New(4)
		must(t, err)
		var links [4]Endpoint
		var live [4]bool
		for i := 0; i < 4; i++ {
			x, _, err := s.Connect(i, (i+1)%4, 96)
			must(t, err)
			links[i] = x
			live[i] = true
			must(t, s.Event(i, babel.Originate{Prefix: prefix(i)}))
		}
		must(t, s.Run(10*time.Second, noLoops))
		count := 0
		s.Fault = func(p Packet) bool {
			count++
			if count%11 == 0 {
				return true
			}
			if count%13 == 0 {
				p.At += 250 * time.Millisecond
				s.Queue = append(s.Queue, p)
				return true
			}
			if count%17 == 0 {
				s.Queue = append(s.Queue, p)
			}
			return false
		}
		for _, v := range events {
			i := int(v % 4)
			switch v / 4 % 4 {
			case 0:
				if live[i] {
					must(t, s.Disconnect(links[i]))
					live[i] = false
				} else {
					x, _, err := s.Connect(i, (i+1)%4, 96)
					must(t, err)
					links[i] = x
					live[i] = true
				}
			case 1:
				must(t, s.Event(i, babel.Withdraw{Prefix: prefix(i)}))
			case 2:
				must(t, s.Event(i, babel.Originate{Prefix: prefix(i)}))
			case 3:
				if live[i] {
					must(t, s.Event(i, babel.SetCost{ID: links[i].Link, Cost: uint16(v) + 1}))
				}
			}
			must(t, noLoops(s))
			must(t, s.Run(s.Now+300*time.Millisecond, noLoops))
		}
		s.Fault = nil
		must(t, s.Run(s.Now+20*time.Second, noLoops))
	})
}

func BenchmarkThousandNodeThreeGateways(b *testing.B) {
	for n := 0; n < b.N; n++ {
		s, err := New(1000)
		must(b, err)
		for i := 0; i < 1000; i++ {
			_, _, err := s.Connect(i, (i+1)%1000, 96)
			must(b, err)
		}
		for _, i := range []int{0, 333, 666} {
			must(b, s.Event(i, babel.Originate{Prefix: prefix(i)}))
		}
		must(b, s.Run(40*time.Second, nil))
		for n := range s.Nodes {
			for _, d := range []int{0, 333, 666} {
				got, err := s.Walk(n, prefix(d).Addr())
				if err != nil || got != d {
					b.Fatal(n, d, got, err)
				}
			}
		}
		b.ReportMetric(float64(s.Bytes)/1000/40, "control-B/node/s")
	}
}
