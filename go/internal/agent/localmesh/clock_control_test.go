package localmesh

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

func clockFrame(t *testing.T, stream *bytes.Buffer, payload map[string]any) {
	t.Helper()
	body, err := netwire.Marshal(map[string]any{"k": "clock", "x": payload})
	if err != nil {
		t.Fatal(err)
	}
	if err = binary.Write(stream, binary.BigEndian, uint32(len(body))); err != nil {
		t.Fatal(err)
	}
	stream.Write(body)
}

func TestClockFutureFramesPreserveFollowingControls(t *testing.T) {
	var stream bytes.Buffer
	for _, payload := range []map[string]any{
		{"v": uint32(2), "o": map[string]any{"future-op": true}, "i": "new-id"},
		{"v": uint32(1), "o": "future-op", "i": map[string]any{"future-id": true}, "d": "new-data"},
	} {
		clockFrame(t, &stream, payload)
	}
	clockFrame(t, &stream, map[string]any{"v": uint32(1), "o": "hello", "future-field": []string{"ignored"}})
	if err := WriteControl(&stream, ControlMessage{Kind: "hello", Hello: &LinkHello{}}); err != nil {
		t.Fatal(err)
	}
	n, p := &Node{}, &nodePeer{}
	for range 2 {
		m, err := ReadControl(&stream)
		if err != nil || m.Clock == nil || m.Clock.supported() {
			t.Fatalf("future clock not skipped: %+v %v", m, err)
		}
		if err = n.clockControl(p, m.Clock); err != nil || p.clockRelay || n.clockWorkers != nil || n.clockCalls != nil {
			t.Fatal("future frame changed clock state", err)
		}
	}
	m, err := ReadControl(&stream)
	if err != nil || !m.Clock.supported() || m.Clock.Op != "hello" {
		t.Fatalf("known extra-field frame lost: %+v %v", m, err)
	}
	m, err = ReadControl(&stream)
	if err != nil || m.Kind != "hello" {
		t.Fatalf("following frame lost: %+v %v", m, err)
	}
	if _, err = ReadControl(&stream); err != io.EOF {
		t.Fatal(err)
	}
}

func TestClockKnownMalformedFramesFail(t *testing.T) {
	for _, payload := range []map[string]any{
		{"v": uint32(0), "o": "hello"},
		{"v": uint32(1), "o": "hello", "i": uint64(1)},
		{"v": uint32(1), "o": "request", "i": uint64(1), "n": make([]byte, 31)},
		{"v": uint32(1), "o": "request", "i": uint64(0), "n": make([]byte, 32)},
		{"v": uint32(1), "o": "reply", "i": uint64(1), "d": make([]byte, 4097)},
		{"v": uint32(1), "o": "reply", "i": uint64(1)},
		{"v": uint32(1), "o": "error", "i": uint64(1), "n": []byte{1}},
	} {
		var stream bytes.Buffer
		clockFrame(t, &stream, payload)
		if _, err := ReadControl(&stream); err == nil {
			t.Fatal("malformed known clock accepted", payload)
		}
	}
}
