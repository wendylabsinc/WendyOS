package twin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A find mission looks for a named object with Go2's camera, walks to it and
// optionally barks. It reuses the supervised orders an operator has: Look and
// Destination. Every walk keeps their route checks, 30-second limit and the
// 600 ms operator lease, which only a rendering client's polls renew. The job
// reads Status and never renews that lease: closing the live view stops Go2.
type findRobot interface {
	findMotion
	findSenses
}

// findMotion is the operator's own supervised order set.
type findMotion interface {
	Status() twinrobot.SiteRobotSnapshot
	Stop(uint64)
	Destination(uint64, string, float64, float64, bool) error
	Look(uint64, int) error
}

// findSenses reads measurements and plays sound; it never moves Go2.
type findSenses interface {
	ExploreTarget([]sitenav.Point) (float64, float64, error)
	LocateInView(float64, float64) (float64, float64, error)
	Bark() error
}

type findSeer interface {
	locate(context.Context, []byte, string) ([4]float64, bool, error)
}

type findView struct {
	number uint64
	image  []byte
}

type findJob struct {
	root         uint64
	frame, query string
	bark         bool
	views        chan findView
	known        *RoomObject
	save         func(RoomObject) error
}

// Stop this far short of the object; retry farther back when that is blocked.
var findStandOffs = []float64{0.8, 1.3}

const findViewpointLimit = 6
const findTimeLimit = 10 * time.Minute
const findViewClosed = "Go2 stopped because the live view closed. Keep it open while Go2 searches."

// Test seams: production waits for the client's frame and for Go2 to settle.
var findViewTimeout = 20 * time.Second
var findSettle = 600 * time.Millisecond

// normalizeObject makes "The  Toolbox" and "toolbox" the same remembered thing.
func normalizeObject(query string) string {
	name := strings.Join(strings.Fields(strings.ToLower(query)), " ")
	for _, article := range []string{"the ", "a ", "an ", "my ", "our "} {
		name = strings.TrimPrefix(name, article)
	}
	if len(name) > 80 || strings.ContainsFunc(name, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return ""
	}
	return name
}

// standOff returns the point d metres short of the target on the line from
// the robot, or far=false when the robot is already about that close.
func standOff(fromX, fromZ, toX, toZ, d float64) (x, z float64, far bool) {
	distance := math.Hypot(toX-fromX, toZ-fromZ)
	if distance <= d+.2 {
		return 0, 0, false
	}
	return toX - (toX-fromX)/distance*d, toZ - (toZ-fromZ)/distance*d, true
}

