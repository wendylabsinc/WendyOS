package localmesh

import (
	"bytes"
	"container/heap"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

// This fixture executes the production directory, synchronizer, cryptographic
// verification and codec. Only time, links and the actor's announcement calls
// are simulated. It deliberately does not claim UDP/QUIC or radio wire counts.
type virtualNode struct {
	dir      *Directory
	pool     *netgossip.Pool
	creds    *Credentials
	revision uint64
}

type virtualEdge struct {
	from, to    int
	own         string
	sync        *Synchronizer
	queue       []ControlMessage
	ingress     []ControlMessage
	retry, busy bool
	next        time.Duration
	capacity    int
}

func (e *virtualEdge) credit() int { return e.capacity - len(e.queue) }
func (e *virtualEdge) enqueue(messages []ControlMessage) {
	if len(messages) > e.credit() {
		panic("simulated control queue overflow")
	}
	e.queue = append(e.queue, messages...)
}

type virtualEvent struct {
	at         time.Duration
	order      int
	edge       int
	kind       string
	wire       []byte
	probeStart time.Duration
	probeHop   int
}
type virtualEvents []virtualEvent

func (q virtualEvents) Len() int { return len(q) }
func (q virtualEvents) Less(i, j int) bool {
	if q[i].at == q[j].at {
		return q[i].order < q[j].order
	}
	return q[i].at < q[j].at
}
func (q virtualEvents) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *virtualEvents) Push(v any)   { *q = append(*q, v.(virtualEvent)) }
func (q *virtualEvents) Pop() any     { a := *q; v := a[len(a)-1]; *q = a[:len(a)-1]; return v }

type virtualStats struct {
	Scenario     string         `json:"scenario"`
	Frames       int            `json:"control_frames"`
	Bytes        int            `json:"control_bytes"`
	Kinds        map[string]int `json:"frames_by_kind"`
	ProbePackets int            `json:"probe_carrier_datagrams"`
	ProbeBytes   int            `json:"probe_carrier_bytes"`
	ProbeP50     float64        `json:"probe_p50_ms"`
	ProbeP95     float64        `json:"probe_p95_ms"`
	ProbeMax     float64        `json:"probe_max_ms"`
	Convergence  float64        `json:"convergence_ms"`
	latencies    []float64
}

type virtualMesh struct {
	services  int
	t         testing.TB
	epoch     time.Time
	at        time.Duration
	nodes     []*virtualNode
	edges     []*virtualEdge
	reverse   []int
	events    virtualEvents
	order     int
	bandwidth int
	delay     time.Duration
	path      []int
	stats     virtualStats
	published map[string]map[uint64]time.Duration
}

func newVirtualMesh(t testing.TB, topology string, count, bandwidth int, delay time.Duration) *virtualMesh {
	m := &virtualMesh{t: t, epoch: time.Now(), bandwidth: bandwidth, delay: delay, published: make(map[string]map[uint64]time.Duration)}
	for i := 0; i < count; i++ {
		c := testCredentialsFor(t, testPrincipal(testTenantUUID, fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)))
		cache, err := OpenIdentityCache("", DefaultCacheLimits(), c.Verify, m.epoch)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = cache.Put(c.Certificate.Certificate, m.epoch); err != nil {
			t.Fatal(err)
		}
		d, err := NewDirectory(c.Device.Tenant(), count*2, cache)
		if err != nil {
			t.Fatal(err)
		}
		m.nodes = append(m.nodes, &virtualNode{dir: d, pool: netgossip.NewPool(), creds: c})
	}
	connect := func(a, b int) {
		index := len(m.edges)
		for _, ends := range [][2]int{{a, b}, {b, a}} {
			n := m.nodes[ends[0]]
			m.edges = append(m.edges, &virtualEdge{from: ends[0], to: ends[1], own: n.creds.Device.Key(), sync: NewSynchronizer(n.dir, n.dir.cache, n.pool), capacity: 16})
		}
		m.reverse = append(m.reverse, index+1, index)
	}
	switch topology {
	case "line":
		for i := 1; i < count; i++ {
			connect(i-1, i)
		}
	case "diamond":
		for _, e := range [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}, {1, 2}} {
			connect(e[0], e[1])
		}
	case "full":
		for i := 0; i < count; i++ {
			for j := i + 1; j < count; j++ {
				connect(i, j)
			}
		}
	default:
		t.Fatal("unknown topology")
	}
	// BFS fixes one application-probe route. Routing itself is not simulated.
	paths := map[int][]int{0: {}}
	queue := []int{0}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		for i, e := range m.edges {
			if e.from != u {
				continue
			}
			if _, ok := paths[e.to]; ok {
				continue
			}
			paths[e.to] = append(append([]int{}, paths[u]...), i)
			queue = append(queue, e.to)
		}
	}
	m.path = paths[count-1]
	m.reset("cold")
	for i := range m.nodes {
		m.publish(i)
	}
	t.Cleanup(func() {
		for _, e := range m.edges {
			e.sync.Close()
		}
	})
	return m
}

