package localmesh

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

func timestampFrame(t *testing.T, stream *bytes.Buffer, timestamp map[string]any) {
	t.Helper()
	body, err := netwire.Marshal(map[string]any{"k": "time", "t": timestamp})
	if err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(stream, binary.BigEndian, uint32(len(body))); err != nil {
		t.Fatal(err)
	}
	stream.Write(body)
}

func TestTimestampFutureFramesPreserveFollowingControls(t *testing.T) {
	var stream bytes.Buffer
	for _, message := range []map[string]any{
		{"v": uint32(1), "o": "future-hint", "i": map[string]any{"future": true}, "d": "future-data"},
		{"v": uint32(2), "o": "next", "i": "future-id", "f": map[string]any{"future": true}},
		{"v": uint32(2), "o": map[string]any{"future-op": true}, "i": "future-id"},
		{"o": "next", "i": uint64(42), "f": uint32(7), "future-field": true},
		{"v": uint32(1), "o": "next", "i": uint64(43), "f": uint32(8)},
	} {
		timestampFrame(t, &stream, message)
	}
	if err := WriteControl(&stream, ControlMessage{Kind: "hello", Hello: &LinkHello{}}); err != nil {
		t.Fatal(err)
	}
	p := &nodePeer{timestamp: &timestampReply{id: 42, expires: time.Now().Add(-time.Second)}}
	n := &Node{}
	pending := p.timestamp
	for range 3 {
		message, err := ReadControl(&stream)
		if err != nil || message.Time == nil || message.Time.supported() {
			t.Fatalf("future timestamp was not discarded: %+v %v", message, err)
		}
		n.timestampControl(p, message.Time)
		if p.timestamp != pending || n.timestampWorkers != nil {
			t.Fatal("future timestamp changed existing job, deadline or admission")
		}
	}
	for _, id := range []uint64{42, 43} {
		message, err := ReadControl(&stream)
		if err != nil || message.Time == nil || message.Time.ID != id || !message.Time.supported() {
			t.Fatalf("known timestamp after future frame lost: %+v %v", message, err)
		}
	}
	message, err := ReadControl(&stream)
	if err != nil || message.Kind != "hello" {
		t.Fatalf("following control lost: %+v %v", message, err)
	}
	if _, err := ReadControl(&stream); err != io.EOF {
		t.Fatal("unexpected remaining frame", err)
	}
}

func TestTimestampWritesExplicitVersionWithoutChangingCaller(t *testing.T) {
	original := &TimestampControl{ID: 42, Op: "next", Offset: 7}
	var stream bytes.Buffer
	if err := WriteControl(&stream, ControlMessage{Kind: "time", Time: original}); err != nil {
		t.Fatal(err)
	}
	message, err := ReadControl(&stream)
	if err != nil || message.Time.Version != timestampControlVersion || original.Version != 0 {
		t.Fatalf("timestamp version not explicit or caller changed: %+v %v", message, err)
	}
}

func TestTimestampKnownMalformedFrameStillFails(t *testing.T) {
	for _, message := range []map[string]any{
		{"v": uint32(1), "o": "next", "i": uint64(42), "f": uint32(0)},
		{"v": uint32(0), "o": "reply", "i": uint64(42), "f": uint32(0), "l": uint32(2), "d": []byte{1, 2, 3}},
		{"v": uint32(1), "i": uint64(42)},
	} {
		var stream bytes.Buffer
		timestampFrame(t, &stream, message)
		if _, err := ReadControl(&stream); err == nil {
			t.Fatal("known malformed timestamp accepted", message)
		}
	}
}

func TestTimestampKnownPullProgressesAfterFutureMessages(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	pending := &timestampReply{id: 42, data: []byte("nonce-bound reply"), offset: 1, expires: deadline}
	p := &nodePeer{control: make(chan ControlMessage, 1), timestamp: pending}
	n := &Node{}
	for _, message := range []*TimestampControl{
		{Version: 2, Op: "next", ID: 42, Offset: 1},
		{Version: 1, Op: "future-hint", ID: 42},
	} {
		n.timestampControl(p, message)
		if p.timestamp != pending || pending.pending || pending.expires != deadline || len(p.control) != 0 {
			t.Fatal("future timestamp changed the pending transfer")
		}
	}
	n.timestampControl(p, &TimestampControl{Version: 1, Op: "next", ID: 42, Offset: 1})
	select {
	case message := <-p.control:
		if message.Kind != "time" || message.Time.Version != 1 || message.Time.ID != 42 || message.Time.Offset != 1 || string(message.Time.Data) != "once-bound reply" || p.timestamp != nil {
			t.Fatalf("known pull did not complete after future messages: %+v", message)
		}
	default:
		t.Fatal("known pull did not progress after future messages")
	}
}
