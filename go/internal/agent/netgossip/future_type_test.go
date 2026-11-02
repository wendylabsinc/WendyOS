package netgossip

import (
	"bytes"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

func TestFutureArtifactTypeSkipsVariantPayloadWithoutSessionState(t *testing.T) {
	for _, kind := range []uint8{3, 255} {
		wire, err := netwire.Marshal(map[string]any{"v": Version, "t": kind,
			"o": map[string]any{"futureOperation": true}, "h": map[string]any{"futureDigest": "named"},
			"r": "future request", "d": []any{true, "future variant"}})
		if err != nil {
			t.Fatal(err)
		}
		var future Message
		if err := netwire.Unmarshal(wire, &future); err != nil {
			t.Fatal("decoded future payload as current schema:", err)
		}
		if !future.FutureType() || future.Version != Version || future.Type != kind || future.Op != "" || future.Digest != nil || future.Data != nil {
			t.Fatalf("future marker retained interpreted variant fields: %+v", future)
		}
		pool := NewPool()
		session := pool.NewSession(Manifest)
		defer session.Close()
		now := time.Now()
		for _, message := range []Message{future, {Version: Version, Type: kind, Op: "have", Digest: []byte("future variant")}} {
			for _, deferred := range []bool{false, true} {
				reply, body, err := session.receive(message, now, deferred)
				if err != nil || reply != nil || body != nil {
					t.Fatal("future type was not ignored", reply, body, err)
				}
			}
		}
		if len(pool.objects) != 0 || len(pool.pending) != 0 || pool.request != 0 || pool.bytes != 0 || pool.serving != 0 || len(session.seen) != 0 || len(session.unavailable) != 0 || len(session.out) != 0 {
			t.Fatal("future type changed hints, claims, quota or output")
		}
		source := NewPool().NewSession(Manifest)
		defer source.Close()
		have := source.Offer([]byte("next ordinary artifact"), now.Add(time.Minute), now)
		want, _, err := session.Receive(*have, now)
		if err != nil || want == nil {
			t.Fatal("next known artifact unusable", want, err)
		}
		data, _, err := source.Receive(*want, now)
		if err != nil || data == nil {
			t.Fatal("next known artifact unavailable", data, err)
		}
		_, body, err := session.Receive(*data, now)
		if err != nil || !bytes.Equal(body, []byte("next ordinary artifact")) {
			t.Fatal("next known artifact failed", body, err)
		}
		session.Resolve(body, now.Add(time.Minute), now, true)
	}
}

func TestFutureTypeKeepsKnownTypesAndVersionBehaviorStrict(t *testing.T) {
	for _, value := range []map[string]any{
		{"v": Version, "t": Manifest, "o": "have", "h": map[string]any{"bad": true}},
		{"v": Version, "t": Service, "o": map[string]any{"bad": true}, "h": []byte("x")},
		{"v": 2, "t": uint8(3), "o": map[string]any{"bad": true}},
	} {
		wire, err := netwire.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var message Message
		if err := netwire.Unmarshal(wire, &message); err == nil {
			t.Fatal("known schema or unsupported version variant was ignored", value)
		}
	}
	for _, message := range []Message{
		{Version: Version, Type: 0, Op: "have", Digest: make([]byte, 32)},
		{Version: Version, Type: Manifest, Op: "have", Digest: []byte("short")},
		{Version: Version, Type: Service, Op: "data", Digest: make([]byte, 32)},
		{Version: 2, Type: 3, Op: "have", Digest: make([]byte, 32)},
	} {
		if err := message.Validate(); err == nil || message.FutureType() {
			t.Fatalf("invalid known/current-version behavior changed: %+v", message)
		}
	}
	// Preserve the existing unknown-operation path even on a future version.
	wire, err := netwire.Marshal(map[string]any{"v": 2, "t": uint8(3), "o": "future-op", "h": map[string]any{"future": true}})
	if err != nil {
		t.Fatal(err)
	}
	var message Message
	if err := netwire.Unmarshal(wire, &message); err != nil || message.Op != "future-op" || message.FutureType() || message.Validate() != nil {
		t.Fatal("existing future-version unknown-op behavior changed", message, err)
	}
}
