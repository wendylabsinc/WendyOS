package backend

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
	"github.com/wendylabsinc/wendy/go/macos/backend/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type SiteRobotSnapshot struct {
	LookDirection                                        int
	LookState                                            int // 0 idle, 1 turning, 2 completed, 3 aborted
	Message                                              string
	Pose, Ready, Aligned, Moving                         bool
	ArrivalTiming, UsingArrivalTiming, TimingUnavailable bool
	ScanReady                                            bool
	UnderBodyAllowance                                   bool
	X, Z, Yaw, FloorY                                    float64
	ObservationRevision                                  uint64
	Route                                                []sitenav.Point // site X, -Z
	Joints                                               []float64
}

// Each handle is bound to one authenticated device session and robot serial.
// UI polling renews a short lease, not motion itself. The agent independently
// expires MOVE after 500ms if this process or its transport disappears.
type SiteRobot struct {
	commandSequence                 uint64
	mu                              sync.Mutex
	ctx                             context.Context
	cancel                          context.CancelFunc
	done                            chan struct{}
	client                          pb.WendyUnitreeRobotServiceClient
	serial                          string
	videoFrames                     chan []byte
	observed                        sitenav.Observations
	alignment                       sitenav.Alignment
	frame                           string
	floorY                          float64
	route                           []sitenav.Point
	preview                         []sitenav.Point
	goal                            sitenav.Point
	moving                          bool
	lookTurn                        *siteLookTurn
	lookState                       int
	lookDirection                   int
	started, heartbeat              time.Time
	message                         string
	closed                          bool
	stopUnconfirmed                 bool
	underBodyAllowance              bool
	departure                       *sitenav.Departure
	localizations                   []siteRobotLocalization
	localizationSequence, poseEpoch uint64
	poseHistory                     []sitenav.Pose
	fusionTracker                   sitenav.PoseTracker
	fusionFrame                     string
	fusionEpoch                     uint64
	lastObservationLog              time.Time
}

func (b *Backend) OpenSiteRobot(session uint64, serial string) (*SiteRobot, error) {
	if !unitree.ValidSerial(serial) {
		return nil, errors.New("Choose a paired Go2.")
	}
	ctx, done := b.begin()
	defer done()
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	if !conn.IsMTLS && !conn.IsSessionProxy {
		return nil, errors.New("Connect to the gateway using its authenticated device connection.")
	}
	listed, err := conn.UnitreeService.ListUnitreePairings(ctx, &pb.ListUnitreePairingsRequest{})
	if err != nil {
		return nil, err
	}
	paired := false
	for _, r := range listed.GetRobots() {
		if r.Serial == serial && r.Paired && r.Connected {
			paired = true
		}
	}
	if !paired {
		return nil, errors.New("Pair and connect this Go2 on the selected gateway first.")
	}
	life, cancel := context.WithCancel(b.life)
	r := &SiteRobot{ctx: life, cancel: cancel, done: make(chan struct{}), client: pb.NewWendyUnitreeRobotServiceClient(conn.Conn), serial: serial, message: "Waiting for robot observations.", heartbeat: time.Now()}
	r.observed.SetArrivalTiming(true)
	r.observed.ClockOffset = measureRobotClock(life, pb.NewWendyTimeSyncServiceClient(conn.Conn))
	stream, err := r.client.Stream(life, &pb.UnitreeStreamRequest{Serial: serial, Topics: []string{sitenav.PoseTopic, sitenav.CloudTopic, sitenav.JointTopic}})
	if err != nil {
		cancel()
		return nil, err
	}
	// Video has its own subscription so decoding/backpressure cannot delay
	// the observations used by the motion watchdog. Both end with this session.
	r.videoFrames = make(chan []byte, 2)
	go r.receiveVideo()
	go func() {
		defer cancel()
		for {
			m, err := stream.Recv()
			if err != nil {
				r.mu.Lock()
				r.message = "Go2 observation stream disconnected."
				r.mu.Unlock()
				return
			}
			t := m.GetTopic()
			if t == nil {
				continue
			}
			r.mu.Lock()
			r.receiveObservationLocked(t.Topic, t.Data, t.Binary, time.Unix(0, t.ReceivedUnixNanos), time.Now())
			r.mu.Unlock()
		}
	}()
	go r.run()
	return r, nil
}