func (m *virtualMesh) now() time.Time     { return m.epoch.Add(m.at) }
func (m *virtualMesh) add(e virtualEvent) { m.order++; e.order = m.order; heap.Push(&m.events, e) }
func (m *virtualMesh) reset(name string) {
	if len(m.events) != 0 {
		m.t.Fatal("phase ended with undelivered network events")
	}
	for _, e := range m.edges {
		if e.busy || len(e.queue) != 0 || len(e.ingress) != 0 {
			m.t.Fatal("phase ended with queued traffic")
		}
	}
	m.stats = virtualStats{Scenario: name, Kinds: map[string]int{}}
}
func (m *virtualMesh) relay(node, exclude int, changes []SignedManifest) {
	for _, w := range changes {
		manifest, err := DecodeManifest(w.Body)
		if err != nil {
			m.t.Fatal(err)
		}
		start, ok := m.published[w.Principal][manifest.Revision]
		if !ok {
			m.t.Fatal("received a revision that was never published")
		}
		m.stats.Convergence = max(m.stats.Convergence, float64(m.at-start)/float64(time.Millisecond))
	}
	for _, e := range m.edges {
		if e.from == node && e.to != exclude {
			for _, w := range changes {
				virtualChange(e, w, m.now())
			}
		}
	}
}
func (m *virtualMesh) publish(i int) {
	n := m.nodes[i]
	n.revision++
	if m.published[n.creds.Device.Principal()] == nil {
		m.published[n.creds.Device.Principal()] = make(map[uint64]time.Duration)
	}
	m.published[n.creds.Device.Principal()][n.revision] = m.at
	manifest := routeManifest(n.creds.Device, m.now())
	manifest.Services = encodingBenchServices(m.services)
	manifest.Revision = n.revision
	manifest.Expires = m.now().Add(15 * time.Minute).UnixMilli()
	w, err := SignManifest(manifest, n.creds.Certificate.Certificate, n.creds.Signer, m.now())
	if err != nil {
		m.t.Fatal(err)
	}
	if changed, err := n.dir.Accept(w, m.now()); err != nil || !changed {
		m.t.Fatalf("publish: %v %v", changed, err)
	}
	m.relay(i, -1, []SignedManifest{w})
}

func (m *virtualMesh) transmit(edge, size int) time.Duration {
	e := m.edges[edge]
	end := max(m.at, e.next) + time.Duration(size)*time.Second/time.Duration(m.bandwidth)
	e.next = end
	return end
}

func (m *virtualMesh) pump() {
	for i, e := range m.edges {
		for len(e.ingress) > 0 {
			replies, changed, err := e.sync.Receive(e.ingress[0], m.now(), e.credit())
			if errors.Is(err, ErrControlBackpressure) {
				break
			}
			if err != nil {
				m.t.Fatalf("receive %d->%d: %v", e.to, e.from, err)
			}
			e.ingress = e.ingress[1:]
			e.enqueue(replies)
			m.relay(e.from, e.to, changed)
		}
		if e.busy {
			continue
		}
		for len(e.queue) > 0 {
			message := e.queue[0]
			e.queue = e.queue[1:]
			if message.Expired(m.now()) {
				continue
			}
			var wire bytes.Buffer
			if err := WriteControl(&wire, message); err != nil {
				m.t.Fatal(err)
			}
			key := message.Kind
			if message.Gossip != nil {
				key += "/" + message.Gossip.Op
			}
			m.stats.Frames++
			m.stats.Bytes += wire.Len()
			m.stats.Kinds[key]++
			end := m.transmit(i, wire.Len())
			e.busy = true
			m.add(virtualEvent{at: end, edge: i, kind: "sent"})
			m.add(virtualEvent{at: end + m.delay, edge: i, kind: "receive", wire: wire.Bytes()})
			break
		}
	}
}

