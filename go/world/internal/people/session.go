// Package people connects image pose results to floor projection and anonymous
// temporal fusion. The simulation adapter preserves original image provenance;
// the projection and tracker themselves also accept measured camera geometry.
package people

import (
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/pose"
	"github.com/wendylabsinc/wendy/go/world/internal/presence"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/tracking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

var Anchor = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const Lifetime = 1250 * time.Millisecond

type Image struct {
	Sample sensing.Sample   `json:"sample"`
	Poses  pose.ImageResult `json:"poses"`
}
type Request struct {
	ParticipantID string          `json:"participant_id"`
	RunID         string          `json:"run_id"`
	Frame         world.FrameRef  `json:"frame"`
	NowNS         int64           `json:"now_ns"`
	FloorY        float64         `json:"floor_y"`
	Zones         []presence.Zone `json:"zones,omitempty"`
	Images        []Image         `json:"images"`
}
type CameraResult struct {
	SensorID    string           `json:"sensor_id"`
	SampleID    string           `json:"sample_id"`
	ImageSHA256 string           `json:"image_sha256"`
	Detected    int              `json:"detected"`
	Projected   int              `json:"projected"`
	Rejected    []pose.Rejection `json:"rejected"`
}
type Output struct {
	ParticipantID string           `json:"participant_id"`
	NowNS         int64            `json:"now_ns"`
	Tracks        []tracking.Track `json:"tracks"`
	Presence      *presence.Output `json:"presence,omitempty"`
	Cameras       []CameraResult   `json:"cameras"`
}
type Session struct {
	tracker          *tracking.Tracker
	presence         *presence.Engine
	participant, run string
	frame            world.FrameRef
	floor            float64
}

func (s *Session) Step(req Request) (Output, error) {
	out := Output{ParticipantID: req.ParticipantID, NowNS: req.NowNS, Tracks: []tracking.Track{}, Cameras: []CameraResult{}}
	if req.NowNS < 0 || req.NowNS > int64(24*time.Hour) || len(req.Images) > 16 {
		return out, errors.New("invalid people request bounds")
	}
	if s.tracker != nil && (s.participant != req.ParticipantID || s.run != req.RunID || s.frame != req.Frame || s.floor != req.FloorY) {
		return out, errors.New("participant, run or calibrated floor changed")
	}
	nextPresence := s.presence
	if nextPresence != nil && !nextPresence.Matches(req.Zones) {
		return out, errors.New("zones cannot change during a tracking session")
	}
	if nextPresence == nil && len(req.Zones) > 0 {
		var err error
		nextPresence, err = presence.New(req.Zones, req.RunID, "simulation:"+req.RunID, req.Frame)
		if err != nil {
			return out, err
		}
	}
	now := Anchor.Add(time.Duration(req.NowNS))
	detections := []tracking.Detection{}
	seen := map[string]bool{}
	for _, image := range req.Images {
		p := image.Sample
		if err := p.Validate(); err != nil {
			return out, err
		}
		if p.Kind != "camera" || p.RunID != req.RunID || p.Frame != req.Frame || seen[p.SensorID] || p.SampleNS > req.NowNS {
			return out, errors.New("camera capture scope, identity or clock differs")
		}
		seen[p.SensorID] = true
		if p.Evidence.SHA256 != image.Poses.ImageSHA256 {
			return out, errors.New("pose result does not reference the captured image")
		}
		deadline := Anchor.Add(time.Duration(p.SampleNS)).Add(min(Lifetime, time.Duration(p.Camera.CalibrationValidForNS)))
		if !now.Before(deadline) {
			continue
		}
		r, err := pose.Project(*p.Camera, image.Poses, pose.Config{FloorY: req.FloorY, AnkleHeightM: .08, FloorUncertaintyM: .08, CameraUncertaintyM: p.UncertaintyM})
		if err != nil {
			return out, err
		}
		out.Cameras = append(out.Cameras, CameraResult{p.SensorID, p.ID, p.Evidence.SHA256, len(image.Poses.Detections), len(r.Detections), r.Rejected})
		for _, d := range r.Detections {
			detections = append(detections, tracking.Detection{
				Source: p.SensorID, SampleID: p.ID, Version: world.Version{Epoch: 1, Sequence: p.Sequence},
				MemberIndex: d.MemberIndex, SampleTime: Anchor.Add(time.Duration(p.SampleNS)), ValidUntil: deadline,
				RunID: req.RunID, ClockDomain: "simulation:" + req.RunID, Frame: req.Frame,
				CalibrationRevision: p.CalibrationRevision, PositionXZ: d.PositionXZ, Covariance: d.Covariance,
				ModelID: image.Poses.ModelID, ModelVersion: image.Poses.ModelVersion, ModelSHA256: image.Poses.ModelSHA256,
				Evidence: []sensing.EvidenceRef{*p.Evidence},
			})
		}
	}
	if s.tracker == nil {
		t, err := tracking.New(tracking.Config{TrackerID: req.ParticipantID, RunID: req.RunID, ClockDomain: "simulation:" + req.RunID, Frame: req.Frame, MaxMiss: Lifetime})
		if err != nil {
			return out, err
		}
		s.tracker = t
		s.participant = req.ParticipantID
		s.run = req.RunID
		s.frame = req.Frame
		s.floor = req.FloorY
	}
	var err error
	out.Tracks, err = s.tracker.Step(now, detections)
	if err != nil {
		return out, err
	}
	if nextPresence != nil {
		result, e := nextPresence.Step(now, out.Tracks)
		if e != nil {
			return out, e
		}
		out.Presence = &result
		s.presence = nextPresence
	}
	return out, nil
}
