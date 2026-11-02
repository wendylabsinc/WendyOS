package localmesh

import (
	"context"
	"errors"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/asyncdiag"
	"io"
	"log"
	"sync"
	"testing"
	"time"
)

type reviewRouteWriter struct {
	entered, release chan struct{}
	once             sync.Once
}

func (w *reviewRouteWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

type reviewNodeRoutingIO struct {
	node       *Node
	persistErr error
	sent       chan struct{}
}

func (r *reviewNodeRoutingIO) Apply(routes []babel.Route) error { return r.node.Apply(routes) }
func (r *reviewNodeRoutingIO) Persist(babel.Checkpoint) error   { return r.persistErr }
func (r *reviewNodeRoutingIO) Resume() error                    { return r.node.Resume() }
func (r *reviewNodeRoutingIO) Stop() error                      { return r.node.Stop() }
func (r *reviewNodeRoutingIO) Send(babel.Datagram) error {
	select {
	case r.sent <- struct{}{}:
	default:
	}
	return nil
}
func TestBlockedRouteDiagnosticDoesNotDelayCommittedDatagramsOrNextTick(t *testing.T) {
	net := newSignedRouteTestNetwork(t, 3)
	net.connect(0, 1, 1)
	net.connect(1, 2, 2)
	proof := net.sign(1, time.Minute, false)
	net.accept(1, proof)
	net.accept(2, proof)
	net.step(0, babel.Originate{Prefix: net.nodes[0].device.prefix})
	net.advance(3 * time.Second)
	net.assertForwarded(1, true, 0)
	routing := net.nodes[1].routing
	node := &Node{kernel: &lifecyclePacketNetwork{}, forwardRoutes: append([]babel.Route(nil), net.nodes[1].io.routes...), routeOrigins: net.nodes[1].origins, routing: routing, routeInput: routeInput{Kind: "peer-removed", Link: 1}}
	adapter := &reviewNodeRoutingIO{node: node, sent: make(chan struct{}, 100)}
	routing.io = adapter
	writer := &reviewRouteWriter{entered: make(chan struct{}), release: make(chan struct{})}
	old := log.Writer()
	log.SetOutput(writer)
	defer func() {
		close(writer.release)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := asyncdiag.Flush(ctx); err != nil {
			t.Error(err)
		}
		log.SetOutput(old)
	}()
	done := make(chan error, 1)
	go func() { done <- routing.Step(net.elapsed, babel.RemoveLink{ID: 1}) }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("route logger not reached")
	}
	if !node.gate.TryLock() {
		t.Fatal("forwarding barrier remained locked during log")
	}
	node.gate.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("route step blocked on sink")
	}
	next := make(chan error, 1)
	go func() { next <- routing.Step(net.elapsed+time.Second, babel.Tick{}) }()
	select {
	case err := <-next:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("next tick blocked on sink")
	}
	select {
	case <-adapter.sent:
	case <-time.After(time.Second):
		t.Fatal("committed datagram not released after trigger gate")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- node.Stop() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown blocked on sink")
	}

}
func TestRoutePersistFailureDiscardsStagedAsyncDiagnostics(t *testing.T) {
	net := newSignedRouteTestNetwork(t, 2)
	net.connect(0, 1, 1)
	net.accept(1, net.sign(1, time.Minute, false))
	net.step(0, babel.Originate{Prefix: net.nodes[0].device.prefix})
	net.advance(3 * time.Second)
	net.assertForwarded(1, true, 0)
	routing := net.nodes[1].routing
	backend := &lifecyclePacketNetwork{}
	node := &Node{kernel: backend, forwardRoutes: append([]babel.Route(nil), net.nodes[1].io.routes...), routeOrigins: net.nodes[1].origins, routing: routing}
	routing.io = &reviewNodeRoutingIO{node: node, persistErr: errors.New("injected durable-write failure"), sent: make(chan struct{}, 1)}
	old := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(old)
	if err := routing.Step(net.elapsed, babel.RemoveLink{ID: 1}); err == nil {
		t.Fatal("persist error ignored")
	}
	if !node.stopped || backend.stopped.Load() != 1 || len(node.routeDiagnostics) != 0 {
		t.Fatalf("failure not cleared: stopped=%v events=%v", node.stopped, node.routeDiagnostics)
	}
	if !node.gate.TryLock() {
		t.Fatal("failed transaction retained gate")
	}
	node.gate.Unlock()
	if err := routing.Step(net.elapsed+time.Second, babel.Tick{}); !errors.Is(err, babel.ErrStopped) {
		t.Fatalf("failed routing reused: %v", err)
	}
}
