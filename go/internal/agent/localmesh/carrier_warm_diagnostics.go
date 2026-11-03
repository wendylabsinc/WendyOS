package localmesh

import (
	"net/netip"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type warmCarrierDiagnostic struct {
	Version           int          `json:"version"`
	Time              time.Time    `json:"time"`
	Self              string       `json:"self"`
	Peer              string       `json:"peer"`
	Link              babel.LinkID `json:"link_id"`
	Transport         string       `json:"transport"`
	TwoWay            bool         `json:"two_way"`
	Retiring          bool         `json:"retiring"`
	CandidateUsable   bool         `json:"candidate_usable"`
	SelectedInstalled bool         `json:"selected_installed"`
	Origin            *RouteOrigin `json:"origin,omitempty"`
	ProofDeadline     time.Time    `json:"proof_deadline,omitempty"`
	ProofRetained     bool         `json:"proof_retained,omitempty"`
}

func logWarmCarrierDiagnostic(row []byte) {
	enqueueCarrierDiagnostic(warmCarrierPrefix, row)
}

// emitWarm runs on the Node actor and copies scalar observations before handing
// bytes to the bounded asynchronous logger. It neither changes route selection
// nor turns a discovery hint or a candidate on another link into readiness.
func (d *carrierDiagnostics) emitWarm(now time.Time, self meshid.Device, links []PeerLink, routing babel.Snapshot, proofs map[RouteOrigin]RouteProof, usable func(RouteOrigin, babel.LinkID) bool, emit func([]byte)) {
	if !d.enabled || now.Before(d.warmNext) {
		return
	}
	d.warmNext = now.Add(carrierDiagnosticInterval)
	// The Node admits at most 64 direct links. Index only those peer prefixes;
	// never copy or emit a mesh-wide candidate table. Each prefix has at most
	// one current and one retained proof. Unexpected excess fails closed here.
	direct := make(map[netip.Prefix][]RouteOrigin, min(len(links), 64))
	ids := make(map[babel.LinkID]bool, min(len(links), 64))
	for _, link := range links[:min(len(links), 64)] {
		if link.ID != 0 && link.Device.IsValid() {
			direct[netip.PrefixFrom(link.Device.MeshAddress(), 128)] = nil
			ids[link.ID] = true
		}
	}
	for origin := range proofs {
		if origins, ok := direct[origin.Prefix]; ok && len(origins) < 3 {
			direct[origin.Prefix] = append(origins, origin)
		}
	}
	twoWay := make(map[babel.LinkID]bool, min(len(links), 64))
	for _, neighbour := range routing.Neighbours {
		if ids[neighbour.Link] {
			twoWay[neighbour.Link] = neighbour.Reachable
		}
	}
	for _, link := range links[:min(len(links), 64)] {
		if !ids[link.ID] {
			continue
		}
		row := warmCarrierDiagnostic{Version: 1, Time: now.UTC(), Self: self.Principal(), Peer: link.Device.Principal(), Link: link.ID, Transport: diagnosticText(link.Transport, 32), TwoWay: twoWay[link.ID], Retiring: link.Retiring, SelectedInstalled: link.ForwardingReady && !link.Retiring}
		origins := direct[netip.PrefixFrom(link.Device.MeshAddress(), 128)]
		if row.TwoWay && !row.Retiring && len(origins) <= 2 && usable != nil {
			for _, origin := range origins {
				proof := proofs[origin]
				if proof.Deadline.After(now) && usable(origin, link.ID) && (!row.CandidateUsable || (row.ProofRetained && !proof.Retained)) {
					copy := origin
					row.Origin, row.ProofDeadline, row.ProofRetained, row.CandidateUsable = &copy, now.Add(proof.Deadline.Sub(now)).UTC(), proof.Retained, true
				}
			}
		}
		row.SelectedInstalled = row.SelectedInstalled && row.CandidateUsable
		if encoded, ok := encodeCarrierDiagnostic(warmCarrierPrefix, row); ok {
			emit(encoded)
		}
	}
}
