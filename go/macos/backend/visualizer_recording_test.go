package backend

import (
	"context"
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

type visualRecordingClient struct {
	pb.DataServiceClient
	start   *pb.DataStartRequest
	active  *pb.DataEpisode
	stopped bool
}

func (c *visualRecordingClient) Status(context.Context, *pb.DataStatusRequest, ...grpc.CallOption) (*pb.DataStatusResponse, error) {
	return &pb.DataStatusResponse{Active: c.active}, nil
}
func (c *visualRecordingClient) Start(_ context.Context, in *pb.DataStartRequest, _ ...grpc.CallOption) (*pb.DataEpisode, error) {
	c.start = in
	c.active = &pb.DataEpisode{Id: "new", State: "recording"}
	return c.active, nil
}
func (c *visualRecordingClient) Stop(context.Context, *pb.DataStopRequest, ...grpc.CallOption) (*pb.DataEpisode, error) {
	c.stopped = true
	c.active.State = "complete"
	return c.active, nil
}
func TestVisualRecordingUsesSelectedSessionAndSources(t *testing.T) {
	client := &visualRecordingClient{}
	b := New()
	b.session = 7
	b.conn = &grpcclient.AgentConnection{DataService: client}
	for _, session := range []uint64{0, 6} {
		if _, err := b.VisualRecordingStatus(session); err == nil {
			t.Fatal("accepted stale status")
		}
		if _, err := b.StartVisualRecording(session, "sensors", []string{"audio:mic"}); err == nil {
			t.Fatal("started on stale device")
		}
		if _, err := b.StopVisualRecording(session); err == nil {
			t.Fatal("stopped on stale device")
		}
	}
	if _, err := b.StartVisualRecording(7, "sensors", nil); err == nil {
		t.Fatal("empty selection implicitly recorded every source")
	}
	ids := []string{"audio:mic", "ros2:default:domain-0:/scan"}
	started, err := b.StartVisualRecording(7, "sensors", ids)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(client.start.GetSources(), ids) || client.start.GetName() != "sensors" || started.GetId() != "new" {
		t.Fatalf("wrong capture request: %+v", client.start)
	}
	active, err := b.VisualRecordingStatus(7)
	if err != nil || active.GetId() != "new" {
		t.Fatalf("wrong active recording: %+v %v", active, err)
	}
	stopped, err := b.StopVisualRecording(7)
	if err != nil || stopped.GetState() != "complete" || !client.stopped {
		t.Fatalf("stop failed: %+v %v", stopped, err)
	}
}