func (m *virtualMesh) tick() {
	for _, n := range m.nodes {
		n.dir.Sweep(m.now())
		n.pool.Sweep(m.now())
	}
	for _, e := range m.edges {
		e.sync.Sweep(m.now())
		virtualBeforeMaintenance(e, m.now())
		replies, changed, err := e.sync.ContinueCertificates(m.now(), e.credit())
		if err != nil {
			m.t.Fatal(err)
		}
		e.enqueue(replies)
		m.relay(e.from, e.to, changed)
		virtualMaintain(e, m.now(), m.at > 0 && m.at%(5*time.Minute) == 0)
	}
}

func (m *virtualMesh) probe(start time.Duration, hop int) {
	if hop == len(m.path) {
		m.stats.latencies = append(m.stats.latencies, float64(m.at-start)/float64(time.Millisecond))
		return
	}
	// A 160-byte IPv6 packet plus the actual carrier fragment header. The
	// scheduler models shared bandwidth, not QUIC's stream/datagram scheduler.
	size := 160 + fragmentHeader
	m.stats.ProbePackets++
	m.stats.ProbeBytes += size
	end := m.transmit(m.path[hop], size)
	m.add(virtualEvent{at: end + m.delay, kind: "probe", probeStart: start, probeHop: hop + 1})
}

func (m *virtualMesh) advance(duration time.Duration, renew, probes bool) {
	end := m.at + duration
	for tick := (m.at/time.Second + 1) * time.Second; tick <= end; tick += time.Second {
		m.add(virtualEvent{at: tick, kind: "tick"})
	}
	if probes {
		for at := m.at; at < end-time.Second; at += 100 * time.Millisecond {
			m.add(virtualEvent{at: at, kind: "probe-start"})
		}
	}
	m.pump()
	for len(m.events) > 0 && m.events[0].at <= end {
		e := heap.Pop(&m.events).(virtualEvent)
		m.at = e.at
		switch e.kind {
		case "sent":
			m.edges[e.edge].busy = false
		case "receive":
			message, err := ReadControl(bytes.NewReader(e.wire))
			if err != nil {
				m.t.Fatal(err)
			}
			receiver := m.edges[m.reverse[e.edge]]
			receiver.ingress = append(receiver.ingress, message)
		case "tick":
			if renew && m.at%(5*time.Minute) == 0 {
				for i := range m.nodes {
					m.publish(i)
				}
			}
			m.tick()
		case "probe-start":
			m.probe(m.at, 0)
		case "probe":
			m.probe(e.probeStart, e.probeHop)
		}
		m.pump()
	}
	m.at = end
}

func (m *virtualMesh) assertConverged() {
	for i, n := range m.nodes {
		records := n.dir.Records(m.now())
		if len(records) != len(m.nodes) {
			m.t.Fatalf("node %d: %d/%d origins", i, len(records), len(m.nodes))
		}
		for _, w := range records {
			manifest, err := DecodeManifest(w.Body)
			if err != nil {
				m.t.Fatal(err)
			}
			for _, origin := range m.nodes {
				if origin.creds.Device.Principal() == w.Principal && manifest.Revision != origin.revision {
					m.t.Fatalf("stale node %d revision %d want %d", i, manifest.Revision, origin.revision)
				}
			}
		}
	}
}

func (m *virtualMesh) report() {
	m.assertConverged()
	if len(m.stats.latencies) > 0 {
		sort.Float64s(m.stats.latencies)
		n := len(m.stats.latencies)
		m.stats.ProbeP50 = m.stats.latencies[(n-1)/2]
		m.stats.ProbeP95 = m.stats.latencies[(n-1)*95/100]
		m.stats.ProbeMax = m.stats.latencies[n-1]
	}
	data, err := json.Marshal(m.stats)
	if err != nil {
		m.t.Fatal(err)
	}
	m.t.Log("MESH_RESULT " + string(data))
}

