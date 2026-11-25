package services

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// The store uses a gateway-local stable identity, with a separate pose for each
// model revision. A persisted lens calibration alone never implies a site pose.
type CameraMetadataStore struct {
	mu    sync.Mutex
	path  string
	state cameraMetadataState
}
type cameraMetadataState struct {
	Revision uint64                        `json:"revision"`
	Cameras  map[string]*pb.CameraMetadata `json:"cameras"`
}

func OpenCameraMetadataStore(path string) (*CameraMetadataStore, error) {
	s := &CameraMetadataStore{path: path, state: cameraMetadataState{Cameras: map[string]*pb.CameraMetadata{}}}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > 4<<20 {
		return nil, fmt.Errorf("camera metadata exceeds 4 MiB")
	}
	if err := json.NewDecoder(file).Decode(&s.state); err != nil {
		return nil, err
	}
	if s.state.Cameras == nil || len(s.state.Cameras) > 256 {
		return nil, fmt.Errorf("invalid camera metadata store")
	}
	for id, m := range s.state.Cameras {
		if m == nil || id != m.StableId || m.Revision == 0 || m.Revision > s.state.Revision {
			return nil, fmt.Errorf("invalid camera metadata revision")
		}
		if err := validateCameraMetadata(m); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *CalibratedFrameService) SetMetadataStore(store *CameraMetadataStore) { s.metadata = store }

func (s *CalibratedFrameService) ListCameraMetadata(_ context.Context, req *pb.ListCameraMetadataRequest) (*pb.ListCameraMetadataResponse, error) {
	if s.metadata == nil {
		return nil, status.Error(codes.FailedPrecondition, "camera metadata store unavailable")
	}
	if len(req.GetStableIds()) > 256 {
		return nil, status.Error(codes.InvalidArgument, "at most 256 camera identities")
	}
	store := s.metadata
	store.mu.Lock()
	defer store.mu.Unlock()
	result := &pb.ListCameraMetadataResponse{}
	keys := make([]string, 0, len(store.state.Cameras))
	for key := range store.state.Cameras {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if len(req.StableIds) == 0 || slices.Contains(req.StableIds, key) {
			result.Cameras = append(result.Cameras, proto.Clone(store.state.Cameras[key]).(*pb.CameraMetadata))
		}
	}
	return result, nil
}

func (s *CalibratedFrameService) SetCameraMetadata(ctx context.Context, req *pb.SetCameraMetadataRequest) (*pb.SetCameraMetadataResponse, error) {
	if s.metadata == nil {
		return nil, status.Error(codes.FailedPrecondition, "camera metadata store unavailable")
	}
	if !validMetadataIdentity(req.GetStableId()) {
		return nil, status.Error(codes.InvalidArgument, "exact stable camera identity required")
	}
	var next *pb.CameraMetadata
	if req.Metadata != nil {
		next = proto.Clone(req.Metadata).(*pb.CameraMetadata)
		if next.StableId != req.StableId {
			return nil, status.Error(codes.InvalidArgument, "camera identity mismatch")
		}
		if err := validateCameraMetadata(next); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		now := time.Now().UnixMilli()
		if next.CalibratedAtUnixMs > now+30_000 {
			return nil, status.Error(codes.InvalidArgument, "calibration timestamp is in the future")
		}
		for _, pose := range next.SitePoses {
			if pose.ObservedAtUnixMs > now+30_000 || pose.ValidUntilUnixMs <= now {
				return nil, status.Error(codes.InvalidArgument, "pose is expired or observed in the future")
			}
		}
	}
	store := s.metadata
	store.mu.Lock()
	defer store.mu.Unlock()
	current := store.state.Cameras[req.StableId]
	if current.GetRevision() != req.ExpectedRevision {
		return nil, status.Error(codes.Aborted, "camera metadata changed; read the current revision before replacing it")
	}
	if next == nil && current == nil {
		return nil, status.Error(codes.NotFound, "camera metadata not found")
	}
	if next != nil && current == nil && len(store.state.Cameras) >= 256 {
		return nil, status.Error(codes.ResourceExhausted, "camera metadata limit reached")
	}
	if store.state.Revision == math.MaxUint64 {
		return nil, status.Error(codes.ResourceExhausted, "metadata revision exhausted")
	}
	state := cameraMetadataState{Revision: store.state.Revision + 1, Cameras: make(map[string]*pb.CameraMetadata, len(store.state.Cameras)+1)}
	for id, m := range store.state.Cameras {
		state.Cameras[id] = m
	}
	if next == nil {
		delete(state.Cameras, req.StableId)
	} else {
		next.Revision = state.Revision
		state.Cameras[req.StableId] = next
	}
	data, err := json.Marshal(state)
	if err != nil || len(data) > 4<<20 {
		return nil, status.Error(codes.ResourceExhausted, "camera metadata limit reached")
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := os.MkdirAll(filepath.Dir(store.path), 0700); err != nil {
		return nil, status.Error(codes.Internal, "cannot create metadata directory")
	}
	file, err := os.CreateTemp(filepath.Dir(store.path), ".camera-metadata-*")
	if err != nil {
		return nil, status.Error(codes.Internal, "cannot write camera metadata")
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), store.path)
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "cannot persist camera metadata")
	}
	store.state = state // Commit in-memory state only after the atomic replacement.
	if next == nil {
		return &pb.SetCameraMetadataResponse{}, nil
	}
	return &pb.SetCameraMetadataResponse{Metadata: proto.Clone(next).(*pb.CameraMetadata)}, nil
}

