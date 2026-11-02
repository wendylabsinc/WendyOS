//go:build linux

package localmesh

import (
	"encoding/json"
	"log"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const carrierDiagnosticInterval = 5 * time.Second

// carrierDiagnostics belongs to the Node event loop. Diagnostics are opt-in at
// process startup and rate limited independently of routed-packet throughput.
// They expose no packet contents and do not add a goroutine or wire traffic.
type carrierDiagnostics struct {
	enabled bool
	next    time.Time
}

type carrierDiagnostic struct {
	Version int             `json:"version" cbor:"v"`
	Time    time.Time       `json:"time" cbor:"t"`
	Self    string          `json:"self" cbor:"s"`
	Peer    string          `json:"peer" cbor:"p"`
	Link    babel.LinkID    `json:"link_id" cbor:"l"`
	TX      CarrierIPStats  `json:"tx" cbor:"T"`
	RX      IPAssemblyStats `json:"rx" cbor:"r"`
}

func logCarrierDiagnostic(row []byte) { log.Printf("wendy net carrier stats=%s", row) }

// emit reads the owner-only peer map. Counter snapshots alone are safe to read
// concurrently with their send/receive owners. A row is a diagnostic sample,
// not an atomic transaction across the QUIC capacity query and both counters.
func (d *carrierDiagnostics) emit(now time.Time, self meshid.Device, peers map[babel.LinkID]*nodePeer, emit func([]byte)) {
	if !d.enabled || now.Before(d.next) {
		return
	}
	d.next = now.Add(carrierDiagnosticInterval)
	for id, p := range peers {
		link, ok := p.link.(*quicNodeLink)
		if !ok || link.sendIP == nil {
			continue
		}
		row, err := json.Marshal(carrierDiagnostic{
			Version: 1, Time: now.UTC(), Self: self.Principal(), Peer: p.device.Principal(), Link: id,
			TX: link.sendIP.snapshot(), RX: p.assembler.Snapshot(),
		})
		if err == nil { // All fields are bounded, JSON-safe scalar values.
			emit(row)
		}
	}
}
