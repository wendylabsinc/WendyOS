package localmesh

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

// This exact test also runs unmodified against pre-clock main: an old reader
// skips the entire optional control kind and can read its next known frame.
func TestClockKindLegacyFraming(t *testing.T) {
	var stream bytes.Buffer
	body, err := netwire.Marshal(map[string]any{"k": "clock", "x": map[string]any{"v": uint32(1), "o": "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = binary.Write(&stream, binary.BigEndian, uint32(len(body))); err != nil {
		t.Fatal(err)
	}
	stream.Write(body)
	if err = WriteControl(&stream, ControlMessage{Kind: "hello", Hello: &LinkHello{}}); err != nil {
		t.Fatal(err)
	}
	if m, err := ReadControl(&stream); err != nil || m.Kind != "clock" {
		t.Fatalf("optional clock frame: %+v %v", m, err)
	}
	if m, err := ReadControl(&stream); err != nil || m.Kind != "hello" {
		t.Fatalf("following known frame: %+v %v", m, err)
	}
}