func validMetadataIdentity(id string) bool {
	prefix, suffix, ok := strings.Cut(id, ":")
	return ok && suffix != "" && len(id) <= 1024 && strings.TrimSpace(id) == id &&
		slices.Contains([]string{"by-id", "by-path", "sensorlink", "ip-mac", "ros2"}, prefix)
}
func finite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

func validateCameraMetadata(m *pb.CameraMetadata) error {
	bad := func() error { return fmt.Errorf("invalid camera calibration or site pose") }
	if !validMetadataIdentity(m.StableId) || len(m.CaptureMode) > 200 || len(m.SitePoses) > 16 || (m.Intrinsics == nil && len(m.SitePoses) == 0) {
		return bad()
	}
	if k := m.Intrinsics; k != nil {
		if k.Width == 0 || k.Width > 8192 || k.Height == 0 || k.Height > 8192 ||
			!finite(float64(k.Fx), float64(k.Fy), float64(k.Cx), float64(k.Cy)) || k.Fx <= 0 || k.Fy <= 0 || k.Fx > 1e7 || k.Fy > 1e7 ||
			k.Cx < 0 || k.Cx > float32(k.Width) || k.Cy < 0 || k.Cy > float32(k.Height) ||
			(k.Provenance != pb.CameraIntrinsics_PROVENANCE_MEASURED && k.Provenance != pb.CameraIntrinsics_PROVENANCE_ASSUMED) ||
			len(k.Note) == 0 || len(k.Note) > 1024 || m.CalibratedAtUnixMs <= 0 || m.CaptureMode == "" {
			return bad()
		}
		if !(m.DistortionModel == "none" && len(m.Distortion) == 0 || m.DistortionModel == "brown_conrady" && len(m.Distortion) == 5) {
			return bad()
		}
		for _, d := range m.Distortion {
			if !finite(d) || math.Abs(d) > 100 {
				return bad()
			}
		}
	} else if m.DistortionModel != "" || len(m.Distortion) != 0 {
		return bad()
	}
	seen := map[string]bool{}
	for _, p := range m.SitePoses {
		if p == nil {
			return bad()
		}
		id, err := uuid.Parse(p.ModelRevision)
		if err != nil || id == uuid.Nil || seen[id.String()] {
			return bad()
		}
		seen[id.String()] = true
		if !strings.EqualFold(p.ModelRevision, id.String()) || len(p.Position) != 3 || len(p.Rotation) != 4 || !finite(p.Position...) || !finite(p.Rotation...) {
			return bad()
		}
		for _, v := range p.Position {
			if math.Abs(v) > 1e6 {
				return bad()
			}
		}
		norm := 0.0
		for _, v := range p.Rotation {
			norm += v * v
		}
		if math.Abs(norm-1) > 0.001 {
			return bad()
		}
		if p.Method != pb.SiteCameraPose_METHOD_REPORTED && p.Method != pb.SiteCameraPose_METHOD_VISUAL_LOCALIZATION {
			return bad()
		}
		if len(p.Provenance) == 0 || len(p.Provenance) > 1024 || p.ObservedAtUnixMs <= 0 || p.ValidUntilUnixMs <= p.ObservedAtUnixMs || p.ValidUntilUnixMs-p.ObservedAtUnixMs > 7*24*60*60*1000 {
			return bad()
		}
		if p.ReprojectionRmsPixels != nil && (!finite(*p.ReprojectionRmsPixels) || *p.ReprojectionRmsPixels < 0 || *p.ReprojectionRmsPixels > 1000) {
			return bad()
		}
		if p.Method == pb.SiteCameraPose_METHOD_VISUAL_LOCALIZATION {
			minimum := uint32(12)
			if m.GetIntrinsics().GetProvenance() == pb.CameraIntrinsics_PROVENANCE_ASSUMED {
				minimum = 8
			}
			if p.ReprojectionRmsPixels == nil || p.InlierCount < minimum {
				return bad()
			}
		}
	}
	return nil
}
