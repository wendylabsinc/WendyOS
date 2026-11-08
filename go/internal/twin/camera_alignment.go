package twin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/twinvision"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type cameraAlignment struct {
	mu               sync.Mutex
	frame, source    string
	record           *pb.CameraMetadata
	landmarks        []byte
	first            twinvision.Frame
	result           twinvision.Result
	metadata         *pb.CameraMetadata
	verifiedAt       time.Time
	readyAt, expires time.Time
}
type cameraAlignmentRequest struct {
	Operation string           `json:"operation"`
	Token     string           `json:"token,omitempty"`
	RoomFrame string           `json:"roomFrame,omitempty"`
	Camera    string           `json:"camera,omitempty"`
	Automatic bool             `json:"automatic,omitempty"`
	Frame     twinvision.Frame `json:"frame,omitempty"`
}
type cameraAlignmentReply struct {
	PolicyVersion int               `json:"policyVersion"`
	Token         string            `json:"token,omitempty"`
	Result        twinvision.Result `json:"result"`
	Metadata      json.RawMessage   `json:"metadata,omitempty"`
}

func (s *Server) cameraRequest(w http.ResponseWriter, r *http.Request) {
	var q cameraAlignmentRequest
	if !decode(w, r, &q, 44<<20) {
		return
	}
	result, e := s.cameraOperation(r.Context(), q)
	respond(w, result, e)
}
func (s *Server) cameraOperation(ctx context.Context, q cameraAlignmentRequest) (cameraAlignmentReply, error) {
	receivedAt := time.Now()
	reply := cameraAlignmentReply{PolicyVersion: twinvision.PolicyVersion}
	if q.Operation == "begin" {
		if e := q.Frame.Validate(); e != nil {
			return reply, e
		}
		if q.Camera == "" || len(q.Camera) > 1024 {
			return reply, errors.New("camera source is required")
		}
		room, e := s.Store.Frame(q.RoomFrame)
		if e != nil {
			return reply, e
		}
		room, e = s.Store.Get(room.Site.ID)
		if e != nil {
			return reply, e
		}
		if !strings.EqualFold(room.Site.ID+"/"+room.Site.ModelRevision, q.RoomFrame) {
			return reply, ErrConflict
		}
		landmarks, e := twinvision.Landmarks(room.VisualMap, room.Model, room.Site.ModelRevision)
		if e != nil {
			return reply, e
		}
		records, e := pb.NewWendyCalibratedFrameServiceClient(s.Conn).ListCameraMetadata(ctx, &pb.ListCameraMetadataRequest{StableIds: []string{q.Camera}})
		if e != nil {
			return reply, e
		}
		record := &pb.CameraMetadata{StableId: q.Camera}
		found := false
		for _, v := range records.Cameras {
			if v.StableId == q.Camera {
				if found {
					return reply, errors.New("ambiguous camera metadata")
				}
				record = v
				found = true
			}
		}
		if q.Automatic {
			for _, p := range record.SitePoses {
				if strings.EqualFold(p.ModelRevision, room.Site.ModelRevision) && p.Method == pb.SiteCameraPose_METHOD_REPORTED && p.ValidUntilUnixMs > time.Now().UnixMilli() {
					return reply, errors.New("manual placement retained")
				}
			}
			for _, raw := range room.Site.Cameras {
				var c struct {
					SourceID string
					Position *Point
				}
				if json.Unmarshal(raw, &c) == nil && c.SourceID == q.Camera && c.Position != nil {
					return reply, errors.New("manual placement retained")
				}
			}
		}
		lens, provenance, e := cameraLens(record, q.Frame)
		if e != nil {
			return reply, e
		}
		job := &cameraAlignment{frame: q.RoomFrame, source: q.Camera, record: record, landmarks: landmarks, first: q.Frame, expires: time.Now().Add(2 * time.Minute)}
		job.first.Gray = nil
		token := uuid.NewString()
		s.cameraMu.Lock()
		for key, old := range s.cameras {
			if time.Now().After(old.expires) {
				delete(s.cameras, key)
			}
		}
		if len(s.cameras) >= 4 {
			s.cameraMu.Unlock()
			return reply, errors.New("camera alignment queue is full")
		}
		for _, old := range s.cameras {
			if old.source == q.Camera {
				s.cameraMu.Unlock()
				return reply, errors.New("camera alignment is already running for this source")
			}
		}
		// Lock before publishing so an unguessable token never exposes partial state.
		job.mu.Lock()
		s.cameras[token] = job
		s.cameraMu.Unlock()
		defer job.mu.Unlock()
		q.Frame.AgeMS += float64(time.Since(receivedAt)) / float64(time.Millisecond)
		result, e := s.solveCamera(ctx, landmarks, q.Frame, lens, provenance, false)
		if e == nil {
			e = twinvision.Validate(result, q.Frame.Width, q.Frame.Height)
		}
		if e != nil {
			s.dropCamera(token)
			return reply, e
		}
		job.result = result
		job.readyAt = time.Now()
		reply.Token = token
		reply.Result = result
		return reply, nil
	}
	s.cameraMu.Lock()
	job := s.cameras[q.Token]
	s.cameraMu.Unlock()
	if job == nil {
		return reply, errors.New("camera alignment expired; start again")
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	s.cameraMu.Lock()
	active := s.cameras[q.Token] == job
	s.cameraMu.Unlock()
	if !active {
		return reply, errors.New("camera alignment was cancelled")
	}
	if q.Operation == "cancel" {
		s.dropCamera(q.Token)
		return reply, nil
	}
	if time.Now().After(job.expires) {
		s.dropCamera(q.Token)
		return reply, errors.New("camera alignment expired; start again")
	}
	if _, e := s.Store.Frame(job.frame); e != nil {
		s.dropCamera(q.Token)
		return reply, e
	}
	switch q.Operation {
	case "verify":
		if job.metadata != nil {
			return reply, errors.New("camera alignment is already verified")
		}
		if e := q.Frame.Validate(); e != nil {
			return reply, e
		}
		if q.Frame.Width != job.first.Width || q.Frame.Height != job.first.Height || q.Frame.Codec != job.first.Codec || q.Frame.Sequence <= job.first.Sequence || time.Since(job.readyAt) < 800*time.Millisecond || time.Since(job.readyAt)-time.Duration(q.Frame.AgeMS*float64(time.Millisecond)) < 800*time.Millisecond {
			return reply, errors.New("verification needs a new frame captured after the first solve")
		}
		provenance := int32(1)
		if job.result.FittedLens == 2 {
			provenance = 2
		}
		q.Frame.AgeMS += float64(time.Since(receivedAt)) / float64(time.Millisecond)
		second, e := s.solveCamera(ctx, job.landmarks, q.Frame, job.result.Lens(), provenance, true)
		if e != nil {
			return reply, e
		}
		verified, e := twinvision.Verify(job.result, second, q.Frame.Width, q.Frame.Height)
		if e != nil {
			return reply, e
		}
		job.result = verified
		job.metadata = alignmentMetadata(job.record, verified, job.first, strings.Split(job.frame, "/")[1], time.Now())
		job.landmarks = nil
		job.verifiedAt = time.Now()
		reply.Metadata, e = (protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true}).Marshal(job.metadata)
		if e != nil {
			return reply, e
		}
		reply.Result = verified
		reply.Token = q.Token
		return reply, nil
	case "commit":
		if job.metadata == nil {
			return reply, errors.New("camera alignment needs two verified frames")
		}
		if time.Since(job.verifiedAt) > 10*time.Second {
			return reply, errors.New("verified camera alignment expired; start again")
		}
		var response *pb.SetCameraMetadataResponse
		e := s.Store.withFrame(job.frame, func() error {
			saveCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			var err error
			response, err = pb.NewWendyCalibratedFrameServiceClient(s.Conn).SetCameraMetadata(saveCtx, &pb.SetCameraMetadataRequest{StableId: job.source, ExpectedRevision: job.record.Revision, Metadata: job.metadata})
			return err
		})
		if e != nil {
			return reply, e
		}
		s.dropCamera(q.Token)
		reply.Result = job.result
		reply.Metadata, e = (protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true}).Marshal(response.Metadata)
		return reply, e
	default:
		return reply, errors.New("unknown camera alignment operation")
	}
}
func (s *Server) dropCamera(token string) {
	s.cameraMu.Lock()
	delete(s.cameras, token)
	s.cameraMu.Unlock()
}
func (s *Server) solveCamera(ctx context.Context, l []byte, f twinvision.Frame, k [9]float64, p int32, v bool) (twinvision.Result, error) {
	queuedAt := time.Now()
	select {
	case s.cameraWorker <- struct{}{}:
		defer func() { <-s.cameraWorker }()
	case <-ctx.Done():
		return twinvision.Result{}, ctx.Err()
	}
	if f.AgeMS+float64(time.Since(queuedAt))/float64(time.Millisecond) > 1500 {
		return twinvision.Result{}, errors.New("camera frame expired while waiting for the matching worker")
	}
	if s.cameraSolver != nil {
		return s.cameraSolver(ctx, l, f, k, p, v)
	}
	return twinvision.Solve(ctx, l, f, k, p, v)
}
func cameraLens(record *pb.CameraMetadata, f twinvision.Frame) ([9]float64, int32, error) {
	var values [9]float64
	i := record.GetIntrinsics()
	p := int32(i.GetProvenance())
	mode := strings.ReplaceAll(record.CaptureMode, "H264 ", "H.264 ")
	if i == nil || int(i.Width) != f.Width || int(i.Height) != f.Height || mode != f.Mode() || (p != 1 && p != 2) {
		if p == 1 {
			return values, 0, errors.New("measured lens belongs to a different capture mode; select that mode or recalibrate the lens")
		}
		return values, 0, nil
	}
	values = [9]float64{float64(i.Fx), float64(i.Fy), float64(i.Cx), float64(i.Cy)}
	switch record.DistortionModel {
	case "none":
		if len(record.Distortion) != 0 {
			return values, 0, errors.New("invalid stored lens distortion")
		}
	case "brown_conrady":
		if len(record.Distortion) != 5 {
			return values, 0, errors.New("invalid stored lens distortion")
		}
		copy(values[4:], record.Distortion)
	default:
		return values, 0, errors.New("unsupported stored lens distortion")
	}
	if e := twinvision.ValidateLens(values, f.Width, f.Height); e != nil {
		return values, 0, e
	}
	return values, p, nil
}
func alignmentMetadata(old *pb.CameraMetadata, r twinvision.Result, f twinvision.Frame, revision string, now time.Time) *pb.CameraMetadata {
	m := proto.Clone(old).(*pb.CameraMetadata)
	source := []string{"measured lens", "joint room fit", "stored lens estimate", "field-of-view search"}[r.Source]
	if r.FittedLens != 0 {
		provenance := pb.CameraIntrinsics_PROVENANCE_MEASURED
		note := "Lens fitted against room references."
		if r.FittedLens == 2 {
			provenance = pb.CameraIntrinsics_PROVENANCE_ASSUMED
			note = "Estimated lens; not measured. Source: " + source
		}
		m.Intrinsics = &pb.CameraIntrinsics{Width: uint32(f.Width), Height: uint32(f.Height), Fx: float32(r.FX), Fy: float32(r.FY), Cx: float32(r.CX), Cy: float32(r.CY), Provenance: provenance, Note: note}
		m.DistortionModel = "brown_conrady"
		m.Distortion = append([]float64(nil), r.Distortion[:]...)
		m.CaptureMode = f.Mode()
		m.CalibratedAtUnixMs = now.UnixMilli()
		m.SitePoses = nil
	}
	poses := m.SitePoses[:0]
	for _, p := range m.SitePoses {
		if !strings.EqualFold(p.ModelRevision, revision) && p.ValidUntilUnixMs > now.UnixMilli() {
			poses = append(poses, p)
		}
	}
	m.SitePoses = append(poses, &pb.SiteCameraPose{ModelRevision: revision, Position: append([]float64(nil), r.Transform[12:15]...), Rotation: twinvision.Quaternion(r.Transform), Method: pb.SiteCameraPose_METHOD_VISUAL_LOCALIZATION, Provenance: fmt.Sprintf("Linux twin camera alignment policy %d; %s; two fresh frames.", twinvision.PolicyVersion, source), ObservedAtUnixMs: now.UnixMilli(), ValidUntilUnixMs: now.Add(7 * 24 * time.Hour).UnixMilli(), ReprojectionRmsPixels: proto.Float64(r.RMS), InlierCount: uint32(r.Inliers)})
	return m
}
