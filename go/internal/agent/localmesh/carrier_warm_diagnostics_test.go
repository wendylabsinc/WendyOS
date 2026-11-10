package localmesh

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

func TestWarmCarrierDiagnosticsProveExactAuthorizedStandby(t *testing.T) {
	f := newCarrierStandbyFixture(t)
	f.addCarrier(128, "infrastructure-wifi")
	f.advance(7*time.Second, 0)
	f.assertSelected(2)
	n := f.nodes[0]
	view, routing := n.Snapshot(), n.routing.Snapshot()
	now := time.Now()
	for _, mode := range []string{"warm", "wrong-incarnation", "expired-proof", "wrong-origin", "not-two-way", "retiring"} {
		t.Run(mode, func(t *testing.T) {
			links := append([]PeerLink(nil), view.Links...)
			state := routing
			state.Neighbours = append([]babel.Neighbour(nil), routing.Neighbours...)
			proofs := make(map[RouteOrigin]RouteProof, len(view.RouteProofs))
			for origin, proof := range view.RouteProofs {
				if mode == "expired-proof" {
					proof.Deadline = now.Add(-time.Nanosecond)
				}
				if mode == "wrong-origin" {
					origin.RouterID++
				}
				proofs[origin] = proof
			}
			for i := range links {
				if links[i].ID == 1 {
					switch mode {
					case "wrong-incarnation":
						links[i].ID = 99
					case "retiring":
						links[i].Retiring = true
					}
				}
			}
			for i := range state.Neighbours {
				if state.Neighbours[i].Link == 1 {
					if mode == "wrong-incarnation" {
						state.Neighbours[i].Link = 99 // Two-way alone cannot prove a candidate on a new incarnation.
					}
					if mode == "not-two-way" {
						state.Neighbours[i].Reachable = false
					}
				}
			}
			var rows []warmCarrierDiagnostic
			emit := func(data []byte) {
				var row warmCarrierDiagnostic
				if err := json.Unmarshal(data, &row); err != nil {
					t.Fatal(err)
				}
				rows = append(rows, row)
			}
			d := carrierDiagnostics{}
			d.emitWarm(now, n.Credentials.Device, links, state, proofs, n.routing.HasUsableOriginOnLink, emit)
			if len(rows) != 0 {
				t.Fatal("warm diagnostics enabled by default")
			}
			d.enabled = true
			d.emitWarm(now, n.Credentials.Device, links, state, proofs, n.routing.HasUsableOriginOnLink, emit)
			d.emitWarm(now.Add(carrierDiagnosticInterval-time.Nanosecond), n.Credentials.Device, links, state, proofs, n.routing.HasUsableOriginOnLink, emit)
			if len(rows) != 2 {
				t.Fatalf("diagnostic rows=%d, want two bounded link rows", len(rows))
			}
			for _, row := range rows {
				if row.Peer != f.nodes[1].Credentials.Device.Principal() || row.Self != n.Credentials.Device.Principal() || !row.Time.Equal(now) {
					t.Fatalf("wrong diagnostic binding: %+v", row)
				}
				if row.Link != 2 {
					if row.CandidateUsable != (mode == "warm") || row.SelectedInstalled {
						t.Fatalf("standby observation falsely inferred readiness: %+v", row)
					}
				} else if mode == "warm" && (!row.CandidateUsable || !row.SelectedInstalled) {
					t.Fatalf("installed LAN route not distinguished: %+v", row)
				}
				if row.CandidateUsable && (row.Origin == nil || !row.ProofDeadline.After(now)) {
					t.Fatalf("usable route omitted current proof: %+v", row)
				}
			}
			d.emitWarm(now.Add(carrierDiagnosticInterval), n.Credentials.Device, links, state, proofs, n.routing.HasUsableOriginOnLink, emit)
			if len(rows) != 4 {
				t.Fatal("warm diagnostics did not resume at their existing cadence")
			}
		})
	}
	// Even malformed local caller input cannot turn this into a global route
	// dump. The production Node's admission ceiling is the same 64 links.
	links := make([]PeerLink, 65)
	for i := range links {
		links[i] = PeerLink{ID: babel.LinkID(i + 1), Device: f.nodes[1].Credentials.Device, Transport: "ble"}
	}
	d, rows := carrierDiagnostics{enabled: true}, 0
	d.emitWarm(now, n.Credentials.Device, links, routing, view.RouteProofs, n.routing.HasUsableOriginOnLink, func([]byte) { rows++ })
	if rows != 64 {
		t.Fatalf("unbounded diagnostic rows=%d", rows)
	}
}
