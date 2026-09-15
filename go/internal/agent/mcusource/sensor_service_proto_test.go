package mcusource_test

import (
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

func TestSensorServiceTypesReuseSensorlinkPayloads(t *testing.T) {
	// StreamSensorsRequest carries channel ids; the stream yields sensorlinkpb.SensorFrame.
	req := &agentpbv2.StreamSensorsRequest{ChannelId: []uint32{1}}
	if len(req.ChannelId) != 1 {
		t.Fatal("channel id not set")
	}
	var _ *sensorlinkpb.SensorManifest = (*sensorlinkpb.SensorManifest)(nil) // manifest type is the shared one
	var _ agentpbv2.WendySensorServiceServer                                 // server iface exists
}
