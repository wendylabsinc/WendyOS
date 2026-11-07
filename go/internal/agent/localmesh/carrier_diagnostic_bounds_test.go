package localmesh

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/asyncdiag"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

func flushCarrierDiagnostics(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := asyncdiag.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

func longDiagnosticOperator(t *testing.T, opaque byte, instance string) meshid.Device {
	t.Helper()
	prefix := "spiffe://wendy.sh/tenant/" + selectionDevice(1).Tenant().String() + "/operator/"
	principal := prefix + strings.Repeat(string(opaque), meshoperator.MaxPrincipalBytes-len(prefix))
	if _, err := meshoperator.ParsePrincipal(principal); err != nil {
		t.Fatalf("test principal is not legal: %v", err)
	}
	peer, err := meshid.PeerFromPrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	peer, err = peer.WithInstance(uuid.MustParse(instance))
	if err != nil {
		t.Fatal(err)
	}
	return peer
}

func TestCarrierDiagnosticsKeepLegalOpaqueIdentitiesAsValidBoundedJSON(t *testing.T) {
	self := longDiagnosticOperator(t, '&', "11111111-1111-4111-8111-111111111111")
	peer := longDiagnosticOperator(t, '&', "22222222-2222-4222-8222-222222222222")
	flushCarrierDiagnostics(t)
	var output bytes.Buffer
	oldWriter, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	defer func() { flushCarrierDiagnostics(t); log.SetOutput(oldWriter); log.SetFlags(oldFlags) }()
	now := time.Now()
	origin := RouteOrigin{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128), RouterID: 7, Seqno: 9}
	proofs := map[RouteOrigin]RouteProof{origin: {Deadline: now.Add(time.Minute), Retained: true}}
	d := carrierDiagnostics{enabled: true}
	d.emitWarm(now, self, []PeerLink{{ID: 1, Device: peer, Transport: "ble"}}, babel.Snapshot{Neighbours: []babel.Neighbour{{Link: 1, Reachable: true}}}, proofs, func(RouteOrigin, babel.LinkID) bool { return true }, logWarmCarrierDiagnostic)
	d.emit(now, self, map[babel.LinkID]*nodePeer{1: {device: peer, link: &quicNodeLink{sendIP: &carrierIPSender{sender: &admissionFixture{capacity: 1356}, negotiated: WholeIPDatagramLimit}}}}, logCarrierDiagnostic)
	flushCarrierDiagnostics(t)
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("legal identity samples lost: lines=%d", len(lines))
	}
	for _, line := range lines {
		if len(line) > 4096 { // The existing logger contract, including its prefix.
			t.Fatalf("diagnostic exceeded logger bound: %d", len(line))
		}
		var body string
		switch {
		case strings.HasPrefix(line, "wendy net carrier readiness="):
			body = strings.TrimPrefix(line, "wendy net carrier readiness=")
		case strings.HasPrefix(line, "wendy net carrier stats="):
			body = strings.TrimPrefix(line, "wendy net carrier stats=")
		default:
			t.Fatalf("unexpected diagnostic: %q", line)
		}
		var row struct{ Self, Peer string }
		if err := json.Unmarshal([]byte(body), &row); err != nil {
			t.Fatalf("truncated or invalid JSON: %v", err)
		}
		if row.Self != self.Principal() || row.Peer != peer.Principal() {
			t.Fatal("legal identities were truncated or substituted")
		}
	}
}
