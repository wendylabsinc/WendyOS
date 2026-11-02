package localmesh

import (
	"bytes"
	"errors"
	"log"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

func TestSignedRouteTransitionDistinguishesProofAdvanceAndPathLoss(t *testing.T) {
	for _, cause := range []string{"new proof", "same sequence renewed", "peer removed", "proof expired"} {
		t.Run(cause, func(t *testing.T) {
			n := newSignedRouteTestNetwork(t, 2, func(c *babel.Config) { c.UpdateInterval = 16 * time.Second })
			n.connect(0, 1, 1)
			proof := n.sign(1, time.Minute, false)
			n.accept(1, proof)
			n.step(0, babel.Originate{Prefix: n.nodes[0].device.prefix})
			n.advance(2 * time.Second)
			n.assertForwarded(1, true, 0)
			before := append([]babel.Route(nil), n.nodes[1].io.routes...)
			input := routeInput{Kind: "tick"}
			want := "origin-superseded"
			switch cause {
			case "new proof":
				origin := n.nodes[0]
				n.step(0, babel.Receive{ID: 1, Packet: routeSequenceRequest(origin.device.prefix, origin.device.routerID, 1)})
				n.accept(1, n.sign(2, time.Minute, false))
				// Do not deliver the independent Babel packet yet. This is the real
				// signature-verified proof-before-update gap seen in the lab.
				n.step(1, babel.Tick{})
			case "same sequence renewed":
				n.accept(1, n.sign(2, time.Minute, false))
				n.step(1, babel.Tick{})
				n.assertForwarded(1, true, 0)
				var observations []routeTransition
				reportRouteTransitions(before, n.nodes[1].io.routes, n.nodes[1].origins, input, n.nodes[1].routing.Snapshot(), n.now(), func(e routeTransition) { observations = append(observations, e) })
				if len(observations) != 0 {
					t.Fatalf("same-sequence renewal changed route: %+v", observations)
				}
				return
			case "peer removed":
				n.step(1, babel.RemoveLink{ID: 1})
				input = routeInput{Kind: "peer-removed", Link: 1}
				want = "babel-route-unavailable"
			case "proof expired":
				n.elapsed += time.Minute
				n.step(1, babel.Tick{})
				want = "origin-not-authorized"
			}
			n.assertForwarded(1, false, 0)
			var events []routeTransition
			reportRouteTransitions(before, n.nodes[1].io.routes, n.nodes[1].origins, input, n.nodes[1].routing.Snapshot(), n.now(), func(e routeTransition) { events = append(events, e) })
			if len(events) != 1 || events[0].Reason != want || events[0].Previous.RouterID != n.nodes[0].device.routerID || events[0].Previous.Seqno != 0 {
				t.Fatalf("transition=%+v want=%s", events, want)
			}
			event := events[0]
			if cause == "new proof" {
				if event.PreviousOriginAuthorized || len(event.CurrentOrigins) != 1 || event.CurrentOrigins[0].Seqno != 1 {
					t.Fatalf("proof transition=%+v", event)
				}
				before = append([]babel.Route(nil), n.nodes[1].io.routes...)
				n.deliver()
				n.advance(20 * time.Second)
				n.assertForwarded(1, true, 1)
				events = nil
				reportRouteTransitions(before, n.nodes[1].io.routes, n.nodes[1].origins, routeInput{Kind: "packet", Link: 1}, n.nodes[1].routing.Snapshot(), n.now(), func(e routeTransition) { events = append(events, e) })
				if len(events) != 1 || events[0].Reason != "route-recovered" || events[0].Current.Seqno != 1 || len(events[0].CurrentOrigins) != 1 || events[0].CurrentOrigins[0].Seqno != 1 {
					t.Fatalf("recovery=%+v", events)
				}
			}
			if cause == "peer removed" && !event.PreviousOriginAuthorized {
				t.Fatal("path loss mislabeled as lost signed authority")
			}
		})
	}
}

type diagnosticPacketNetwork struct {
	lifecyclePacketNetwork
	applyError error
}

func (n *diagnosticPacketNetwork) Apply([]babel.Route) error { return n.applyError }

func TestNodeReportsRouteTransitionOnlyAfterSuccessfulApply(t *testing.T) {
	prefix := netip.MustParsePrefix("fd45:b891:567f:1234::1/128")
	old := babel.Route{Prefix: prefix, RouterID: 3, Seqno: 7, Metric: 4096, Link: 12}
	drop := babel.Route{Prefix: prefix, Metric: babel.Infinity, Unreachable: true}
	next := RouteOrigin{Prefix: prefix, RouterID: 3, Seqno: 8}
	for _, phase := range []string{"resume", "failed-apply", "stop-before-resume"} {
		var out bytes.Buffer
		loggerOutput := log.Writer()
		log.SetOutput(&out)
		backend := &diagnosticPacketNetwork{}
		if phase == "failed-apply" {
			backend.applyError = errors.New("simulated forwarding failure")
		}
		n := &Node{kernel: backend, forwardRoutes: []babel.Route{old}, routeOrigins: map[RouteOrigin]bool{next: true}}
		err := n.Apply([]babel.Route{drop})
		if out.Len() != 0 {
			t.Fatal("diagnostic logged while forwarding was paused")
		}
		if phase != "resume" {
			_ = n.Stop()
		} else {
			_ = n.Resume()
		}
		log.SetOutput(loggerOutput)
		if phase != "resume" {
			if phase == "failed-apply" && err == nil {
				t.Fatal("expected apply failure")
			}
			if strings.Contains(out.String(), "wendy net route transition=") {
				t.Fatalf("failed apply claimed transition: %v %s", err, out.String())
			}
		} else if err != nil || !strings.Contains(out.String(), `"reason":"origin-superseded"`) || !strings.Contains(out.String(), `"Seqno":7`) || !strings.Contains(out.String(), `"seqno":8`) {
			t.Fatalf("missing actual apply diagnostic: %v %s", err, out.String())
		}
	}
}

func TestRouteDiagnosticsIgnoreInitialAndOrdinaryMetricChanges(t *testing.T) {
	prefix := netip.MustParsePrefix("fd45:b891:567f:1234::1/128")
	route := babel.Route{Prefix: prefix, RouterID: 2, Seqno: 1, Metric: 4096, Link: 9}
	changed := route
	changed.Metric = 4224
	changed.Seqno = 2
	count := 0
	emit := func(routeTransition) { count++ }
	reportRouteTransitions(nil, []babel.Route{route}, nil, routeInput{}, babel.Snapshot{}, time.Now(), emit)
	reportRouteTransitions([]babel.Route{route}, []babel.Route{changed}, nil, routeInput{}, babel.Snapshot{}, time.Now(), emit)
	if count != 0 {
		t.Fatalf("ordinary routes generated %d failure diagnostics", count)
	}
}