// Caller holds mu. Packet rejection must not erase continuity for the next pose.
func (r *SiteRobot) receiveObservationLocked(topic string, raw []byte, binary bool, received, now time.Time) {
	received = received.Add(-r.observed.ClockOffset)
	err := r.observed.Receive(topic, raw, binary, received, now)
	if err == nil && topic == sitenav.PoseTopic {
		r.poseHistory = append(r.poseHistory, r.observed.Pose)
		if len(r.poseHistory) > 128 {
			r.poseHistory = r.poseHistory[len(r.poseHistory)-128:]
		}
	}
	if err == nil && topic == sitenav.CloudTopic && r.moving {
		if navErr := r.observed.Ready(now); navErr != nil {
			r.message = navErr.Error()
			r.stopLocked()
		}
	}
	if err == nil && topic == sitenav.CloudTopic && !r.moving && len(r.preview) == 0 && !r.stopUnconfirmed {
		r.message = "Live LiDAR and odometry."
	}
	if err != nil && topic != sitenav.JointTopic {
		if !errors.Is(err, sitenav.ErrDuplicatePose) {
			r.message = err.Error()
		}
		r.recordObservationFault(topic, err, received, now)
		if topic == sitenav.PoseTopic && !sitenav.TransientPoseError(err) {
			r.poseEpoch++
			r.poseHistory = nil
			r.fusionTracker = sitenav.PoseTracker{}
			r.localizations = nil
			r.frame = ""
			r.observed.Pose = sitenav.Pose{}
		}
		if r.moving {
			r.stopLocked()
		}
	}
}

func (r *SiteRobot) receiveVideo() {
	defer close(r.videoFrames)
	stream, err := r.client.Stream(r.ctx, &pb.UnitreeStreamRequest{Serial: r.serial, Video: true})
	if err != nil {
		return
	}
	for {
		message, err := stream.Recv()
		if err != nil {
			return
		}
		frame := message.GetVideo().GetAnnexb()
		if len(frame) == 0 || len(frame) > 8<<20 {
			continue
		}
		select {
		case r.videoFrames <- frame:
		default:
			select {
			case <-r.videoFrames:
			default:
			}
			select {
			case r.videoFrames <- frame:
			default:
			}
		}
	}
}

// Camera reads never acquire the navigation lock or issue robot commands.
func (r *SiteRobot) NextVideo() ([]byte, error) {
	if r.ctx.Err() != nil {
		return nil, errors.New("Go2 camera is disconnected.")
	}
	select {
	case <-r.ctx.Done():
		return nil, errors.New("Go2 camera is disconnected.")
	case frame, ok := <-r.videoFrames:
		if !ok {
			return nil, errors.New("Go2 camera stream ended. Reconnect Go2 to reopen it.")
		}
		return frame, nil
	case <-time.After(250 * time.Millisecond):
		return nil, nil
	}
}

func (r *SiteRobot) run() {
	defer close(r.done)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			r.mu.Lock()
			r.closed = true
			if r.moving {
				r.stopLocked()
			}
			r.mu.Unlock()
			return
		case now := <-ticker.C:
			r.mu.Lock()
			if r.moving {
				r.stepLocked(now)
			}
			r.mu.Unlock()
		}
	}
}
func (r *SiteRobot) command(command pb.UnitreeCommand, forward, turn float64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_, err := r.client.Command(ctx, &pb.UnitreeCommandRequest{Serial: r.serial, Command: command, Forward: forward, Yaw: turn})
	return err
}
func (r *SiteRobot) stopLocked() error {
	r.moving = false
	if r.lookTurn != nil && r.lookState != 2 {
		r.lookState = 3
	}
	r.lookTurn = nil
	r.departure = nil
	r.route = nil
	r.preview = nil
	if err := r.command(pb.UnitreeCommand_UNITREE_COMMAND_STOP, 0, 0); err != nil {
		r.message = "Stop was not acknowledged. Use the Go2 controller to verify it has stopped."
		r.stopUnconfirmed = true
		return errors.New(r.message)
	}
	r.stopUnconfirmed = false
	return nil
}
func (r *SiteRobot) stepLocked(now time.Time) {
	var fault error
	switch {
	case r.ctx.Err() != nil || r.closed:
		fault = errors.New("Robot connection closed.")
	case now.Sub(r.heartbeat) > 600*time.Millisecond:
		fault = errors.New("Movement stopped because the map is no longer active.")
	case now.Sub(r.started) > 30*time.Second:
		fault = errors.New("The 30-second movement limit was reached.")
	case r.frame == "" && r.lookTurn == nil:
		fault = errors.New("Align Go2 with this site again.")
	default:
		fault = r.observed.Ready(now)
	}
	if fault != nil {
		r.message = fault.Error()
		r.stopLocked()
		return
	}
	if r.lookTurn != nil {
		r.stepLookLocked(now)
		return
	}
	p := r.observed.Pose.Position
	if p.Distance(r.alignment.Origin) > sitenav.MaxDistance {
		r.message = "Align Go2 again before moving farther."
		r.stopLocked()
		return
	}
	if p.Distance(r.goal) < .08 {
		r.message = "Arrived"
		r.stopLocked()
		return
	}
	for len(r.route) > 1 && p.Distance(r.route[0]) < .04 {
		r.route = r.route[1:]
	}
	if len(r.route) == 0 {
		r.message = "Route unavailable."
		r.stopLocked()
		return
	}
	grid := r.observed.Grid.WithDeparture(r.departure)
	if !grid.Transition(p, r.observed.Pose.Yaw, r.route[0]) {
		// Stop before planning a new route around changed occupied cells.
		if err := r.command(pb.UnitreeCommand_UNITREE_COMMAND_STOP, 0, 0); err != nil {
			r.message = "Could not confirm Stop."
			r.stopLocked()
			return
		}
		path, err := grid.PlanPose(p, r.observed.Pose.Yaw, r.goal)
		if err != nil {
			r.message = err.Error()
			r.stopLocked()
			return
		}
		arrivalYaw := sitenav.Heading(path[len(path)-2], r.goal)
		if err := r.observed.Grid.PoseClearanceError(r.goal, arrivalYaw, "the destination"); err != nil {
			r.message = err.Error()
			r.stopLocked()
			return
		}
		r.route = path[1:]
		// Planning can outlast a sensor or UI lease. Stay stopped until the
		// next tick rechecks freshness before issuing another MOVE.
		return
	}
	forward, turn := sitenav.Velocity(p, r.observed.Pose.Yaw, r.route[0])
	// Check the stopping horizon as well as the full planned segment.
	if forward > 0 {
		// Cover the 500 ms command lease plus braking at 0.5 m/s² and
		// 5 cm tracking tolerance. Slow approaches need less forward space.
		horizon := forward*.5 + forward*forward + .05
		look := sitenav.Point{X: p.X + math.Cos(r.observed.Pose.Yaw)*horizon, Y: p.Y + math.Sin(r.observed.Pose.Yaw)*horizon}
		if !grid.Sweep(p, look, r.observed.Pose.Yaw) {
			r.message = "Obstacle or unobserved floor ahead."
			r.stopLocked()
			return
		}
	}
	if err := r.command(pb.UnitreeCommand_UNITREE_COMMAND_MOVE, forward, turn); err != nil {
		r.message = "Movement command failed."
		r.stopLocked()
	}
}