func TestVirtualMeshRegression(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
	}{{"diamond", 4}, {"line", 8}, {"full", 8}} {
		for _, slow := range []bool{false, true} {
			name := fmt.Sprintf("%s-%d/slow=%t", tc.name, tc.n, slow)
			t.Run(name, func(t *testing.T) {
				rate, delay := 1000000, time.Millisecond
				if slow {
					rate, delay = 8000, 20*time.Millisecond
				}
				m := newVirtualMesh(t, tc.name, tc.n, rate, delay)
				m.advance(60*time.Second, false, true)
				m.report()
				m.reset("steady-with-renewals")
				m.advance(11*time.Minute, true, true)
				m.report()
				m.reset("publication-burst")
				// More origins/revisions than a carrier's 16 queued frames.
				for range 32 {
					for i := range m.nodes {
						m.publish(i)
					}
				}
				m.advance(60*time.Second, false, true)
				m.report()
				m.reset("warm-reconnect")
				for _, e := range m.edges {
					e.sync.Close()
					n := m.nodes[e.from]
					e.sync = NewSynchronizer(n.dir, n.dir.cache, n.pool)
					virtualSeed(e, m.now())
				}
				m.advance(60*time.Second, false, true)
				m.report()
				if m.stats.Kinds["gossip/data"] != 0 {
					t.Fatal("warm reconnect redownloaded known manifests")
				}
			})
		}
	}
}

// Probe arrivals inside the inventory transfer expose serialization delays
// that the normal 100 ms sampling cadence can miss.
func TestVirtualMeshImmediateWarmProbe(t *testing.T) {
	for _, offset := range []time.Duration{0, 10 * time.Millisecond, 20 * time.Millisecond, 31 * time.Millisecond, 40 * time.Millisecond} {
		t.Run(offset.String(), func(t *testing.T) {
			m := newVirtualMesh(t, "full", 8, 8000, 20*time.Millisecond)
			m.advance(60*time.Second, false, false)
			m.reset("warm-reconnect")
			for _, e := range m.edges {
				e.sync.Close()
				n := m.nodes[e.from]
				e.sync = NewSynchronizer(n.dir, n.dir.cache, n.pool)
				virtualSeed(e, m.now())
			}
			m.add(virtualEvent{at: m.at + offset, kind: "probe-start"})
			m.advance(time.Second, false, false)
			m.report()
		})
	}
}

func virtualCPU(t testing.TB) int64 {
	var r syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &r); err != nil {
		t.Fatal(err)
	}
	return r.Utime.Nano() + r.Stime.Nano()
}

func BenchmarkVirtualMeshPublication(b *testing.B) {
	m := newVirtualMesh(b, "diamond", 4, 1000000, time.Millisecond)
	m.advance(30*time.Second, false, false)
	m.assertConverged()
	m.reset("benchmark")
	b.ReportAllocs()
	b.ResetTimer()
	cpu := virtualCPU(b)
	for i := 0; i < b.N; i++ {
		// Keep lease and certificate times fixed across arbitrary b.N. Each
		// revision gets a fresh one-second event window at the same epoch.
		m.at = 30 * time.Second
		for _, e := range m.edges {
			e.next = m.at
		}
		m.publish(0)
		m.advance(time.Second, false, false)
	}
	cpu = virtualCPU(b) - cpu
	b.StopTimer()
	m.assertConverged()
	b.ReportMetric(float64(cpu)/float64(b.N), "cpu-ns/op")
	b.ReportMetric(float64(cpu)/float64(m.stats.Bytes), "cpu-ns/ctrl-B")
	b.ReportMetric(float64(m.stats.Frames)/float64(b.N), "ctrl-frames/op")
	b.ReportMetric(float64(m.stats.Bytes)/float64(b.N), "ctrl-B/op")
}

