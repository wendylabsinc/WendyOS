package localmesh

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/asyncdiag"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const carrierDiagnosticInterval = 5 * time.Second
const carrierStatsPrefix = "wendy net carrier stats="
const warmCarrierPrefix = "wendy net carrier readiness="

// carrierDiagnostics belongs to the Node event loop. Diagnostics are opt-in at
// process startup and rate limited independently of routed-packet throughput.
// They expose no packet contents and do not add a goroutine or wire traffic.
type carrierDiagnostics struct {
	enabled  bool
	next     time.Time
	warmNext time.Time
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

func logCarrierDiagnostic(row []byte) { enqueueCarrierDiagnostic(carrierStatsPrefix, row) }

func enqueueCarrierDiagnostic(prefix string, row []byte) {
	if len(prefix)+len(row) <= asyncdiag.MaxMessageBytes {
		asyncdiag.Print(prefix + string(row))
	}
}

// Legal opaque principals can contain '&' and other URI text that HTML-safe
// JSON needlessly expands. These local log rows are not embedded in HTML.
// Check the complete encoded line before dispatch; an oversized observation is
// absent, never a truncated JSON record that could look like partial evidence.
func encodeCarrierDiagnostic(prefix string, value any) ([]byte, bool) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, false
	}
	row := bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})
	return row, len(prefix)+len(row) <= asyncdiag.MaxMessageBytes
}

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
		row, ok := encodeCarrierDiagnostic(carrierStatsPrefix, carrierDiagnostic{
			Version: 1, Time: now.UTC(), Self: self.Principal(), Peer: p.device.Principal(), Link: id,
			TX: link.sendIP.snapshot(), RX: p.assembler.Snapshot(),
		})
		if ok { // All fields are bounded, JSON-safe scalar values.
			emit(row)
		}
	}
}
