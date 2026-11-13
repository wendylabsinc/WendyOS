package localmesh

import (
	"bytes"
	"encoding/binary"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"testing"
	"time"
)

func TestFutureControlKeepsStreamFraming(t *testing.T) {
	var stream bytes.Buffer
	for _, value := range []any{map[string]any{netwire.Key("kind"): "future-cache-summary", netwire.Key("hello"): map[string]any{"futureFormat": true}},
		map[string]any{netwire.Key("kind"): "gossip", netwire.Key("gossip"): map[string]any{netwire.Key("op"): "future-cache-hint", netwire.Key("data"): map[string]any{"mode": "compact"}}},
		map[string]any{netwire.Key("kind"): "hello", netwire.Key("hello"): map[string]any{}}} {
		body, err := netwire.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = binary.Write(&stream, binary.BigEndian, uint32(len(body))); err != nil {
			t.Fatal(err)
		}
		stream.Write(body)
	}
	for _, kind := range []string{"future-cache-summary", "gossip", "hello"} {
		m, err := ReadControl(&stream)
		if err != nil || m.Kind != kind {
			t.Fatalf("%s: %v %v", kind, m, err)
		}
		if kind == "gossip" {
			if _, _, err := (&Synchronizer{}).Receive(m, time.Now()); err != nil {
				t.Fatal("ignored future operation closed synchronizer", err)
			}
		}
	}
}