func (r *SiteRobot) Align(sequence uint64, frame string, x, z, floor, yaw float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sequence <= r.commandSequence {
		return errors.New("This robot order was superseded.")
	}
	r.commandSequence = sequence
	if r.moving || r.stopUnconfirmed {
		if err := r.stopLocked(); err != nil {
			return err
		}
	}
	p := sitenav.Point{X: x, Y: -z}
	if r.closed || r.ctx.Err() != nil || len(frame) == 0 || len(frame) > 200 || !p.Valid() || math.IsNaN(yaw) || math.IsInf(yaw, 0) || math.IsNaN(floor) || math.IsInf(floor, 0) || math.Abs(floor) > 1000 {
		return errors.New("Invalid site alignment.")
	}
	if !sitenav.Fresh(r.observed.Pose.Received, time.Now()) {
		return errors.New("Wait for a live Go2 pose before aligning.")
	}
	r.alignment = sitenav.Alignment{Origin: r.observed.Pose.Position, Site: p, Rotation: yaw - r.observed.Pose.Yaw}
	r.frame = frame
	r.floorY = floor
	r.preview = nil
	r.message = "Aligned to this site using robot odometry."
	return nil
}
func (r *SiteRobot) Destination(sequence uint64, frame string, x, z float64, move bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sequence <= r.commandSequence {
		return errors.New("This robot order was superseded.")
	}
	r.commandSequence = sequence
	if r.moving || r.stopUnconfirmed {
		if err := r.stopLocked(); err != nil {
			return err
		}
	}
	if r.closed || r.ctx.Err() != nil || frame == "" || r.frame != frame {
		return errors.New("Align Go2 with this site first.")
	}
	if time.Since(r.heartbeat) > 600*time.Millisecond {
		return errors.New("The map is no longer active.")
	}
	if err := r.observed.Ready(time.Now()); err != nil {
		return err
	}
	p := sitenav.Point{X: x, Y: -z}
	if !p.Valid() {
		return errors.New("Invalid destination.")
	}
	goal := r.alignment.ToRobot(p)
	r.preview = nil
	r.departure = nil
	if r.underBodyAllowance {
		r.departure = &sitenav.Departure{Position: r.observed.Pose.Position, Yaw: r.observed.Pose.Yaw}
	}
	path, err := r.observed.Grid.WithDeparture(r.departure).PlanPose(r.observed.Pose.Position, r.observed.Pose.Yaw, goal)
	if err != nil {
		r.departure = nil
		return err
	}
	arrivalYaw := r.observed.Pose.Yaw
	if len(path) > 1 && path[len(path)-2].Distance(goal) > 1e-9 {
		arrivalYaw = sitenav.Heading(path[len(path)-2], goal)
	}
	// The fixed departure allowance must never authorize hidden floor at a goal.
	if err := r.observed.Grid.PoseClearanceError(goal, arrivalYaw, "the destination"); err != nil {
		r.departure = nil
		return err
	}
	r.preview = path
	r.goal = goal
	r.message = "Route ready"
	if move {
		r.route = append([]sitenav.Point(nil), path[1:]...)
		r.moving = true
		r.started = time.Now()
		r.message = "Moving"
	}
	return nil
}
func (r *SiteRobot) Stop(sequence uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sequence <= r.commandSequence {
		return
	}
	r.commandSequence = sequence
	r.message = "Stopped"
	r.stopLocked()
}
func (r *SiteRobot) Close() { r.cancel(); <-r.done }

