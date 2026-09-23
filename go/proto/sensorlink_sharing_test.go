package proto

import (
	"testing"

	protobuf "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"

	_ "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// wendy_com_msg.proto is shared verbatim with the wendy-lite project, where it
// sits beside sensorlink.proto in one directory and imports it by bare name.
// Here sensorlink.proto lives at wendy/lite/sensorlink.proto and is registered
// under that path, because the agent's v2 sensor service imports it that way.
// So the two importers name the same file differently, and the WendyCom
// descriptor's import cannot resolve. The tests below pin down what that does
// and does not cost: the Go types and the field descriptors are the real
// shared ones, only the unused import listing is a placeholder.

// TestSensorlinkMessagesRegisterOnce fails if litepb ever starts carrying its
// own copy of the sensorlink messages instead of importing sensorlinkpb, which
// would panic every binary linking both — the names would be registered twice.
func TestSensorlinkMessagesRegisterOnce(t *testing.T) {
	for _, name := range []protoreflect.FullName{
		"wendy.lite.sensorlink.SensorFrame",
		"wendy.lite.sensorlink.SensorManifest",
		"wendy.agent.services.v2.WendySensorService",
	} {
		if _, err := protoregistry.GlobalFiles.FindDescriptorByName(name); err != nil {
			t.Errorf("%s not registered: %v", name, err)
		}
	}
}

// TestWendyComFieldsResolveSensorlinkMessages guards the part the unresolved
// import could plausibly break: protoc-gen-go wires cross-file message fields
// through its Go type table rather than by descriptor path, so these fields
// must still point at the real sensorlink descriptors.
func TestWendyComFieldsResolveSensorlinkMessages(t *testing.T) {
	for _, tc := range []struct {
		msg   protoreflect.ProtoMessage
		field protoreflect.Name
		want  protoreflect.FullName
	}{
		{&litepb.WendyComMessage{}, "sensor_frame", "wendy.lite.sensorlink.SensorFrame"},
		{&litepb.WendyComResponse{}, "sensor_link_manifest", "wendy.lite.sensorlink.SensorManifest"},
		{&litepb.WendyComCommand{}, "sensor_link_subscribe", "wendy.lite.sensorlink.Subscribe"},
	} {
		fd := tc.msg.ProtoReflect().Descriptor().Fields().ByName(tc.field)
		if fd == nil {
			t.Errorf("%s has no field %s", tc.msg.ProtoReflect().Descriptor().FullName(), tc.field)
			continue
		}
		if md := fd.Message(); md == nil || md.IsPlaceholder() || md.FullName() != tc.want {
			t.Errorf("field %s resolves to %v, want %s", tc.field, md, tc.want)
		}
	}
}

// TestWendyComSensorFrameRoundTrips is the end-to-end check: a frame the device
// sends over WendyCom has to survive encode and decode across the package split.
func TestWendyComSensorFrameRoundTrips(t *testing.T) {
	in := &litepb.WendyComMessage{
		Msg: &litepb.WendyComMessage_SensorFrame{
			SensorFrame: &sensorlinkpb.SensorFrame{ChannelId: 3, Seq: 42, TsUs: 1234, Flags: 1, Payload: []byte("jpeg")},
		},
	}
	wire, err := protobuf.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out litepb.WendyComMessage
	if err := protobuf.Unmarshal(wire, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := out.GetSensorFrame()
	if got == nil {
		t.Fatal("sensor frame lost in round trip")
	}
	if got.ChannelId != 3 || got.Seq != 42 || string(got.Payload) != "jpeg" {
		t.Errorf("round trip changed frame: %+v", got)
	}
}
