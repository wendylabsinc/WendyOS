package backend

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/macos/backend/sitenav"
)

type SiteFusionRequest struct {
	Frame        string              `json:"frame"`
	Architecture []float64           `json:"architecture"`
	Furniture    []float64           `json:"furniture"`
	Visual       *sitenav.VisualHint `json:"visual"`
	Reference    []sitenav.Surface   `json:"reference"`
}
type SiteScanPoint struct {
	X, Y, Z float64
	Class   int
}
type SiteFusionReport struct {
	Message             string                 `json:"message"`
	Candidates          []SiteRobotCandidate   `json:"candidates"`
	Tracking            sitenav.TrackingResult `json:"tracking"`
	Overlay             []SiteScanPoint        `json:"overlay"`
	Reference           []sitenav.Surface      `json:"reference"`
	Accepted, Rejected  int
	VisualUsed          bool   `json:"visualUsed"`
	ObservationRevision uint64 `json:"observationRevision"`
}

func fusionTriangles(values []float64) ([]sitenav.Triangle, error) {
	if len(values)%9 != 0 || len(values) > 90000 {
		return nil, errors.New("Unsupported localization geometry.")
	}
	mesh := make([]sitenav.Triangle, len(values)/9)
	for i := range mesh {
		for j := range mesh[i] {
			k := i*9 + j*3
			mesh[i][j] = sitenav.Vertex{X: values[k], Y: values[k+1], Z: values[k+2]}
		}
	}
	return mesh, nil
}

