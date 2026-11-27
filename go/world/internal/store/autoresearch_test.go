package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// This fixed workload measures preparation CPU cost, not storage latency. The
// normal package tests remain the durability, failure and repair acceptance gate.
// Keep this fixture and its expected output outside candidate edit permissions.
func autoresearchPreparationFixture(tb testing.TB) (*Store, *world.Replica, map[world.Source]uint64) {
	tb.Helper()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	cfg := world.Config{Tenant: "tenant", Site: "office", ClockDomain: "test", Now: func() time.Time { return now }, MaxEntries: 200, Frames: []world.FrameRef{{ID: "floor", Revision: "1"}}}
	r, err := world.NewReplica(cfg)
	if err != nil {
		tb.Fatal(err)
	}
	epochs := make(map[world.Source]uint64, 200)
	for i := 199; i >= 0; i-- {
		source := world.Source{Device: fmt.Sprintf("device-%03d", i), App: "app", Service: "tracker", Stream: "pose"}
		c := world.Component{Key: world.Key{Tenant: "tenant", Site: "office", Entity: fmt.Sprintf("entity-%03d", i), Type: "pose", Source: source}, Version: world.Version{Epoch: 1, Sequence: uint64(i + 1)}, Frame: cfg.Frames[0], Timing: world.Timing{SampleTime: now, ClockDomain: "test", ErrorKnown: true}, TTL: time.Hour, Provenance: world.Provenance{Origin: world.Measured, AppVersion: "v1"}, Payload: json.RawMessage(fmt.Sprintf(`{"x":%d,"y":2,"label":"retained"}`, i))}
		if i%8 == 0 {
			c.Tombstone, c.Payload = true, nil
		} else if i%8 == 1 {
			c.TTL = time.Second
		}
		if result, err := r.Apply(source, c); err != nil || result != world.Applied {
			tb.Fatalf("fixture apply: %s %v", result, err)
		}
		epochs[source] = uint64(i + 2)
	}
	now = now.Add(2 * time.Second)
	return &Store{config: cfg}, r, epochs
}

func TestAutoresearchPreparationRetainsExactState(t *testing.T) {
	s, r, epochs := autoresearchPreparationFixture(t)
	want, err := r.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := s.prepare(r, epochs)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, prepared.checkpoint) || prepared.replica != r || len(prepared.epochs) != 200 {
		t.Fatal("preparation changed retained checkpoint or epoch coverage")
	}
	var outer envelope
	if err := decode(prepared.data, &outer); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(outer.State)
	if outer.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("invalid persisted checksum")
	}
	var saved state
	if err := decode(outer.State, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Version != 1 || !reflect.DeepEqual(want, saved.World) || len(saved.Epochs) != 200 {
		t.Fatal("serialized state lost checkpoint or epochs")
	}
	for i, e := range saved.Epochs {
		if e.Source.Device != fmt.Sprintf("device-%03d", i) || e.Value != uint64(i+2) {
			t.Fatal("epoch order or value changed")
		}
	}
	// This hash seals the complete existing format, including retained expired
	// entries, tombstones, source order, frames, expiry caps and clock floor.
	const expected = "be4640b806984757fad47e076a568b7e3d4553bc5ca6192bdfb388dafcf39f15"
	actual := sha256.Sum256(prepared.data)
	if hex.EncodeToString(actual[:]) != expected {
		t.Fatalf("serialized fixture changed: sha256=%x", actual)
	}
	again, err := s.prepare(r, epochs)
	if err != nil || !bytes.Equal(prepared.data, again.data) {
		t.Fatal("identical preparation is not deterministic", err)
	}
	for i := range prepared.checkpoint.Entries {
		if len(prepared.checkpoint.Entries[i].Component.Payload) != 0 {
			prepared.checkpoint.Entries[i].Component.Payload[0] = '!'
			break
		}
	}
	after, err := r.Checkpoint()
	if err != nil || !reflect.DeepEqual(want, after) {
		t.Fatal("prepared checkpoint aliases retained replica", err)
	}
}

var autoresearchPreparationSink *preparedState

func BenchmarkAutoresearchPrepare200(b *testing.B) {
	s, r, epochs := autoresearchPreparationFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		prepared, err := s.prepare(r, epochs)
		if err != nil {
			b.Fatal(err)
		}
		autoresearchPreparationSink = prepared
	}
}
