package netgossip

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"testing"
	"time"
)

func TestFutureOperationLeavesSessionUsable(t *testing.T) {
	var future Message
	wire, err := netwire.Marshal(map[string]any{"op": "future-cache-hint", "data": map[string]any{"mode": "compact"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := netwire.Unmarshal(wire, &future); err != nil {
		t.Fatal(err)
	}
	s := NewPool().NewSession(Service)
	now := time.Now()
	if reply, data, err := s.Receive(future, now); err != nil || reply != nil || data != nil {
		t.Fatalf("future op: %v %v %v", reply, data, err)
	}
	source := NewPool().NewSession(Service)
	have := source.Offer([]byte("next ordinary object"), now.Add(time.Minute), now)
	if want, _, err := s.Receive(*have, now); err != nil || want == nil {
		t.Fatalf("session after extension: %v %v", want, err)
	}
	s.Close()
	source.Close()
}
