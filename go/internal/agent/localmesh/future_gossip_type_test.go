package localmesh

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

func TestFutureGossipTypePreservesControlFramingAndConsumesNoResponseCredit(t *testing.T) {
	var stream bytes.Buffer
	future := map[string]any{"k": "gossip", "g": map[string]any{"v": 1, "t": 3,
		"o": map[string]any{"future": true}, "h": map[string]any{"future": "digest"}, "d": []any{"variant", true}}}
	for _, value := range []any{future, map[string]any{"k": "hello", "h": map[string]any{}}} {
		body, err := netwire.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := binary.Write(&stream, binary.BigEndian, uint32(len(body))); err != nil {
			t.Fatal(err)
		}
		stream.Write(body)
	}
	message, err := ReadControl(&stream)
	if err != nil || message.Gossip == nil || !message.Gossip.FutureType() {
		t.Fatal("future type broke carrier decoding", message, err)
	}
	if demand := directoryResponseDemand(message); demand != 0 {
		t.Fatal("future type reserved response credit", demand)
	}
	// Programmatic known Op must not reserve credit for an unknown family.
	message.Gossip.Op = "have"
	if demand := directoryResponseDemand(message); demand != 0 {
		t.Fatal("programmatic future type reserved response credit", demand)
	}
	directory, _, _, _, now := directoryFixture(t)
	for _, synchronizer := range []*Synchronizer{NewSynchronizer(directory, directory.cache), NewSynchronizer(directory, directory.cache, netgossip.NewPool())} {
		defer synchronizer.Close()
		replies, changed, err := synchronizer.Receive(message, now, 0)
		if err != nil || len(replies) != 0 || len(changed) != 0 || len(synchronizer.pending) != 0 || len(synchronizer.sent) != 0 {
			t.Fatal("future type changed admission/output state", replies, changed, err)
		}
	}
	if next, err := ReadControl(&stream); err != nil || next.Kind != "hello" {
		t.Fatal("known frame after future type failed", next, err)
	}
}

func TestFutureGossipTypeKeepsManifestControlKnownTypesStrict(t *testing.T) {
	for _, message := range []netgossip.Message{
		{Version: 1, Type: netgossip.Service, Op: "have", Digest: make([]byte, 32)},
		{Version: 1, Type: netgossip.Manifest, Op: "have", Digest: []byte("short")},
		{Version: 1, Type: 0, Op: "have", Digest: make([]byte, 32)},
		{Version: 2, Type: 3, Op: "have", Digest: make([]byte, 32)},
	} {
		var stream bytes.Buffer
		if err := WriteControl(&stream, ControlMessage{Kind: "gossip", Gossip: &message}); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadControl(&stream); err == nil {
			t.Fatalf("known wrong phase or invalid envelope ignored: %+v", message)
		}
	}
}