// Isolate inventory production from simulation bookkeeping and receivers.
// One operation advertises the entire directory through a 16-frame queue.
func BenchmarkVirtualInventory(b *testing.B) {
	for _, count := range []int{4, 32, 128} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			now := time.Now()
			first := testCredentialsFor(b, testPrincipal(testTenantUUID, "00000000-0000-4000-8000-000000000001"))
			cache, err := OpenIdentityCache("", DefaultCacheLimits(), first.Verify, now)
			if err != nil {
				b.Fatal(err)
			}
			d, err := NewDirectory(first.Device.Tenant(), count, cache)
			if err != nil {
				b.Fatal(err)
			}
			for i := 0; i < count; i++ {
				c := testCredentialsFor(b, testPrincipal(testTenantUUID, fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)))
				if _, err := cache.Put(c.Certificate.Certificate, now); err != nil {
					b.Fatal(err)
				}
				w, err := SignManifest(routeManifest(c.Device, now), c.Certificate.Certificate, c.Signer, now)
				if err != nil {
					b.Fatal(err)
				}
				if ok, err := d.Accept(w, now); err != nil || !ok {
					b.Fatalf("accept: %t %v", ok, err)
				}
			}
			e := &virtualEdge{own: first.Device.Key(), sync: NewSynchronizer(d, cache, netgossip.NewPool()), capacity: 16}
			defer e.sync.Close()
			// Establish retained artifacts before timing warm inventory sweeps.
			drain := func() {
				virtualSeed(e, now)
				frames := 0
				origins := 0
				for len(e.queue) != 0 {
					for _, message := range e.queue {
						if err := WriteControl(io.Discard, message); err != nil {
							b.Fatal(err)
						}
						frames++
						origins += virtualInventoryCount(message)
					}
					e.queue = e.queue[:0]
					virtualMaintain(e, now, false)
				}
				if origins != count {
					b.Fatalf("inventory emitted %d origins in %d frames, want %d origins", origins, frames, count)
				}
			}
			drain()
			b.ReportAllocs()
			b.ResetTimer()
			cpu := virtualCPU(b)
			for i := 0; i < b.N; i++ {
				drain()
			}
			cpu = virtualCPU(b) - cpu
			b.StopTimer()
			b.ReportMetric(float64(cpu)/float64(b.N), "cpu-ns/op")
			b.ReportMetric(float64(cpu)/float64(b.N*count), "cpu-ns/origin")
		})
	}
}

func BenchmarkVirtualCarrierCost(b *testing.B) {
	for _, tc := range []struct {
		name     string
		size     int
		capacity int64
	}{{"small", 160, 1356}, {"whole", TunnelMTU, 1356}, {"fragmented", TunnelMTU, 1156}} {
		b.Run(tc.name, func(b *testing.B) {
			packet := make([]byte, tc.size)
			packet[0] = 0x60
			binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)-40))
			sender := &carrierIPSender{sender: &benchmarkCarrierDatagrams{capacity: tc.capacity}, negotiated: WholeIPDatagramLimit}
			b.SetBytes(int64(tc.size))
			b.ReportAllocs()
			b.ResetTimer()
			cpu := virtualCPU(b)
			for i := 0; i < b.N; i++ {
				if err := sender.sendIP(uint32(i), packet); err != nil {
					b.Fatal(err)
				}
			}
			cpu = virtualCPU(b) - cpu
			b.StopTimer()
			s := sender.snapshot()
			b.ReportMetric(float64(cpu)/float64(b.N), "cpu-ns/op")
			b.ReportMetric(float64(cpu)/float64(b.N*tc.size), "cpu-ns/B")
			b.ReportMetric(float64(s.Fragments)/float64(b.N), "datagrams/op")
			b.ReportMetric(float64(s.Bytes)/float64(b.N), "carrier-B/op")
		})
	}
}

func encodingBenchServices(count int) []ManifestService {
	var services []ManifestService
	for i := 0; i < count; i++ {
		services = append(services, ManifestService{
			Mesh: "lab", AppID: "dev.wendy.camera", ServiceID: fmt.Sprintf("camera-%03d", i),
			Type: "_http._tcp", Instance: fmt.Sprintf("Camera %d", i), Port: uint16(8000 + i),
			TXT: []string{"codec=h264", "description=" + strings.Repeat("s", 96)},
		})
	}
	return services
}

