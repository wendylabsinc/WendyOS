package services

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func metadataFixture() *pb.CameraMetadata {
	now := time.Now().UnixMilli()
	return &pb.CameraMetadata{StableId: "sensorlink:device:0", CaptureMode: "640x480-full-sensor", CalibratedAtUnixMs: now,
		DistortionModel: "none", Intrinsics: &pb.CameraIntrinsics{Fx: 500, Fy: 500, Cx: 320, Cy: 240, Width: 640, Height: 480, Provenance: pb.CameraIntrinsics_PROVENANCE_MEASURED, Note: "test calibration"},
		SitePoses: []*pb.SiteCameraPose{{ModelRevision: "af2d6d4e-8ace-46ac-aaf2-98a9e93f1ff4", Position: []float64{1, 2, 3}, Rotation: []float64{0, 1, 0, 0}, Method: pb.SiteCameraPose_METHOD_REPORTED, Provenance: "test transform", ObservedAtUnixMs: now, ValidUntilUnixMs: now + 60000}}}
}

func TestCameraMetadataPersistenceCASAndRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	store, err := OpenCameraMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	svc := &CalibratedFrameService{metadata: store}
	ctx := context.Background()
	m := metadataFixture()
	first, err := svc.SetCameraMetadata(ctx, &pb.SetCameraMetadataRequest{StableId: m.StableId, Metadata: m})
	if err != nil {
		t.Fatal(err)
	}
	if m.Revision != 0 || first.Metadata.Revision != 1 {
		t.Fatal("request mutated or revision not assigned")
	}
	// Returned data must not mutate the store.
	first.Metadata.Intrinsics.Fx = 1
	store, err = OpenCameraMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	svc.metadata = store
	list, err := svc.ListCameraMetadata(ctx, &pb.ListCameraMetadataRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Cameras) != 1 || list.Cameras[0].Intrinsics.Fx != 500 {
		t.Fatal("calibration did not round-trip")
	}
	_, err = svc.SetCameraMetadata(ctx, &pb.SetCameraMetadataRequest{StableId: m.StableId, Metadata: m})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("stale write: %v", err)
	}
	unknown, _ := svc.ListCameraMetadata(ctx, &pb.ListCameraMetadataRequest{StableIds: []string{"sensorlink:other:0"}})
	if len(unknown.Cameras) != 0 {
		t.Fatal("metadata leaked across stable IDs")
	}
	_, err = svc.SetCameraMetadata(ctx, &pb.SetCameraMetadataRequest{StableId: m.StableId, ExpectedRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	recreated, err := svc.SetCameraMetadata(ctx, &pb.SetCameraMetadataRequest{StableId: m.StableId, Metadata: m})
	if err != nil {
		t.Fatal(err)
	}
	if recreated.Metadata.Revision <= 1 {
		t.Fatal("delete/create reused a revision")
	}
	_, err = svc.SetCameraMetadata(ctx, &pb.SetCameraMetadataRequest{StableId: m.StableId, ExpectedRevision: 1, Metadata: m})
	if status.Code(err) != codes.Aborted {
		t.Fatal("old writer overwrote recreated calibration")
	}
}

func TestCameraMetadataRejectsInvalidGeometry(t *testing.T) {
	// Foundation's UUID JSON representation is uppercase.
	uppercase := metadataFixture()
	uppercase.SitePoses[0].ModelRevision = strings.ToUpper(uppercase.SitePoses[0].ModelRevision)
	if err := validateCameraMetadata(uppercase); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*pb.CameraMetadata){
		"numeric identity":      func(m *pb.CameraMetadata) { m.StableId = "96" },
		"nan focal":             func(m *pb.CameraMetadata) { m.Intrinsics.Fx = float32(math.NaN()) },
		"unknown provenance":    func(m *pb.CameraMetadata) { m.Intrinsics.Provenance = 0 },
		"no mode":               func(m *pb.CameraMetadata) { m.CaptureMode = "" },
		"missing distortion":    func(m *pb.CameraMetadata) { m.DistortionModel = "" },
		"bad distortion":        func(m *pb.CameraMetadata) { m.DistortionModel = "brown_conrady"; m.Distortion = []float64{1} },
		"unnormalized rotation": func(m *pb.CameraMetadata) { m.SitePoses[0].Rotation[1] = 2 },
		"unknown frame":         func(m *pb.CameraMetadata) { m.SitePoses[0].ModelRevision = "map" },
		"duplicate frame": func(m *pb.CameraMetadata) {
			m.SitePoses = append(m.SitePoses, proto.Clone(m.SitePoses[0]).(*pb.SiteCameraPose))
		},
		"no expiry":               func(m *pb.CameraMetadata) { m.SitePoses[0].ValidUntilUnixMs = 0 },
		"unverified localization": func(m *pb.CameraMetadata) { m.SitePoses[0].Method = pb.SiteCameraPose_METHOD_VISUAL_LOCALIZATION },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := metadataFixture()
			mutate(m)
			if validateCameraMetadata(m) == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}

func TestCameraMetadataFailedSaveDoesNotPublishAndExpiryRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	store := &CameraMetadataStore{path: filepath.Join(path, "metadata.json"), state: cameraMetadataState{Cameras: map[string]*pb.CameraMetadata{}}}
	svc := &CalibratedFrameService{metadata: store}
	m := metadataFixture()
	_, err := svc.SetCameraMetadata(context.Background(), &pb.SetCameraMetadataRequest{StableId: m.StableId, Metadata: m})
	if status.Code(err) != codes.Internal || len(store.state.Cameras) != 0 || store.state.Revision != 0 {
		t.Fatal("failed disk write published in-memory data")
	}
	m.SitePoses[0].ObservedAtUnixMs = time.Now().Add(-time.Hour).UnixMilli()
	m.SitePoses[0].ValidUntilUnixMs = time.Now().Add(-time.Minute).UnixMilli()
	_, err = svc.SetCameraMetadata(context.Background(), &pb.SetCameraMetadataRequest{StableId: m.StableId, Metadata: m})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatal("expired pose accepted")
	}
}
