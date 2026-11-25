package udpmessage

import (
	"bytes"
	"testing"
	"time"
)

// Drive two interleaved messages through a small reference model. The operation
// stream chooses fragment order, duplicates, conflicting duplicates and time.
func FuzzMessageReassembly(f *testing.F) {
	f.Add([]byte{}, uint16(0), []byte{})
	f.Add([]byte("small"), uint16(252), []byte{})
	f.Add([]byte("hello"), uint16(237), []byte{0, 1, 0, 0, 1, 0, 1, 1})
	f.Add([]byte("conflict"), uint16(981), []byte{0, 0, 2, 0, 0, 1})
	f.Add([]byte("expiry"), uint16(1000), []byte{0, 0, 4, 0, 4, 0, 0, 1})
	f.Fuzz(func(t *testing.T, data []byte, size uint16, operations []byte) {
		if len(data) > 4096 || len(operations) > 512 {
			return
		}
		m, now := manualManager(t)
		e := endpoint(t, m, 1)
		stride := MinChunkSize - HeaderSize
		n := int(size) % (MaxMessageSize + 1)
		payloads := [2][]byte{make([]byte, n), make([]byte, n)}
		for i := range n {
			if len(data) != 0 {
				payloads[0][i] = data[i%len(data)]
			}
			payloads[1][i] = payloads[0][i] ^ 0xff
		}
		count := max(1, (n+stride-1)/stride)
		type messageState struct {
			born     time.Time
			seen     map[int]bool
			terminal bool
		}
		states := [2]messageState{{seen: map[int]bool{}}, {seen: map[int]bool{}}}
		send := func(id, index int, conflict bool) {
			t.Helper()
			for i := range states {
				s := &states[i]
				if len(s.seen) > 0 && !now.Before(s.born.Add(Lifetime)) {
					s.terminal = true
				}
			}
			s := &states[id]
			frame := encode(uint64(id+1), payloads[id], stride, index)
			// Only corrupt a duplicate, so a first fragment always belongs to
			// the original payload and the oracle never blesses mixed bytes.
			conflict = conflict && s.seen[index] && len(frame) > HeaderSize
			if conflict {
				frame[HeaderSize] ^= 1
			}
			wantDone := false
			if !s.terminal {
				if conflict {
					s.terminal = true
				} else {
					if len(s.seen) == 0 {
						s.born = *now
					}
					s.seen[index] = true
					wantDone = len(s.seen) == count
					s.terminal = wantDone
				}
			}
			got, done, err := receiveFrame(e, frame)
			if err != nil || done != wantDone || (done && !bytes.Equal(got, payloads[id])) {
				t.Fatalf("message %d fragment %d: done=%v want=%v err=%v payloadEqual=%v", id, index, done, wantDone, err, bytes.Equal(got, payloads[id]))
			}
			stats := m.Snapshot()
			// Small messages still incur the manager's 256-byte minimum charge.
			if stats.RXBytes < 0 || stats.RXBytes > 2*max(n, 256) || stats.RXMessages < 0 || stats.RXMessages > 2 {
				t.Fatalf("unbounded reservations: %+v", stats)
			}
		}
		for i := 0; i+1 < len(operations); i += 2 {
			op, index := operations[i], int(operations[i+1])%count
			if op%6 >= 4 {
				*now = now.Add(Lifetime / 2)
				continue
			}
			send(int(op%2), index, op%6 >= 2)
		}
		// Completing every surviving message also checks that missing and
		// duplicate fragments never cause early or repeated delivery.
		for i := count - 1; i >= 0; i-- {
			send(0, i, false)
			send(1, i, false)
		}
		assertEmpty(t, m)
		// An abandoned partial message must release its reservation on close.
		_, _, _ = receiveFrame(e, encode(3, make([]byte, stride+1), stride, 0))
		e.Close()
		assertEmpty(t, m)
	})
}