func BenchmarkVirtualEncodingPublication(b *testing.B) {
	for _, services := range []int{0, 32} {
		b.Run(fmt.Sprintf("services=%d", services), func(b *testing.B) {
			m := newVirtualMesh(b, "diamond", 4, 1000000, time.Millisecond)
			m.services = services
			for i := range m.nodes {
				m.publish(i)
			}
			m.advance(30*time.Second, false, false)
			m.assertConverged()
			m.reset("benchmark")
			b.ReportAllocs()
			b.ResetTimer()
			cpu := virtualCPU(b)
			for i := 0; i < b.N; i++ {
				m.at = 30 * time.Second
				for _, e := range m.edges {
					e.next = m.at
				}
				m.publish(0)
				m.advance(5*time.Second, false, false)
				if len(m.events) != 0 {
					b.Fatal("transfer did not drain in its measurement window")
				}
			}
			cpu = virtualCPU(b) - cpu
			b.StopTimer()
			m.assertConverged()
			manifestBytes := 0
			for _, w := range m.nodes[0].dir.Records(m.now()) {
				if w.Principal == m.nodes[0].creds.Device.Principal() {
					manifestBytes = len(w.Body)
					break
				}
			}
			if manifestBytes == 0 {
				b.Fatal("missing benchmark origin")
			}
			b.ReportMetric(float64(manifestBytes), "manifest-B")
			b.ReportMetric(float64(cpu)/float64(b.N), "cpu-ns/op")
			b.ReportMetric(float64(cpu)/float64(m.stats.Bytes), "cpu-ns/ctrl-B")
			b.ReportMetric(float64(m.stats.Frames)/float64(b.N), "ctrl-frames/op")
			b.ReportMetric(float64(m.stats.Bytes)/float64(b.N), "ctrl-B/op")
		})
	}
}
func BenchmarkVirtualEncodingInventory(b *testing.B) {
	for _, tc := range []struct{ origins, services int }{{4, 0}, {32, 0}, {128, 0}, {4, 32}, {32, 32}, {128, 32}} {
		count := tc.origins
		b.Run(fmt.Sprintf("origins=%d/services=%d", count, tc.services), func(b *testing.B) {
			now := time.Now()
			first := testCredentialsFor(b, testPrincipal(testTenantUUID, "00000000-0000-4000-8000-000000000001"))
			cache, err := OpenIdentityCache("", DefaultCacheLimits(), first.Verify, now)
			if err != nil {
				b.Fatal(err)
			}
			d, err := NewDirectory(first.Device.Tenant(), count, cache)
			if err != nil {
				b.Fatal(err)
			}
			for i := 0; i < count; i++ {
				c := testCredentialsFor(b, testPrincipal(testTenantUUID, fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)))
				if _, err := cache.Put(c.Certificate.Certificate, now); err != nil {
					b.Fatal(err)
				}
				manifest := routeManifest(c.Device, now)
				manifest.Services = encodingBenchServices(tc.services)
				w, err := SignManifest(manifest, c.Certificate.Certificate, c.Signer, now)
				if err != nil {
					b.Fatal(err)
				}
				if ok, err := d.Accept(w, now); err != nil || !ok {
					b.Fatalf("accept: %t %v", ok, err)
				}
			}
			e := &virtualEdge{own: first.Device.Key(), sync: NewSynchronizer(d, cache, netgossip.NewPool()), capacity: 16}
			defer e.sync.Close()
			// Establish retained artifacts before timing warm inventory sweeps.
			drain := func() {
				virtualSeed(e, now)
				frames := 0
				origins := 0
				for len(e.queue) != 0 {
					for _, message := range e.queue {
						if err := WriteControl(io.Discard, message); err != nil {
							b.Fatal(err)
						}
						frames++
						origins += virtualInventoryCount(message)
					}
					e.queue = e.queue[:0]
					virtualMaintain(e, now, false)
				}
				if origins != count {
					b.Fatalf("inventory emitted %d origins in %d frames, want %d origins", origins, frames, count)
				}
			}
			drain()
			b.ReportAllocs()
			b.ResetTimer()
			cpu := virtualCPU(b)
			for i := 0; i < b.N; i++ {
				drain()
			}
			cpu = virtualCPU(b) - cpu
			b.StopTimer()
			b.ReportMetric(float64(cpu)/float64(b.N), "cpu-ns/op")
			b.ReportMetric(float64(cpu)/float64(b.N*count), "cpu-ns/origin")
		})
	}
}
