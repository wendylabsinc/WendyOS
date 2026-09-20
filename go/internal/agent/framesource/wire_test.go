package framesource

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestWire_RoundTripsADescriptorThenFrames(t *testing.T) {
	var buf bytes.Buffer
	src := &agentpbv2.CalibratedSource{Source: "realsense:123", ColourWidth: 640, ColourHeight: 480}
	if err := WriteRecord(&buf, RecordSource, src); err != nil {
		t.Fatalf("writing descriptor: %v", err)
	}
	for i := uint64(1); i <= 3; i++ {
		if err := WriteRecord(&buf, RecordFrame, &agentpbv2.CalibratedFrame{FrameId: i}); err != nil {
			t.Fatalf("writing frame %d: %v", i, err)
		}
	}

	gotSrc, err := ReadSource(&buf)
	if err != nil {
		t.Fatalf("reading descriptor: %v", err)
	}
	if gotSrc.GetSource() != "realsense:123" || gotSrc.GetColourWidth() != 640 {
		t.Errorf("descriptor = %+v", gotSrc)
	}
	for i := uint64(1); i <= 3; i++ {
		f, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("reading frame %d: %v", i, err)
		}
		if f.GetFrameId() != i {
			t.Errorf("frame id = %d, want %d", f.GetFrameId(), i)
		}
	}
	if _, _, err := ReadRecord(&buf); !errors.Is(err, io.EOF) {
		t.Errorf("end of stream = %v, want io.EOF", err)
	}
}

// A helper killed mid-frame must not look like one that finished. The agent
// distinguishes the two to decide whether the camera stopped or the capture
// broke, so this is the difference between a clean end and an error.
func TestWire_TruncatedRecordIsNotACleanEnd(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRecord(&buf, RecordFrame, &agentpbv2.CalibratedFrame{
		FrameId: 1, Colour: make([]byte, 512),
	}); err != nil {
		t.Fatal(err)
	}
	truncated := bytes.NewReader(buf.Bytes()[:buf.Len()-10])
	_, _, err := ReadRecord(truncated)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated record = %v, want io.ErrUnexpectedEOF", err)
	}
	if isCleanEnd(err) {
		t.Error("a truncated record was treated as a clean end of capture")
	}
}

// A length field is attacker-shaped input from another process: honouring it
// blindly would allocate whatever it asked for.
func TestWire_RefusesAnOversizedLengthWithoutAllocatingIt(t *testing.T) {
	header := make([]byte, 5)
	header[0] = RecordFrame
	binary.BigEndian.PutUint32(header[1:], 1<<30)
	_, _, err := ReadRecord(bytes.NewReader(header))
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("oversized record = %v, want a limit error", err)
	}
}

func TestWire_FrameWhereADescriptorWasExpectedIsAnError(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRecord(&buf, RecordFrame, &agentpbv2.CalibratedFrame{FrameId: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSource(&buf); err == nil {
		t.Error("a frame was accepted as a source descriptor")
	}
}
