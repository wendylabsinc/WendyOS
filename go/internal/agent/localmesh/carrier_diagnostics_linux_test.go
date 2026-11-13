//go:build linux

package localmesh

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

func TestCarrierDiagnosticsOptInRateAndSchema(t *testing.T) {
	// No live connection or kernel setup is required: the production snapshot
	// reads the exact same admission contract used by a live QUIC connection.
	fixture := &admissionFixture{capacity: 1356}
	sender := &carrierIPSender{sender: fixture, negotiated: WholeIPDatagramLimit}
	sender.add(func(c *CarrierIPStats) { c.WholePackets, c.Fragments, c.Bytes = 2, 2, 2658 })
	self, remote := testDevices(t)
	peer := &nodePeer{device: remote, link: &quicNodeLink{sendIP: sender}}
	peers := map[babel.LinkID]*nodePeer{7: peer, 8: {link: nil}}
	now := time.Unix(1000, 123)
	var rows [][]byte
	emit := func(row []byte) { rows = append(rows, append([]byte(nil), row...)) }
	d := carrierDiagnostics{}
	d.emit(now, self, peers, emit)
	if len(rows) != 0 {
		t.Fatal("diagnostics enabled by default")
	}
	d.enabled = true
	d.emit(now, self, peers, emit)
	d.emit(now, self, peers, emit)
	d.emit(now.Add(carrierDiagnosticInterval-time.Nanosecond), self, peers, emit)
	if len(rows) != 1 {
		t.Fatalf("rows before interval: %d", len(rows))
	}
	var row carrierDiagnostic
	if err := json.Unmarshal(rows[0], &row); err != nil {
		t.Fatal(err)
	}
	if row.Version != 1 || row.Self != self.Principal() || row.Peer != remote.Principal() || row.Link != 7 || !row.Time.Equal(now) || row.TX.ConfiguredPacketSize != 1400 || row.TX.ConfiguredIPDatagram != WholeIPDatagramLimit || row.TX.NegotiatedIPDatagram != 1329 || !row.TX.CurrentDatagramKnown || row.TX.CurrentDatagram != 1356 || row.TX.WholePackets != 2 || row.TX.Bytes != 2658 {
		t.Fatalf("bad schema/sample: %+v", row)
	}
	d.emit(now.Add(carrierDiagnosticInterval), self, peers, emit)
	if len(rows) != 2 {
		t.Fatalf("rows after interval: %d", len(rows))
	}
	fixture.capacityErr = context.Canceled
	d.emit(now.Add(2*carrierDiagnosticInterval), self, peers, emit)
	if err := json.Unmarshal(rows[2], &row); err != nil {
		t.Fatal(err)
	}
	if row.TX.CurrentDatagramKnown || row.TX.CurrentDatagram != 0 {
		t.Fatalf("unavailable capacity misreported as a measurement: %+v", row.TX)
	}
}
