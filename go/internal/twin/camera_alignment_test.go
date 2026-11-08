package twin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
	"github.com/wendylabsinc/wendy/go/internal/twinvision"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type cameraMetadataServer struct {
	pb.UnimplementedWendyCalibratedFrameServiceServer
	record *pb.CameraMetadata
	writes int
}

func (m *cameraMetadataServer) ListCameraMetadata(context.Context, *pb.ListCameraMetadataRequest) (*pb.ListCameraMetadataResponse, error) {
	return &pb.ListCameraMetadataResponse{Cameras: []*pb.CameraMetadata{proto.Clone(m.record).(*pb.CameraMetadata)}}, nil
}
func (m *cameraMetadataServer) SetCameraMetadata(_ context.Context, q *pb.SetCameraMetadataRequest) (*pb.SetCameraMetadataResponse, error) {
	if q.ExpectedRevision != m.record.Revision {
		return nil, status.Error(codes.Aborted, "revision changed")
	}
	m.record = proto.Clone(q.Metadata).(*pb.CameraMetadata)
	m.record.Revision++
	m.writes++
	return &pb.SetCameraMetadataResponse{Metadata: m.record}, nil
}
func cameraTestServer(t *testing.T) (*Server, *cameraMetadataServer, string) {
	t.Helper()
	store, e := NewStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	r := room()
	r.Model = []byte("PK\x03\x04fixture")
	r.VisualMap, _ = json.Marshal(map[string]any{"version": 1, "frameCount": 1, "modelRevision": r.Site.ModelRevision, "modelSHA256": fmt.Sprintf("%x", sha256.Sum256(r.Model)), "landmarks": make([]byte, 120*131*4)})
	if _, e = store.Put(r); e != nil {
		t.Fatal(e)
	}
	metadata := &cameraMetadataServer{record: &pb.CameraMetadata{StableId: "camera", Revision: 7}}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pb.RegisterWendyCalibratedFrameServiceServer(server, metadata)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, e := grpc.NewClient("passthrough:///camera", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { conn.Close() })
	s := &Server{Store: store, Conn: conn, Context: context.Background()}
	s.Handler()
	s.cameraSolver = func(_ context.Context, _ []byte, _ twinvision.Frame, l [9]float64, p int32, verify bool) (twinvision.Result, error) {
		result := twinvision.Result{Source: 1, Success: 1, Inliers: 30, FX: 500, FY: 500, CX: 16, CY: 16, RMS: 0.01, FittedLens: 1, Transform: [16]float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 1, 2, 3, 1}}
		if verify {
			result.FittedLens = 0
			result.FX = l[0]
			result.FY = l[1]
			result.CX = l[2]
			result.CY = l[3]
			copy(result.Distortion[:], l[4:])
			if p == 2 {
				result.FittedLens = 2
			}
		}
		return result, nil
	}
	return s, metadata, r.Site.ID + "/" + r.Site.ModelRevision
}
func cameraFrame(seq uint64) twinvision.Frame {
	return twinvision.Frame{Width: 32, Height: 32, Codec: "H.264", Sequence: seq, Gray: make([]byte, 1024)}
}
func TestCameraServiceWorkflowAndCancellation(t *testing.T) {
	for _, scenario := range []string{"commit", "cancel", "changed_metadata", "changed_room", "repeat", "mode", "stale", "early", "failure"} {
		t.Run(scenario, func(t *testing.T) {
			s, m, frame := cameraTestServer(t)
			ctx := context.Background()
			begin, e := s.cameraOperation(ctx, cameraAlignmentRequest{Operation: "begin", RoomFrame: frame, Camera: "camera", Frame: cameraFrame(1)})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.cameraOperation(ctx, cameraAlignmentRequest{Operation: "commit", Token: begin.Token}); e == nil {
				t.Fatal("unverified commit")
			}
			job := s.cameras[begin.Token]
			job.readyAt = time.Now().Add(-time.Second)
			second := cameraFrame(2)
			switch scenario {
			case "cancel":
				s.cameraOperation(ctx, cameraAlignmentRequest{Operation: "cancel", Token: begin.Token})
			case "repeat":
				second.Sequence = 1
			case "mode":
				second.Codec = "JPEG"
			case "stale":
				second.AgeMS = 2000
			case "early":
				job.readyAt = time.Now()
			case "failure":
				s.cameraSolver = func(context.Context, []byte, twinvision.Frame, [9]float64, int32, bool) (twinvision.Result, error) {
					return twinvision.Result{}, nil
				}
			}
			_, e = s.cameraOperation(ctx, cameraAlignmentRequest{Operation: "verify", Token: begin.Token, Frame: second})
			good := scenario == "commit" || scenario == "changed_metadata" || scenario == "changed_room"
			if (e == nil) != good {
				t.Fatalf("verify: %v", e)
			}
			if m.writes != 0 {
				t.Fatal("preview wrote metadata")
			}
			if scenario == "changed_metadata" {
				m.record.Revision++
			}
			if scenario == "changed_room" {
				r, _ := s.Store.Frame(frame)
				r.Removed = true
				s.Store.Put(r)
			}
			_, e = s.cameraOperation(ctx, cameraAlignmentRequest{Operation: "commit", Token: begin.Token})
			if (e == nil) != (scenario == "commit") {
				t.Fatalf("commit: %v", e)
			}
			if scenario == "commit" {
				if m.writes != 1 || m.record.Intrinsics.Provenance != pb.CameraIntrinsics_PROVENANCE_MEASURED {
					t.Fatal(m)
				}
				if _, e = s.cameraOperation(ctx, cameraAlignmentRequest{Operation: "commit", Token: begin.Token}); e == nil {
					t.Fatal("replayed commit")
				}
			} else if m.writes != 0 {
				t.Fatal("failed attempt changed metadata")
			}
		})
	}
}
func TestCameraLensCaptureModeAndProvenance(t *testing.T) {
	f := cameraFrame(1)
	r := &pb.CameraMetadata{Intrinsics: &pb.CameraIntrinsics{Width: 32, Height: 32, Fx: 30, Fy: 30, Cx: 16, Cy: 16, Provenance: pb.CameraIntrinsics_PROVENANCE_ASSUMED}, CaptureMode: f.Mode(), DistortionModel: "none"}
	if _, p, e := cameraLens(r, f); p != 2 || e != nil {
		t.Fatal(p, e)
	}
	r.CaptureMode = "H.264 640x480"
	if _, p, e := cameraLens(r, f); p != 0 || e != nil {
		t.Fatal(p, e)
	}
	r.Intrinsics.Provenance = pb.CameraIntrinsics_PROVENANCE_MEASURED
	if _, _, e := cameraLens(r, f); e == nil {
		t.Fatal("measured lens silently discarded")
	}
}
func TestCameraMetadataRetainsAssumedLensAndOtherMeasuredPoses(t *testing.T) {
	old := &pb.CameraMetadata{StableId: "camera", Intrinsics: &pb.CameraIntrinsics{Provenance: pb.CameraIntrinsics_PROVENANCE_MEASURED}, SitePoses: []*pb.SiteCameraPose{{ModelRevision: "other", ValidUntilUnixMs: time.Now().Add(time.Hour).UnixMilli()}}}
	r := twinvision.Result{FittedLens: 2, Source: 2, Transform: [16]float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 1, 2, 3, 1}}
	m := alignmentMetadata(old, r, cameraFrame(1), "new", time.Now())
	if m.Intrinsics.Provenance != pb.CameraIntrinsics_PROVENANCE_ASSUMED || len(m.SitePoses) != 1 {
		t.Fatal(m)
	}
	r.FittedLens = 0
	m = alignmentMetadata(old, r, cameraFrame(1), "new", time.Now())
	if len(m.SitePoses) != 2 || !proto.Equal(m.Intrinsics, old.Intrinsics) {
		t.Fatal(m)
	}
}

