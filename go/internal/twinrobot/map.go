package twinrobot

import (
	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	"time"
)

// MappingSample does not renew the operator's motion lease. Returns Y-up room
// points only after alignment and fresh observations. Historical points never
// feed the navigation collision grid.
func (r *SiteRobot) MappingSample() (string, uint64, []sitenav.Vertex) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.ctx.Err() != nil || r.frame == "" || r.observed.SurfaceReady(time.Now()) != nil {
		return "", 0, nil
	}
	points := make([]sitenav.Vertex, 0, len(r.observed.Surfaces.Surfaces))
	for _, s := range r.observed.Surfaces.Surfaces {
		p := r.alignment.ToSite(sitenav.Point{X: s.Position.X, Y: s.Position.Y})
		points = append(points, sitenav.Vertex{X: p.X, Y: s.Position.Z - r.observed.FloorHeight + r.floorY, Z: -p.Y})
	}
	for _, v := range r.observed.LocalNavigationSamples {
		p := r.alignment.ToSite(sitenav.Point{X: v.X, Y: v.Y})
		points = append(points, sitenav.Vertex{X: p.X, Y: v.Z - r.observed.FloorHeight + r.floorY, Z: -p.Y})
	}
	return r.frame, uint64(r.observed.CloudReceived.UnixNano()), points
}

// ReleaseOperator ends control without ending the observation subscription.
// A new operator must obtain a new handle and cannot revive an old order.
func (r *SiteRobot) ReleaseOperator() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commandSequence = ^uint64(0)
	r.heartbeat = time.Time{}
	r.stopLocked()
}

// StartRoomFromOdometry requires an explicit empty-room intent. It establishes
// an origin from the current pose without issuing a movement command.
func (r *SiteRobot) StartRoomFromOdometry(sequence uint64, frame string) error {
	return r.Align(sequence, frame, 0, 0, 0, 0)
}