func (r *SiteRobot) SetUnderBodyAllowance(sequence uint64, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sequence <= r.commandSequence {
		return errors.New("This robot order was superseded.")
	}
	r.commandSequence = sequence
	if r.closed || r.ctx.Err() != nil {
		return errors.New("Robot connection closed.")
	}
	if r.moving || r.stopUnconfirmed {
		if err := r.stopLocked(); err != nil {
			return err
		}
	}
	r.underBodyAllowance = enabled
	r.preview = nil
	r.route = nil
	r.departure = nil
	r.localizations = nil
	r.message = "Departure allowance changed. Choose a destination to preview its route."
	return nil
}

func (r *SiteRobot) SetArrivalTiming(sequence uint64, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sequence <= r.commandSequence {
		return errors.New("This robot order was superseded.")
	}
	r.commandSequence = sequence
	if r.closed || r.ctx.Err() != nil {
		return errors.New("Robot connection closed.")
	}
	if r.moving || r.stopUnconfirmed {
		if err := r.stopLocked(); err != nil {
			return err
		}
	}
	r.preview = nil
	r.route = nil
	r.localizations = nil
	r.observed.SetArrivalTiming(enabled)
	r.fusionTracker = sitenav.PoseTracker{}
	r.message = "Timing mode changed. Waiting for a new scan."
	return nil
}

func (r *SiteRobot) Snapshot() SiteRobotSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.heartbeat = now
	s := SiteRobotSnapshot{Message: r.message, Pose: sitenav.Fresh(r.observed.Pose.Received, now) && !r.closed, Aligned: r.frame != "", Moving: r.moving, FloorY: r.floorY}
	s.LookState = r.lookState
	s.LookDirection = r.lookDirection
	s.ObservationRevision = r.observed.Surfaces.Fingerprint()
	s.ArrivalTiming = r.observed.AllowArrivalTime
	s.UsingArrivalTiming = r.observed.UsingArrivalTime
	s.TimingUnavailable = r.observed.TimingUnavailable
	s.UnderBodyAllowance = r.underBodyAllowance
	s.ScanReady = (r.observed.SurfaceReady(now) == nil || r.observed.LocalizationReady(now) == nil) && !r.closed && !r.stopUnconfirmed
	if err := r.observed.Ready(now); err == nil {
		s.Ready = s.Aligned && !r.closed && !r.stopUnconfirmed
		if s.Ready && !r.moving {
			var departure *sitenav.Departure
			if r.underBodyAllowance {
				departure = &sitenav.Departure{Position: r.observed.Pose.Position, Yaw: r.observed.Pose.Yaw}
			}
			if err := r.observed.Grid.WithDeparture(departure).PoseClearanceError(r.observed.Pose.Position, r.observed.Pose.Yaw, "Go2"); err != nil {
				s.Ready = false
				s.Message = err.Error()
			}
		}
	} else if !r.moving {
		s.Message = err.Error()
	}
	if !s.Aligned && s.Pose && !s.ScanReady && !r.moving {
		if err := r.observed.LocalizationReady(now); err != nil {
			s.Message = err.Error()
		}
	}
	if s.Pose && s.Aligned {
		p := r.alignment.ToSite(r.observed.Pose.Position)
		s.X = p.X
		s.Z = -p.Y
		s.Yaw = r.observed.Pose.Yaw + r.alignment.Rotation
	}
	if sitenav.Fresh(r.observed.JointsAt, now) {
		s.Joints = append([]float64(nil), r.observed.Joints...)
	}
	path := r.preview
	if r.moving {
		path = append([]sitenav.Point{r.observed.Pose.Position}, r.route...)
	}
	for _, p := range path {
		s.Route = append(s.Route, r.alignment.ToSite(p))
	}
	return s
}
