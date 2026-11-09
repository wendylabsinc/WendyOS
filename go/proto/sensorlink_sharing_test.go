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

// Both the Lite envelope and Agent sensor service import the canonical sensorlink descriptor.
// TestSensorlinkMessagesRegisterOnce fails if litepb ever starts carrying its
// own copy of the sensorlink messages instead of importing sensorlinkpb, which
// would panic every binary linking both — the names would be registered twice.
func TestSensorlinkMessagesRegisterOnce(t *testing.T) {
	for _, name := range []protoreflect.FullName{
		"wendylite.v1.SensorData",
		"wendylite.v1.SensorManifest",
		"wendy.agent.services.v2.WendySensorService",
	} {
		if _, err := protoregistry.GlobalFiles.FindDescriptorByName(name); err != nil {
			t.Errorf("%s not registered: %v", name, err)
		}
	}
}

// TestWendyComFieldsResolveSensorlinkMessages checks cross-file field descriptors.
func TestWendyComFieldsResolveSensorlinkMessages(t *testing.T) {
	for _, tc := range []struct {
		msg   protoreflect.ProtoMessage
		field protoreflect.Name
		want  protoreflect.FullName
	}{
		{&litepb.WendyComMessage{}, "sensor_data", "wendylite.v1.SensorData"},
		{&litepb.WendyComResponse{}, "sensor_link_manifest", "wendylite.v1.SensorManifest"},
		{&litepb.WendyComCommand{}, "sensor_link_subscribe", "wendylite.v1.Subscribe"},
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

// TestWendyComSensorDataRoundTrips is the end-to-end check: a frame chunk the
// device sends over WendyCom has to survive encode and decode across the
// package split.
func TestWendyComSensorDataRoundTrips(t *testing.T) {
	in := &litepb.WendyComMessage{
		Msg: &litepb.WendyComMessage_SensorData{
			SensorData: &sensorlinkpb.SensorData{ChannelId: 3, FrameSeq: 42, ChunkSeq: 2, TsUs: 1234, Flags: 3, Payload: []byte("jpeg")},
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
	got := out.GetSensorData()
	if got == nil {
		t.Fatal("sensor data lost in round trip")
	}
	if got.ChannelId != 3 || got.FrameSeq != 42 || got.ChunkSeq != 2 || got.Flags != 3 || string(got.Payload) != "jpeg" {
		t.Errorf("round trip changed sensor data: %+v", got)
	}
}

func TestWendyComSensorlinkImportResolves(t *testing.T) {
	imports := (&litepb.WendyComMessage{}).ProtoReflect().Descriptor().ParentFile().Imports()
	if imports.Len() != 1 || imports.Get(0).IsPlaceholder() || imports.Get(0).Path() != "wendylite/v1/sensorlink.proto" {
		t.Fatalf("sensorlink import did not resolve: %v", imports)
	}
}
