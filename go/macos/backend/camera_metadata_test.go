package backend

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

type alignmentAgent struct {
	pb.UnimplementedWendyCalibratedFrameServiceServer
	current *pb.CameraMetadata
	written *pb.SetCameraMetadataRequest
}

func (a *alignmentAgent) ListCameraMetadata(context.Context, *pb.ListCameraMetadataRequest) (*pb.ListCameraMetadataResponse, error) {
	return &pb.ListCameraMetadataResponse{Cameras: []*pb.CameraMetadata{a.current}}, nil
}
func (a *alignmentAgent) SetCameraMetadata(_ context.Context, r *pb.SetCameraMetadataRequest) (*pb.SetCameraMetadataResponse, error) {
	a.written = proto.Clone(r).(*pb.SetCameraMetadataRequest)
	return &pb.SetCameraMetadataResponse{}, nil
}
func TestSaveAlignmentPreservesOnlyCompatiblePosesAndUsesCAS(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	model := "A5CBDF03-4A58-431F-B6C5-98FE5D7E7A30"
	now := time.Now().UnixMilli()
	existing := &pb.CameraMetadata{StableId: "sensorlink:test:0", Revision: 7, Intrinsics: &pb.CameraIntrinsics{Width: 640, Height: 480, Fx: 500, Fy: 500}, CaptureMode: "JPEG 640x480", DistortionModel: "brown_conrady", Distortion: []float64{0, 0, 0, 0, 0}, CalibratedAtUnixMs: now - 10000,
		SitePoses: []*pb.SiteCameraPose{{ModelRevision: strings.ToLower(model), ValidUntilUnixMs: now + 60000}, {ModelRevision: "other", ValidUntilUnixMs: now + 60000}, {ModelRevision: "expired", ValidUntilUnixMs: now - 1}}}
	agent := &alignmentAgent{current: existing}
	pb.RegisterWendyCalibratedFrameServiceServer(server, agent)
	go server.Serve(listener)
	defer server.Stop()
	defer listener.Close()
	conn, err := grpc.NewClient("passthrough:///alignment", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	b := New()
	b.conn = grpcclient.NewFromConn(conn)
	b.life, b.stop = context.WithCancel(context.Background())
	b.session = 7
	defer b.Shutdown()
	input := proto.Clone(existing).(*pb.CameraMetadata)
	input.Revision = 0
	input.CalibratedAtUnixMs = now
	input.SitePoses = []*pb.SiteCameraPose{{ModelRevision: model, ValidUntilUnixMs: now + 60000}}
	if err = b.SaveCameraAlignment(7, input, 6); status.Code(err) != codes.Aborted || agent.written != nil {
		t.Fatalf("stale revision wrote calibration: %v", err)
	}
	if err = b.SaveCameraAlignment(8, input, 7); err == nil || agent.written != nil {
		t.Fatal("stale session wrote calibration")
	}
	if err = b.SaveCameraAlignment(7, input, 7); err != nil {
		t.Fatal(err)
	}
	saved := agent.written
	if saved.ExpectedRevision != 7 || len(saved.Metadata.SitePoses) != 2 || saved.Metadata.SitePoses[1].ModelRevision != "other" || saved.Metadata.CalibratedAtUnixMs != existing.CalibratedAtUnixMs {
		t.Fatalf("bad merge: %v", saved)
	}
	if len(input.SitePoses) != 1 || input.CalibratedAtUnixMs != now {
		t.Fatal("mutated caller's input")
	}
	input.Intrinsics.Fx = 520
	if err = b.SaveCameraAlignment(7, input, 7); err != nil {
		t.Fatal(err)
	}
	if len(agent.written.Metadata.SitePoses) != 1 || agent.written.Metadata.CalibratedAtUnixMs != now {
		t.Fatal("kept poses derived from a different lens calibration")
	}
	// Standalone lens measurement clears poses derived from another lens.
	input.SitePoses = nil
	input.Intrinsics.Provenance = pb.CameraIntrinsics_PROVENANCE_MEASURED
	if err = b.SaveCameraLens(7, input, 7); err != nil {
		t.Fatal(err)
	}
	if len(agent.written.Metadata.SitePoses) != 0 || agent.written.Metadata.Intrinsics.Provenance != pb.CameraIntrinsics_PROVENANCE_MEASURED {
		t.Fatal("standalone lens save produced a pose or lost measured provenance")
	}
	agent.current.Intrinsics.Provenance = pb.CameraIntrinsics_PROVENANCE_MEASURED
	assumed := proto.Clone(input).(*pb.CameraMetadata)
	assumed.Intrinsics.Provenance = pb.CameraIntrinsics_PROVENANCE_ASSUMED
	assumed.SitePoses = []*pb.SiteCameraPose{{ModelRevision: model, ValidUntilUnixMs: now + 60000}}
	if err = b.SaveCameraAlignment(7, assumed, 7); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("replaced measured lens with estimate: %v", err)
	}

}
