package proto

import (
	"encoding/hex"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/protobuf/proto"
)

// Fixtures encoded from WendyOS 78e94b382de14253214381e750dba3ac2417bbf0
// descriptors before moving sources or applying D1/D2 package identities.
func TestAgentMovePreservesBaselineWire(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		message    proto.Message
	}{
		{"restart policy", "080310f9ffffffffffffffff01", &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_ON_FAILURE, OnFailureMaxRetries: -7}},
		{"sensor data", "082a102a182a202a282a320600ff77697265", &sensorlinkpb.SensorData{ChannelId: 42, FrameSeq: 42, ChunkSeq: 42, TsUs: 42, Flags: 42, Payload: []byte{0, 255, 'w', 'i', 'r', 'e'}}},
		{"recording reference", "0a0766697874757265102a", &recordingpb.SampleRef{SourceId: "fixture", SampleId: 42}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := hex.DecodeString(tc.wire)
			if err != nil {
				t.Fatal(err)
			}
			decoded := tc.message.ProtoReflect().New().Interface()
			if err := proto.Unmarshal(wire, decoded); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(decoded, tc.message) {
				t.Fatalf("baseline wire decoded as %v", decoded)
			}
			encoded, err := proto.Marshal(tc.message)
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(encoded) != tc.wire {
				t.Fatalf("wire changed: %x", encoded)
			}
		})
	}
}
