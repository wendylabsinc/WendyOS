package localmesh

import (
	"bytes"
	"encoding/binary"
	"sync"
	"testing"

	quic "github.com/quic-go/quic-go"
)

// Model TrySendDatagram's owned copy without retaining an unbounded packet
// history. This measures framing/admission allocations, not network throughput.
type benchmarkCarrierDatagrams struct {
	mu       sync.Mutex
	capacity int64
	full     bool
	packets  [32][]byte
	next     int
}

func (d *benchmarkCarrierDatagrams) CurrentMaxDatagramPayloadSize() (int64, error) {
	return d.capacity, nil
}

func (d *benchmarkCarrierDatagrams) TrySendDatagram(packet []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.full {
		return quic.ErrDatagramQueueFull
	}
	d.packets[d.next] = bytes.Clone(packet)
	d.next = (d.next + 1) % len(d.packets)
	return nil
}

func BenchmarkCarrierIPAdmission(b *testing.B) {
	for _, tc := range []struct {
		name     string
		size     int
		capacity int64
		full     bool
	}{
		{"small", 160, 1356, false},
		{"whole", TunnelMTU, 1356, false},
		{"fragmented", TunnelMTU, 1156, false},
		{"queue_full", TunnelMTU, 1356, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			packet := make([]byte, tc.size)
			packet[0] = 0x60
			binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)-40))
			sender := &carrierIPSender{sender: &benchmarkCarrierDatagrams{capacity: tc.capacity, full: tc.full}, negotiated: WholeIPDatagramLimit}
			b.SetBytes(int64(tc.size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := sender.sendIP(uint32(i), packet); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Concurrent streams share one per-link sender, as they do on the forwarding path.
func BenchmarkCarrierIPConcurrentAdmission(b *testing.B) {
	for _, full := range []bool{false, true} {
		name := "fragmented"
		if full {
			name = "queue_full"
		}
		b.Run(name, func(b *testing.B) {
			packet := make([]byte, TunnelMTU)
			packet[0] = 0x60
			binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)-40))
			sender := &carrierIPSender{sender: &benchmarkCarrierDatagrams{capacity: 1156, full: full}, negotiated: WholeIPDatagramLimit}
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if err := sender.sendIP(1, packet); err != nil {
						b.Error(err)
					}
				}
			})
		})
	}
}
