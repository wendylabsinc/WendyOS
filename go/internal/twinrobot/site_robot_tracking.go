package twinrobot

import (
	"context"
	"errors"
	"log"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

type roomTracking struct {
	model sitenav.LayeredRoom
	frame string
	epoch uint64
}

// Geometry is bound to the imported model revision. It supplies no pose and
// cannot authorize motion. Manual placement can provide it along with Align.
func trackingGeometry(request SiteFusionRequest) (*roomTracking, error) {
	if request.Frame == "" || len(request.Frame) > 200 {
		return nil, errors.New("Invalid room frame.")
	}
	a, err := fusionTriangles(request.Architecture)
	if err != nil {
		return nil, err
	}
	f, err := fusionTriangles(request.Furniture)
	if err != nil {
		return nil, err
	}
	m, err := sitenav.NewLayeredRoom(a, f)
	if err != nil {
		return nil, err
	}
	return &roomTracking{model: m, frame: request.Frame}, nil
}

func (r *SiteRobot) AlignWithGeometry(sequence uint64, frame string, x, z, floor, yaw float64, request SiteFusionRequest) error {
	if request.Frame != frame {
		return errors.New("Room geometry frame mismatch.")
	}
	geometry, err := trackingGeometry(request)
	if err != nil {
		return err
	}
	if err = r.Align(sequence, frame, x, z, floor, yaw); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.commandSequence != sequence || r.frame != frame {
		return errors.New("Alignment superseded.")
	}
	geometry.epoch = r.poseEpoch
	r.roomTracking = geometry
	return nil
}

func (r *SiteRobot) runRoomTracking() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.refineRoomAlignment()
		}
	}
}

// Matching runs outside the observation/motion lock. Accepting its result never
// renews a lease, clears a fault, or starts an order. In-flight results are bound
// to the exact alignment, command, model, scan and odometry epoch they read.
func (r *SiteRobot) refineRoomAlignment() {
	r.mu.Lock()
	now := time.Now()
	g := r.roomTracking
	if g == nil || r.intentActive || r.closed || r.ctx.Err() != nil || r.frame == "" || r.frame != g.frame || r.poseEpoch != g.epoch || r.stopUnconfirmed || r.lookTurn != nil || r.discoveryMove != nil || r.observed.SurfaceReady(now) != nil || r.observed.CloudReceived == r.lastTrackingScan {
		r.mu.Unlock()
		return
	}
	pose, cloud, alignment, sequence := r.observed.Pose, r.observed.Surfaces, r.alignment, r.commandSequence
	captured := r.observed.CloudAt
	r.lastTrackingScan = r.observed.CloudReceived
	input := sitenav.RegistrationInput{Cloud: cloud, Pose: pose, ClearFloor: r.observed.Grid.ObservedClearFloor()}
	prior := sitenav.PoseCandidate{Position: alignment.ToSite(pose.Position), Rotation: alignment.Rotation}
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.ctx, 500*time.Millisecond)
	defer cancel()
	fits, err := sitenav.TrackSurfaces(ctx, g.model, input, prior)
	if err != nil {
		return
	}
	fit, ok := sitenav.SelectTrackingFit(fits, prior)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commitRoomFitLocked(g, pose, alignment, sequence, captured, fit)
}

func (r *SiteRobot) commitRoomFitLocked(g *roomTracking, pose sitenav.Pose, alignment sitenav.Alignment, sequence uint64, captured time.Time, fit sitenav.PoseCandidate) {
	if r.intentActive || r.lookTurn != nil || r.discoveryMove != nil || r.closed || r.ctx.Err() != nil || r.roomTracking != g || r.poseEpoch != g.epoch || r.frame != g.frame || r.alignment != alignment || r.commandSequence != sequence || r.stopUnconfirmed || !sitenav.Fresh(captured, time.Now()) || r.observed.SurfaceReady(time.Now()) != nil {
		return
	}
	r.applyRoomFitLocked(pose, fit)
}

func (r *SiteRobot) applyRoomFitLocked(pose sitenav.Pose, fit sitenav.PoseCandidate) {
	old := r.alignment
	current := old.ToSite(pose.Position)
	if current.Distance(fit.Position) < .015 && math.Abs(math.Remainder(old.Rotation-fit.Rotation, 2*math.Pi)) < .005 {
		return
	}
	updated := sitenav.Alignment{Origin: pose.Position, Site: fit.Position, Rotation: fit.Rotation}
	// Keep the original distance bound. Corrections must not reset travel limits.
	updated.Site = updated.ToSite(old.Origin)
	updated.Origin = old.Origin
	log.Printf("room alignment corrected translation=%.3fm yaw=%.3frad rms=%.3fm coverage=%.2f", current.Distance(fit.Position), math.Remainder(fit.Rotation-old.Rotation, 2*math.Pi), fit.RMS, fit.Coverage)
	r.alignment = updated
	r.localizations = nil
	r.preview = nil
	if r.moving {
		// Preserve the user's site destination, then stop and replan through the
		// existing live clearance checks. Never continue an old odometry route.
		r.goal = updated.ToRobot(old.ToSite(r.goal))
		r.replanLocked(r.navigationGridLocked(r.departure), r.observed.Pose.Position)
	}
}
