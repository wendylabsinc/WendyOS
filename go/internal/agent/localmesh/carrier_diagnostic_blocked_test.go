package localmesh

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/asyncdiag"
)

type blockedCarrierWriter struct {
	entered, release chan struct{}
	once             sync.Once
	mu               sync.Mutex
	buffer           bytes.Buffer
}

func (w *blockedCarrierWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.Write(data)
}

func TestCarrierDiagnosticsFlagCannotBlockNodeWithdrawalOrTimer(t *testing.T) {
	t.Setenv("WENDY_NET_CARRIER_DIAGNOSTICS", "1")
	f := newCarrierStandbyFixture(t)
	n := f.nodes[0]
	if !n.diagnostics.enabled {
		t.Fatal("real Node startup did not bind the diagnostic flag")
	}
	// Add a real Node QUIC carrier using a bounded in-memory datagram adapter.
	// The existing BLE remains the actually installed route until removal.
	stats := &admissionFixture{capacity: 1356}
	peer := &nodePeer{device: f.nodes[1].Credentials.Device, cost: 128, transport: "infrastructure-wifi", link: &quicNodeLink{sendIP: &carrierIPSender{sender: stats, negotiated: WholeIPDatagramLimit}}, control: make(chan ControlMessage, 4096), done: make(chan struct{}), retire: make(chan struct{})}
	if err := n.handle(nodeEvent{add: peer}); err != nil {
		t.Fatal(err)
	}
	flushCarrierDiagnostics(t)
	writer := &blockedCarrierWriter{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(writer.release) }) }
	oldWriter, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(writer)
	log.SetFlags(0)
	t.Cleanup(func() {
		release()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := asyncdiag.Flush(ctx); err != nil {
			t.Error(err)
		}
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})
	// Make the existing diagnostic cadence due without changing routing time.
	n.diagnostics.next, n.diagnostics.warmNext = time.Time{}, time.Time{}
	run := func(action func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- action() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			release()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("actor did not finish after releasing the injected sink")
			}
			t.Fatal("diagnostic sink blocked the real Node actor")
		}
	}
	run(func() error { n.refresh(); return nil })
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("injected sink was never reached")
	}
	run(func() error { return n.handle(nodeEvent{id: 1, remove: true}) })
	prefix := netip.PrefixFrom(f.nodes[1].Credentials.Device.MeshAddress(), 128)
	for _, route := range n.Snapshot().ForwardingRoutes {
		if route.Prefix == prefix && !route.Unreachable && !route.Local && route.Metric != babel.Infinity {
			t.Fatal("withdrawal did not remove the actually installed BLE route")
		}
	}
	f.elapsed += time.Second
	run(func() error { return n.step(babel.Tick{}) })
	if n.routing.Snapshot().Now != f.elapsed {
		t.Fatal("next Babel timer step did not advance while logging was blocked")
	}
	if !n.gate.TryLock() {
		t.Fatal("diagnostic retained the forwarding gate")
	}
	n.gate.Unlock()
	release()
	flushCarrierDiagnostics(t)
	writer.mu.Lock()
	output := writer.buffer.String()
	writer.mu.Unlock()
	seenStats, seenWarm := false, false
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		for _, item := range []struct {
			prefix string
			seen   *bool
		}{{"wendy net carrier stats=", &seenStats}, {"wendy net carrier readiness=", &seenWarm}} {
			if strings.HasPrefix(line, item.prefix) {
				*item.seen = true
				if !json.Valid([]byte(strings.TrimPrefix(line, item.prefix))) {
					t.Fatal("queued carrier sample was not intact JSON")
				}
			}
		}
	}
	if !seenStats || !seenWarm {
		t.Fatalf("both enabled diagnostic paths were not exercised: stats=%v warm=%v", seenStats, seenWarm)
	}
}
