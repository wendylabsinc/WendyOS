package sensorlink_test

import (
	"testing"

	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/protobuf/proto"
)

func TestEnvelopeFrameRoundTrip(t *testing.T) {
	in := &sensorlinkpb.Envelope{Msg: &sensorlinkpb.Envelope_Frame{Frame: &sensorlinkpb.SensorFrame{
		ChannelId: 7, Seq: 42, TsUs: 1234, Flags: 1, Payload: []byte("jpegbytes"),
	}}}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out sensorlinkpb.Envelope
	if err := proto.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	f := out.GetFrame()
	if f == nil || f.ChannelId != 7 || f.Seq != 42 || string(f.Payload) != "jpegbytes" {
		t.Fatalf("round-trip mismatch: %+v", f)
	}
}
