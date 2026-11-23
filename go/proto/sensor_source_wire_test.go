package proto_test

import (
	"testing"

	sensorpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Build the previous descriptor shape to verify both directions without a
// second generated Go package registering the same protobuf symbols.
func TestSensorSourceWireCompatibility(t *testing.T) {
	file := protodesc.ToFileDescriptorProto(sensorpb.File_wendylite_v1_sensorlink_proto)
	for i, message := range file.MessageType {
		if message.GetName() == "SensorSource" {
			file.MessageType = append(file.MessageType[:i], file.MessageType[i+1:]...)
			break
		}
	}
	for _, message := range file.MessageType {
		if message.GetName() == "SensorDescriptor" {
			for i, field := range message.Field {
				if field.GetName() == "source" {
					message.Field = append(message.Field[:i], message.Field[i+1:]...)
					break
				}
			}
		}
	}
	oldFile, err := protodesc.NewFile(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := dynamicpb.NewMessage(oldFile.Messages().ByName("SensorManifest"))
	current := &sensorpb.SensorManifest{Sensors: []*sensorpb.SensorDescriptor{{ChannelId: 17, Name: "imu", Format: &sensorpb.SensorDescriptor_Sensor{Sensor: &sensorpb.SensorFormat{Schema: "example.imu.v1", SampleBytes: 24}}, Source: &sensorpb.SensorSource{PairingKey: "asset:3", ChannelId: 0, InputId: 0}}}}
	wire, err := proto.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(wire, old); err != nil {
		t.Fatalf("old reader rejected new manifest: %v", err)
	}
	oldWire, err := proto.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var restored sensorpb.SensorManifest
	if err := proto.Unmarshal(oldWire, &restored); err != nil {
		t.Fatal(err)
	}
	if len(restored.Sensors) != 1 || restored.Sensors[0].Source != nil {
		t.Fatal("old manifest unexpectedly requires source metadata")
	}
	expected := proto.Clone(current).(*sensorpb.SensorManifest)
	expected.Sensors[0].Source = nil
	if !proto.Equal(expected, &restored) {
		t.Fatalf("existing fields changed: %v", &restored)
	}
}