// Both UI clients speak this API; no client-specific path changes the decision.
func TestCameraHTTPContract(t *testing.T) {
	s, _, frame := cameraTestServer(t)
	body, _ := json.Marshal(cameraAlignmentRequest{Operation: "begin", RoomFrame: frame, Camera: "camera", Frame: cameraFrame(1)})
	w := httptest.NewRecorder()
	s.cameraRequest(w, httptest.NewRequest("POST", "/v1/camera-alignment", bytes.NewReader(body)))
	var out cameraAlignmentReply
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.PolicyVersion != twinvision.PolicyVersion || out.Token == "" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

type observerRobot struct {
	robotControl
	polls, observations, closes int
}

func (r *observerRobot) Snapshot() twinrobot.SiteRobotSnapshot {
	r.polls++
	return twinrobot.SiteRobotSnapshot{}
}
func (r *observerRobot) Status() twinrobot.SiteRobotSnapshot {
	r.observations++
	return twinrobot.SiteRobotSnapshot{}
}
func (r *observerRobot) Close() { r.closes++ }
func TestObserversCannotReplaceOrRenewOperator(t *testing.T) {
	s := &Server{}
	handler := s.Handler()
	robot := &observerRobot{}
	session := &robotSession{robot: robot, serial: "robot", lastActivity: time.Now()}
	s.robots["operator"] = session
	s.observers["robot"] = session
	for _, operation := range []string{"observe", "open"} {
		body, _ := json.Marshal(twinrobot.Request{Operation: operation, Serial: "robot"})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/robot", bytes.NewReader(body)))
		want := 200
		if operation == "open" {
			want = 409
		}
		if w.Code != want {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if s.robots["operator"] != session || robot.closes != 0 || robot.polls != 0 || robot.observations != 1 {
		t.Fatal("observer changed operator control")
	}
}
