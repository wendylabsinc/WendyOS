//go:build linux

package localmesh

import (
	"testing"
	"time"
)

func TestStreamIPQueueCompetingFlightsKeepAdmittedPrefix(t *testing.T) {
	for _, app := range []bool{false, true} {
		t.Run(map[bool]string{false: "bulk", true: "app QUIC"}[app], func(t *testing.T) {
			q := newStreamIPSendQueue()
			now := time.Now()
			packet := func(source byte) []byte {
				if app {
					return queueTestAppUDP(1248, source, 49200, AppSessionPort)
				}
				return queueTestIP(1248, source)
			}
			if !q.enqueue(1, packet(1), now) {
				t.Fatal("opening packet not admitted")
			}
			// Other senders can fill the remaining shared budget. Alternating
			// sender identities must not repeatedly replace the opening bytes
			// with the tail of each new flight while the writer is occupied.
			for id := uint32(2); id <= 20; id++ {
				q.enqueue(id, packet(byte(1+id%3)), now)
			}
			served := 0
			got, ok := q.pop(now, &served)
			if !ok || got.sequence != 1 {
				t.Fatalf("competing flights removed opening packet: first=%d, ok=%t", got.sequence, ok)
			}
		})
	}
}
