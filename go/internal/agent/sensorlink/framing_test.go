package sensorlink_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

func TestWriteReadRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	env := &sensorlinkpb.Envelope{Msg: &sensorlinkpb.Envelope_Frame{Frame: &sensorlinkpb.SensorFrame{ChannelId: 3, Payload: []byte("hi")}}}
	if err := sensorlink.WriteMessage(&buf, env); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := sensorlink.ReadMessage(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.GetFrame().ChannelId != 3 || string(got.GetFrame().Payload) != "hi" {
		t.Fatalf("mismatch: %+v", got)
	}
}

func TestReadRejectsOversizedFrame(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], sensorlink.MaxFrameBytes+1)
	_, err := sensorlink.ReadMessage(bytes.NewReader(hdr[:]))
	if err == nil {
		t.Fatal("expected error for oversized frame")
	}
}
