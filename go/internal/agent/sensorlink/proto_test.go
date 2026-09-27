package sensorlink_test

import (
	"testing"

	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/protobuf/proto"
)

func TestEnvelopeDataRoundTrip(t *testing.T) {
	in := &sensorlinkpb.Envelope{Msg: &sensorlinkpb.Envelope_Data{Data: &sensorlinkpb.SensorData{
		ChannelId: 7, FrameSeq: 42, ChunkSeq: 3, TsUs: 1234, Flags: 1, Payload: []byte("jpegbytes"),
	}}}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out sensorlinkpb.Envelope
	if err := proto.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	d := out.GetData()
	if d == nil || d.ChannelId != 7 || d.FrameSeq != 42 || d.ChunkSeq != 3 || string(d.Payload) != "jpegbytes" {
		t.Fatalf("round-trip mismatch: %+v", d)
	}
}