func (s *Server) startFind(session *robotSession, q twinrobot.Request) error {
	robot, ok := session.robot.(findRobot)
	if !ok {
		return errors.New("this robot connection cannot search")
	}
	query := normalizeObject(q.Query)
	if query == "" {
		return errors.New("Say what Go2 should look for, in a few words.")
	}
	room, err := s.Store.Frame(q.Frame)
	if err != nil {
		return err
	}
	if state := robot.Status(); !state.Ready {
		if !state.Aligned {
			return errors.New("Go2's position in this site is not confirmed yet.")
		}
		return errors.New("Go2 is not ready to move: " + state.Message)
	}
	setup, done := context.WithTimeout(s.Context, 8*time.Second)
	vision, err := selectPreferredVisionModel(setup, &http.Client{Timeout: 100 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, q.ModelPort, q.Model)
	done()
	if err != nil {
		return err
	}
	job := findJob{root: q.Sequence, frame: q.Frame, query: query, bark: q.Bark, views: make(chan findView, 1),
		save: func(object RoomObject) error { return s.Store.SaveObject(q.Frame, query, object) }}
	if known, ok := room.Objects[query]; ok {
		job.known = &known
	}
	ctx, cancel := context.WithTimeout(s.Context, findTimeLimit)
	session.findMu.Lock()
	if q.Sequence <= session.findRoot {
		session.findMu.Unlock()
		cancel()
		return errors.New("search superseded")
	}
	session.findCancel = cancel
	session.findRoot = q.Sequence
	session.findViews = job.views
	session.find = twinrobot.FindStatus{State: "searching", Phase: "looking", Message: "Starting the search", Query: query}
	session.findMu.Unlock()
	go s.find(ctx, cancel, session, robot, vision, job)
	return nil
}

// acceptFindView hands the job the camera frame it asked for. Late or
// duplicate frames are refused instead of being matched to a later heading.
func (session *robotSession) acceptFindView(number uint64, image []byte) error {
	session.findMu.Lock()
	defer session.findMu.Unlock()
	if session.find.State != "searching" || number == 0 || session.find.WantsView != number {
		return errors.New("this camera frame is no longer needed")
	}
	if len(image) < 4 || len(image) > 4<<20 || image[0] != 0xff || image[1] != 0xd8 {
		return errors.New("send one JPEG camera frame")
	}
	select {
	case session.findViews <- findView{number, image}:
	default:
	}
	return nil
}

func (s *Server) find(ctx context.Context, cancel context.CancelFunc, session *robotSession, robot findRobot, vision findSeer, job findJob) {
	defer cancel()
	sequence := job.root
	update := func(change func(*twinrobot.FindStatus)) {
		session.findMu.Lock()
		defer session.findMu.Unlock()
		if session.findRoot == job.root {
			change(&session.find)
		}
	}
	finish := func(state, message string) {
		log.Printf("find %q %s: %s", job.query, state, message)
		update(func(f *twinrobot.FindStatus) { f.State, f.Phase, f.Message, f.WantsView = state, "", message, 0 })
	}
	// A newer operator order has a higher sequence, so this Stop cannot undo it.
	fail := func(message string) {
		robot.Stop(job.root + 999999)
		if message == twinrobot.MovementLeaseLostMessage {
			message = findViewClosed
		}
		finish("failed", message)
	}
	// refused reports an order the robot engine rejected outright.
	refused := func(err error) {
		if errors.Is(err, twinrobot.ErrOperatorLease) {
			fail(findViewClosed)
		} else {
			fail(err.Error())
		}
	}
	stopped := func() {
		robot.Stop(job.root + 999999)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			finish("failed", "The search reached its ten-minute limit.")
		} else {
			finish("stopped", "Search stopped.")
		}
	}
	pause := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	// settle waits for the current order to end and reports how it ended.
	settle := func() (twinrobot.SiteRobotSnapshot, bool) {
		for {
			if !pause(100 * time.Millisecond) {
				return twinrobot.SiteRobotSnapshot{}, false
			}
			if state := robot.Status(); state.Disconnected || !state.Moving {
				return state, true
			}
		}
	}
	// lost reports faults that end the mission instead of the current step. A
	// control warning is not one: it also reports a harmless missing avoidance
	// switch, and an unconfirmed Stop makes the next order fail by itself.
	lost := func(state twinrobot.SiteRobotSnapshot) bool {
		return state.Disconnected || !state.Aligned || state.Message == twinrobot.MovementLeaseLostMessage
	}
	// walk orders one supervised route. started is false when no route exists.
	walk := func(x, z float64) (started, arrived bool, reason string, alive bool) {
		sequence++
		if err := robot.Destination(sequence, job.frame, x, z, true); err != nil {
			if errors.Is(err, twinrobot.ErrOperatorLease) {
				refused(err)
				return false, false, "", false
			}
			return false, false, err.Error(), true
		}
		state, ok := settle()
		if !ok {
			stopped()
			return true, false, "", false
		}
		if lost(state) {
			fail(state.Message)
			return true, false, "", false
		}
		// The engine's "Arrived" message lasts only until the next LiDAR
		// packet, so judge arrival from where Go2 actually stopped.
		short := math.Hypot(state.X-x, state.Z-z)
		return true, short < .3, fmt.Sprintf("it stopped %.1f m short", short), true
	}
	var views uint64
	// look examines one stationary camera frame. box is nil when nothing matched.
	look := func() (box *[4]float64, number uint64, alive bool) {
		if !pause(findSettle) {
			stopped()
			return nil, 0, false
		}
		views++
		number = views
		update(func(f *twinrobot.FindStatus) {
			f.WantsView, f.Phase, f.Message = number, "looking", "Looking for the "+job.query
		})
		var image []byte
		timeout := time.After(findViewTimeout)
		for image == nil {
			select {
			case <-ctx.Done():
				stopped()
				return nil, 0, false
			case <-timeout:
				fail("The live view stopped sending Go2 camera frames.")
				return nil, 0, false
			case view := <-job.views:
				if view.number == number {
					image = view.image
				}
			}
		}
		update(func(f *twinrobot.FindStatus) {
			f.WantsView, f.Phase, f.Message, f.Views = 0, "thinking", "Checking what the camera sees", int(number)
		})
		thinking, done := context.WithTimeout(ctx, 90*time.Second)
		found, seen, err := vision.locate(thinking, image, job.query)
		done()
		if ctx.Err() != nil {
			stopped()
			return nil, 0, false
		}
		if err != nil {
			fail("The vision model could not check the camera view: " + err.Error())
			return nil, 0, false
		}
		if !seen {
			return nil, number, true
		}
		return &found, number, true
	}
	// conclude walks up to a sighting, remembers it and barks.
	conclude := func(box [4]float64, number uint64) {
		update(func(f *twinrobot.FindStatus) {
			f.Seen, f.Box, f.BoxView, f.Phase, f.Message = true, box, number, "approaching", "Spotted the "+job.query
		})
		note := ""
		x, z, err := robot.LocateInView(box[0]+box[2]/2, box[1]+box[3])
		if err != nil {
			note = " Go2 can see it from here but could not measure how far away it is."
			if !errors.Is(err, twinrobot.ErrNoViewRange) {
				note = " Go2 could not place it in the room: " + err.Error()
			}
		} else {
			update(func(f *twinrobot.FindStatus) {
				f.Located, f.X, f.Z, f.Message = true, x, z, "Spotted the "+job.query+". Walking over."
			})
			if err := job.save(RoomObject{X: x, Z: z, SeenUnix: time.Now().Unix()}); err != nil {
				log.Printf("find %q: location not saved: %v", job.query, err)
			}
			state := robot.Status()
			for _, distance := range findStandOffs {
				gx, gz, far := standOff(state.X, state.Z, x, z, distance)
				if !far {
					note = ""
					break
				}
				started, arrived, reason, alive := walk(gx, gz)
				if !alive {
					return
				}
				if arrived {
					note = ""
					break
				}
				if started {
					note = " Go2 walked toward it, but " + reason + "."
					break
				}
				note = " Go2 could not walk closer: " + reason
			}
		}
		if job.bark {
			update(func(f *twinrobot.FindStatus) { f.Phase = "barking" })
			err := robot.Bark()
			update(func(f *twinrobot.FindStatus) {
				f.Barked = err == nil
				if status.Code(err) == codes.InvalidArgument || status.Code(err) == codes.Unimplemented {
					f.BarkFailure = "This device's Wendy Agent cannot play sounds on Go2 yet. Update the agent."
				} else if err != nil {
					f.BarkFailure = status.Convert(err).Message()
				}
			})
		}
		finish("found", "Found the "+job.query+"."+note)
	}

	if job.known != nil {
		update(func(f *twinrobot.FindStatus) {
			f.Remembered, f.Phase, f.Message = true, "recalling", "Heading to where the "+job.query+" was last seen"
		})
		state := robot.Status()
		if x, z, far := standOff(state.X, state.Z, job.known.X, job.known.Z, findStandOffs[0]); far {
			if _, _, _, alive := walk(x, z); !alive {
				return
			}
		}
	}
	var visited []sitenav.Point // site X, -Z, as routes are reported
	for viewpoint := 1; ; viewpoint++ {
		state := robot.Status()
		if lost(state) {
			fail(state.Message)
			return
		}
		visited = append(visited, sitenav.Point{X: state.X, Y: -state.Z})
		update(func(f *twinrobot.FindStatus) { f.Viewpoints = viewpoint })
		direction := 0
		for turn := 0; turn < twinrobot.LocalizationTurnCount; turn++ {
			box, number, alive := look()
			if !alive {
				return
			}
			if box != nil {
				conclude(*box, number)
				return
			}
			if turn == twinrobot.LocalizationTurnCount-1 {
				break
			}
			update(func(f *twinrobot.FindStatus) { f.Phase, f.Message = "turning", "Turning to look around" })
			sequence++
			err := robot.Look(sequence, direction)
			if errors.Is(err, twinrobot.ErrDiscoveryBlocked) || errors.Is(err, twinrobot.ErrObservationUnavailable) {
				break // no room to turn here; look from somewhere else
			}
			if err != nil {
				refused(err)
				return
			}
			state, ok := settle()
			if !ok {
				stopped()
				return
			}
			if state.LookState != 2 {
				if lost(state) {
					fail(state.Message)
					return
				}
				break
			}
			if direction == 0 {
				direction = state.LookDirection
			}
		}
		reason := "It reached its limit of six places."
		moved := false
		for attempt := 0; viewpoint < findViewpointLimit && attempt < 3 && !moved; attempt++ {
			x, z, err := robot.ExploreTarget(visited)
			if err != nil {
				reason = err.Error()
				break
			}
			update(func(f *twinrobot.FindStatus) {
				f.Phase, f.Message = "moving", "Walking to another spot to keep looking"
			})
			started, _, why, alive := walk(x, z)
			if !alive {
				return
			}
			// Reached or refused, this spot is not chosen again.
			visited = append(visited, sitenav.Point{X: x, Y: -z})
			moved, reason = started, why
		}
		if !moved {
			robot.Stop(job.root + 999999)
			places := "places"
			if viewpoint == 1 {
				places = "place"
			}
			finish("not-found", fmt.Sprintf("Go2 looked around from %d %s and did not see the %s. %s", viewpoint, places, job.query, reason))
			return
		}
	}
}
