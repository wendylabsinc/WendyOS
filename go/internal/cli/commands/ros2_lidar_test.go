package commands

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/ros2inspection"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/metadata"
)

type fakeLidarSamples struct {
	headers  metadata.MD
	messages []string
}

func (f *fakeLidarSamples) Header() (metadata.MD, error) { return f.headers, nil }
func (f *fakeLidarSamples) Recv() (*agentpbv2.ROS2Message, error) {
	if len(f.messages) == 0 {
		return nil, io.EOF
	}
	payload := f.messages[0]
	f.messages = f.messages[1:]
	return &agentpbv2.ROS2Message{Yaml: payload}, nil
}

func TestLidarCLIRequiresHandshakeAndValidGeometryEnvelope(t *testing.T) {
	opts := ros2inspection.DefaultLidarOptions()
	opts.Topic = "/cloud"
	valid := `{"schema_version":1,"topic":"/cloud","message_type":"sensor_msgs/msg/PointCloud2","status":"observed","sample_points":[[1,2,3]]}`
	headers := metadata.Pairs(ros2inspection.LidarMetadata, ros2inspection.LidarVersion, ros2inspection.ScopeMetadata, ros2inspection.HostScope)
	for _, tc := range []struct {
		name, payload string
		headers       metadata.MD
		wantErr       bool
	}{
		{"valid", valid, headers, false},
		{"old agent raw payload", "data: [0, 1, 2]", nil, true},
		{"wrong scope", valid, metadata.Pairs(ros2inspection.LidarMetadata, "1"), true},
		{"wrong topic", strings.Replace(valid, "/cloud", "/elsewhere", 1), headers, true},
		{"oversized", strings.Repeat(" ", ros2inspection.LidarMaxSummaryBytes+1), headers, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := drainLidarSamples(&fakeLidarSamples{tc.headers, []string{tc.payload}}, ros2inspection.HostScope, opts, &out)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			if tc.wantErr && out.Len() != 0 {
				t.Fatal("invalid payload escaped validation")
			}
			if !tc.wantErr && out.String() != valid+"\n" {
				t.Fatalf("changed summary: %s", out.String())
			}
		})
	}
}

func TestLidarCLIDoesNotTreatMissingDataAsSuccess(t *testing.T) {
	opts := ros2inspection.DefaultLidarOptions()
	opts.Topic = "/cloud"
	headers := metadata.Pairs(ros2inspection.LidarMetadata, "1")
	for _, messages := range [][]string{nil, {`{"schema_version":1,"topic":"/cloud","message_type":"sensor_msgs/msg/PointCloud2","status":"unknown"}`}} {
		if err := drainLidarSamples(&fakeLidarSamples{headers, messages}, ros2inspection.AppScope, opts, io.Discard); err == nil {
			t.Fatal("missing geometry was successful")
		}
	}
}