// FuseLocalization binds all evidence to the live handle, model revision and
// odometry epoch. It produces a preview only, never a movement command.
func (r *SiteRobot) FuseLocalization(sequence uint64, request SiteFusionRequest) (SiteFusionReport, error) {
	var report SiteFusionReport
	if len(request.Frame) == 0 || len(request.Frame) > 200 || len(request.Reference) > 3000 {
		return report, errors.New("Invalid localization request.")
	}
	architecture, err := fusionTriangles(request.Architecture)
	if err != nil {
		return report, err
	}
	furniture, err := fusionTriangles(request.Furniture)
	if err != nil {
		return report, err
	}
	r.mu.Lock()
	if sequence <= r.commandSequence || r.closed || r.ctx.Err() != nil {
		r.mu.Unlock()
		return report, errors.New("This localization request is no longer active.")
	}
	r.commandSequence = sequence
	if r.moving || r.stopUnconfirmed {
		if err = r.stopLocked(); err != nil {
			r.mu.Unlock()
			return report, err
		}
	}
	r.localizations = nil
	r.preview = nil
	if err = r.observed.SurfaceReady(time.Now()); err != nil {
		r.mu.Unlock()
		return report, err
	}
	pose, cloud, epoch := r.observed.Pose, r.observed.Surfaces, r.poseEpoch
	report.ObservationRevision = cloud.Fingerprint()
	input := sitenav.RegistrationInput{Cloud: cloud, Pose: pose, ClearFloor: r.observed.Grid.ObservedClearFloor(), Reference: request.Reference}
	input.Visual = pairedVisualHint(request.Visual, r.poseHistory, pose, time.Now())
	report.VisualUsed = input.Visual != nil

	r.mu.Unlock()
	model, err := sitenav.NewLayeredRoom(architecture, furniture)
	if err != nil {
		return report, err
	}
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	fits, err := sitenav.RegisterSurfaces(ctx, model, input)
	if err != nil {
		return report, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.ctx.Err() != nil || r.commandSequence != sequence || r.poseEpoch != epoch {
		return report, errors.New("This localization request was superseded.")
	}
	if err = r.observed.SurfaceReady(time.Now()); err != nil {
		return report, err
	}
	if !sameLocalizationPose(pose, r.observed.Pose) {
		return report, errors.New("Go2 moved during matching. Retrying with the new observations is required.")
	}
	if r.fusionFrame != request.Frame || r.fusionEpoch != epoch {
		r.fusionTracker = sitenav.PoseTracker{}
		r.fusionFrame = request.Frame
		r.fusionEpoch = epoch
	}
	report.Tracking = r.fusionTracker.Update(pose, cloud, fits, time.Now())
	report.Message = report.Tracking.Message
	report.Accepted = len(cloud.Surfaces)
	report.Rejected = len(cloud.Rejected)
	selected := report.Tracking.Selected
	if selected >= 0 {
		c := report.Tracking.Fits[selected].Pose
		alignment := sitenav.Alignment{Origin: pose.Position, Site: c.Position, Rotation: c.Rotation}
		r.localizations = []siteRobotLocalization{{alignment: alignment, pose: pose, frame: request.Frame, floorY: model.Base.FloorY, rms: c.RMS, coverage: c.Coverage, created: time.Now(), surfaceBased: true}}
		r.localizationSequence = sequence
		p := alignment.ToSite(r.observed.Pose.Position)
		report.Candidates = []SiteRobotCandidate{{p.X, -p.Y, r.observed.Pose.Yaw + c.Rotation, model.Base.FloorY, c.RMS, c.Coverage}}
		referenceCloud := cloud
		referenceCloud.Surfaces = append([]sitenav.Surface(nil), cloud.Surfaces...)
		classes := report.Tracking.Fits[selected].Classes
		for i := range referenceCloud.Surfaces {
			referenceCloud.Surfaces[i].Stable = i < len(classes) && classes[i] == 1
		}
		report.Reference = sitenav.SiteReference(referenceCloud, pose, c, model.Base.FloorY)
	}
	// The overlay is explicitly a provisional fit until the preview is confirmed.
	// A failed match still exposes the best hypothesis without rendering a robot.
	overlayIndex := selected
	if overlayIndex < 0 && len(report.Tracking.Fits) > 0 {
		overlayIndex = 0
	}
	if overlayIndex >= 0 {
		fit := report.Tracking.Fits[overlayIndex]
		a := sitenav.Alignment{Origin: pose.Position, Site: fit.Pose.Position, Rotation: fit.Pose.Rotation}
		for i, v := range cloud.Surfaces {
			p := a.ToSite(sitenav.Point{X: v.Position.X, Y: v.Position.Y})
			class := 0
			if i < len(fit.Classes) {
				class = fit.Classes[i]
			}
			report.Overlay = append(report.Overlay, SiteScanPoint{p.X, model.Base.FloorY + v.Position.Z - cloud.Floor, -p.Y, class})
		}
		for _, v := range cloud.Rejected {
			p := a.ToSite(sitenav.Point{X: v.X, Y: v.Y})
			report.Overlay = append(report.Overlay, SiteScanPoint{p.X, model.Base.FloorY + v.Z - cloud.Floor, -p.Y, 0})
		}
	}
	r.recordLocalizationEvidence(request, input, report, time.Now())
	return report, nil
}

// Camera hints use host arrival time and must have an odometry sample from that
// same instant. Compensate intervening motion before evaluating the live scan.
func pairedVisualHint(v *sitenav.VisualHint, history []sitenav.Pose, current sitenav.Pose, now time.Time) *sitenav.VisualHint {
	if v == nil || !finiteFusion(v.At) || v.At < 0 || v.At > float64(now.Unix())+1 ||
		!v.Position.Valid() || !finiteFusion(v.Yaw) || v.Inliers < 12 ||
		!finiteFusion(v.Residual) || v.Residual < 0 || v.Residual > 5 {
		return nil
	}
	captured := time.Unix(0, int64(v.At*1e9))
	if now.Sub(captured) < 0 || now.Sub(captured) >= 3*time.Second {
		return nil
	}
	var paired *sitenav.Pose
	for i := range history {
		p := &history[i]
		if math.Abs(p.Received.Sub(captured).Seconds()) < .15 &&
			(paired == nil || math.Abs(p.Received.Sub(captured).Seconds()) < math.Abs(paired.Received.Sub(captured).Seconds())) {
			paired = p
		}
	}
	if paired == nil {
		return nil
	}
	copy := *v
	s, c := math.Sincos(v.Yaw - paired.Yaw)
	dx, dy := current.Position.X-paired.Position.X, current.Position.Y-paired.Position.Y
	copy.Position.X += c*dx - s*dy
	copy.Position.Y += s*dx + c*dy
	copy.Yaw += math.Remainder(current.Yaw-paired.Yaw, 2*math.Pi)
	return &copy
}

func finiteFusion(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func (r *SiteRobot) FuseLocalizationJSON(sequence uint64, data []byte) ([]byte, error) {
	if len(data) > 12<<20 {
		return nil, errors.New("Localization request is too large.")
	}
	var input SiteFusionRequest
	if err := json.Unmarshal(data, &input); err != nil {
		return nil, err
	}
	report, err := r.FuseLocalization(sequence, input)
	if err != nil {
		return nil, err
	}
	return json.Marshal(report)
}
