package twin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
)

const activeObjectLimit = 5 * time.Minute
const activeObjectViewWait = 25 * time.Second

type activeObjectOwner interface {
	Status() twinrobot.SiteRobotSnapshot
	Check() error
	Close()
	ObjectSearchLook(uint64, int) error
	ObjectSearchStep(uint64) error
	SearchCameraView(int64, time.Time) (*twinrobot.SearchCameraView, error)
	CheckSearchCameraView(*twinrobot.SearchCameraView) error
	FinishSearchCameraView(*twinrobot.SearchCameraView) error
}
type activeObjectVision interface {
	locate(context.Context, []byte, string) ([4]float64, bool, error)
}
type activeObjectOwnerFactory func(context.Context, robotControl) (activeObjectOwner, error)
type activeObjectFrame struct {
	image []byte
	view  *twinrobot.SearchCameraView
}
type activeObjectJob struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	owner    activeObjectOwner
	vision   activeObjectVision
	request  robotCommandRequest
	progress twinrobot.ObjectSearchProgress
	result   twinrobot.CommandResult
	demandAt time.Time
	accepted map[string]bool
	frames   chan activeObjectFrame
}

func (session *robotSession) cancelActiveObject() {
	session.activeObjectMu.Lock()
	job := session.activeObject
	session.activeObjectMu.Unlock()
	if job != nil {
		job.cancel()
	}
}
func beginActiveObjectOwner(ctx context.Context, robot robotControl) (activeObjectOwner, error) {
	concrete, ok := robot.(interface {
		BeginLocalizationSearch(context.Context) (*twinrobot.LocalizationSearchOwner, error)
	})
	if !ok {
		return nil, errors.New("controller needs the bounded object-search update")
	}
	owner, err := concrete.BeginLocalizationSearch(ctx)
	if err != nil {
		return nil, err
	}
	return owner, nil
}
func (s *Server) prepareActiveObjectSearch(ctx context.Context, session *robotSession, q robotCommandRequest) (twinrobot.CommandResult, func()) {
	client := &http.Client{Timeout: 95 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("local model redirect refused") }}
	setup, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	vision, err := selectVisionModel(setup, client, 11434, q.Model)
	if err != nil {
		return twinrobot.CommandResult{State: "rejected", Message: err.Error(), Snapshot: session.robot.Status()}, nil
	}
	return s.prepareActiveObjectSearchUsing(ctx, session, q, vision, beginActiveObjectOwner)
}
func (s *Server) prepareActiveObjectSearchUsing(requestContext context.Context, session *robotSession, q robotCommandRequest, vision activeObjectVision, begin activeObjectOwnerFactory) (twinrobot.CommandResult, func()) {
	reject := func(err error) (twinrobot.CommandResult, func()) {
		return twinrobot.CommandResult{State: "rejected", Message: err.Error(), Snapshot: session.robot.Status()}, nil
	}
	if requestContext.Err() != nil {
		return reject(requestContext.Err())
	}
	if s.Context == nil || s.Context.Err() != nil {
		return reject(errors.New("robot gateway is unavailable"))
	}
	session.commandMu.Lock()
	pending := false
	for id, receipt := range session.commandReceipts {
		if id != q.RequestID && receipt.result.State == "running" {
			pending = true
		}
	}
	session.commandMu.Unlock()
	if pending {
		return reject(errors.New("another robot command is pending; resolve or stop it before searching"))
	}
	session.activeObjectMu.Lock()
	prior := session.activeObject
	session.activeObjectMu.Unlock()
	if prior != nil {
		prior.mu.Lock()
		active := prior.result.State == "running"
		prior.mu.Unlock()
		if active {
			return reject(errors.New("an object search is already active"))
		}
	}
	life, cancel := context.WithTimeout(s.Context, activeObjectLimit)
	stopRequestCancel := context.AfterFunc(requestContext, cancel)
	owner, err := begin(life, session.robot)
	if err != nil {
		stopRequestCancel()
		cancel()
		return reject(err)
	}
	job := &activeObjectJob{ctx: life, cancel: cancel, owner: owner, vision: vision, request: q, progress: twinrobot.ObjectSearchProgress{Query: strings.TrimSpace(q.Query), Viewpoints: 1}, accepted: map[string]bool{}, frames: make(chan activeObjectFrame, 1)}
	job.result = twinrobot.CommandResult{State: "running", Message: "Starting bounded camera search using measured local floor. No saved room or alignment is required; finding an object will not approach it.", Snapshot: session.robot.Status()}
	progress := job.progress
	job.result.ObjectSearch = &progress
	session.activeObjectMu.Lock()
	session.activeObject = job
	session.activeObjectMu.Unlock()
	launch := func() {
		if !stopRequestCancel() || requestContext.Err() != nil {
			cancel()
			owner.Close()
			s.finishActiveObject(session, job, "canceled", "Object search canceled before startup.")
			return
		}
		go s.runActiveObject(session, job)
	}
	return job.result, launch
}
func (s *Server) objectProgress(session *robotSession, job *activeObjectJob, message string) {
	snapshot := session.robot.Status()
	job.mu.Lock()
	progress := job.progress
	result := twinrobot.CommandResult{State: "running", Message: message, Snapshot: snapshot, ObjectSearch: &progress}
	job.result = result
	updateActiveReceipt(session, job.request, result)
	job.mu.Unlock()
}
func (s *Server) finishActiveObject(session *robotSession, job *activeObjectJob, state, message string) {
	job.owner.Close()
	snapshot := session.robot.Status()
	job.mu.Lock()
	job.progress.ViewID = ""
	progress := job.progress
	result := twinrobot.CommandResult{State: state, Message: message, Snapshot: snapshot, ObjectSearch: &progress}
	job.result = result
	updateActiveReceipt(session, job.request, result)
	job.mu.Unlock()
	job.cancel()
}
func activeObjectError(job *activeObjectJob, err error) (string, string) {
	if errors.Is(err, twinrobot.ErrLocalizationSearchSuperseded) {
		return "superseded", "Object search was superseded by another robot command."
	}
	if errors.Is(job.ctx.Err(), context.DeadlineExceeded) {
		return "failed", "Object search reached its five-minute limit without finding the object."
	}
	if job.ctx.Err() != nil {
		return "canceled", "Object search was canceled."
	}
	return "failed", err.Error()
}
func (s *Server) runActiveObject(session *robotSession, job *activeObjectJob) {
	defer job.cancel()
	fail := func(err error) {
		state, message := activeObjectError(job, err)
		s.finishActiveObject(session, job, state, message)
	}
	var sequence uint64
	moves, turns, direction := 0, 0, 0
	for {
		if err := job.owner.Check(); err != nil {
			fail(err)
			return
		}
		// Take each picture only at rest, after the previous movement settled.
		select {
		case <-job.ctx.Done():
			fail(job.ctx.Err())
			return
		case <-time.After(600 * time.Millisecond):
		}
		token := uuid.NewString()
		job.mu.Lock()
		job.progress.ViewID = token
		job.demandAt = time.Now()
		job.mu.Unlock()
		s.objectProgress(session, job, "Waiting for a fresh Go2 camera view to find "+job.progress.Query+".")
		timer := time.NewTimer(activeObjectViewWait)
		poll := time.NewTicker(100 * time.Millisecond)
		var frame activeObjectFrame
	frameWait:
		for {
			select {
			case <-job.ctx.Done():
				timer.Stop()
				poll.Stop()
				fail(job.ctx.Err())
				return
			case <-timer.C:
				poll.Stop()
				fail(errors.New("Camera frame did not arrive within 25 seconds; search stopped without another movement."))
				return
			case <-poll.C:
				if err := job.owner.Check(); err != nil {
					timer.Stop()
					poll.Stop()
					fail(err)
					return
				}
			case frame = <-job.frames:
				break frameWait
			}
		}
		timer.Stop()
		poll.Stop()
		job.mu.Lock()
		job.progress.Views++
		job.mu.Unlock()
		s.objectProgress(session, job, "Checking the Go2 camera with the local vision model.")
		thinking, done := context.WithTimeout(job.ctx, 90*time.Second)
		box, seen, err := job.vision.locate(thinking, frame.image, job.progress.Query)
		done()
		if job.ctx.Err() != nil {
			fail(job.ctx.Err())
			return
		}
		if ownerErr := job.owner.Check(); ownerErr != nil {
			fail(ownerErr)
			return
		}
		if err != nil {
			fail(fmt.Errorf("Local vision could not inspect the camera: %w", err))
			return
		}
		if err = job.owner.CheckSearchCameraView(frame.view); err != nil {
			fail(err)
			return
		}
		if seen {
			if err := job.owner.FinishSearchCameraView(frame.view); err != nil {
				fail(err)
				return
			}
			job.mu.Lock()
			job.progress.Seen = true
			job.progress.Box = box
			job.mu.Unlock()
			s.finishActiveObject(session, job, "completed", "Found "+job.progress.Query+" in the Go2 camera. Search stopped. Its distance and a navigable approach have not been established; no approach was sent.")
			return
		}
		if moves >= 3 && turns >= 6 {
			fail(errors.New("Object was not seen within the bounded search: three nearby viewpoint moves and six turns. Go2 stopped."))
			return
		}
		// Try a separated viewpoint on measured floor first. A blocked preflight
		// may use a safe look turn instead; an executed movement fault ends the job.
		discovery := false
		var blocked error
		if moves < 3 {
			sequence++
			err = job.owner.ObjectSearchStep(sequence)
			if err == nil {
				moves++
				discovery = true
			} else if errors.Is(err, twinrobot.ErrDiscoveryBlocked) {
				blocked = err
			} else {
				fail(err)
				return
			}
		}
		if !discovery {
			if turns >= 6 {
				fail(fmt.Errorf("Object was not seen; no further measured viewpoint is available: %v", blocked))
				return
			}
			sequence++
			err = job.owner.ObjectSearchLook(sequence, direction)
			if err != nil {
				fail(fmt.Errorf("Object was not seen and no safe next view is available: %w", err))
				return
			}
			turns++
		}
		job.mu.Lock()
		job.progress.Turns = turns
		job.mu.Unlock()
		message := "Turning to inspect another camera view."
		if discovery {
			message = "Moving to a nearby viewpoint on observed clear floor."
		}
		s.objectProgress(session, job, message)
		tick := time.NewTicker(100 * time.Millisecond)
	movement:
		for {
			select {
			case <-job.ctx.Done():
				tick.Stop()
				fail(job.ctx.Err())
				return
			case <-tick.C:
				if err := job.owner.Check(); err != nil {
					tick.Stop()
					fail(err)
					return
				}
				state := job.owner.Status()
				if state.Disconnected {
					tick.Stop()
					fail(errors.New(state.Message))
					return
				}
				terminal := state.LookState
				if discovery {
					terminal = state.DiscoveryState
				}
				if terminal == 3 {
					tick.Stop()
					fail(errors.New(searchMotionStopMessage(state)))
					return
				}
				if terminal == 2 && !state.Moving {
					if discovery {
						job.mu.Lock()
						job.progress.Viewpoints++
						job.mu.Unlock()
					} else if direction == 0 {
						direction = state.LookDirection
					}
					break movement
				}
			}
		}
		tick.Stop()
	}
}

type activeObjectViewRequest struct {
	Serial     string `json:"serial"`
	RequestID  string `json:"request_id"`
	ViewID     string `json:"view_id"`
	Image      []byte `json:"image"`
	Received   int64  `json:"gateway_received_unix_nanos"`
	CameraMode string `json:"camera_mode"`
}

func (s *Server) activeObjectView(w http.ResponseWriter, r *http.Request) {
	var q activeObjectViewRequest
	if !decode(w, r, &q, 3<<20) {
		return
	}
	s.mu.Lock()
	session := s.observers[q.Serial]
	s.mu.Unlock()
	if session == nil {
		respond(w, nil, errors.New("object search session is unavailable"))
		return
	}
	session.activeObjectMu.Lock()
	job := session.activeObject
	session.activeObjectMu.Unlock()
	if job == nil || job.request.RequestID != q.RequestID || q.ViewID == "" {
		respond(w, nil, errors.New("camera view does not belong to this search"))
		return
	}
	job.mu.Lock()
	if job.accepted[q.ViewID] {
		result := job.result
		job.mu.Unlock()
		respond(w, result, nil)
		return
	}
	valid := job.result.State == "running" && job.progress.ViewID == q.ViewID
	after := job.demandAt
	job.mu.Unlock()
	if !valid {
		respond(w, nil, errors.New("camera view is no longer requested"))
		return
	}
	if q.CameraMode != "go2-front-native-1280x720" || len(q.Image) == 0 || len(q.Image) > 2<<20 {
		respond(w, nil, errors.New("send a bounded native Go2 camera JPEG"))
		return
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(q.Image))
	if err != nil || cfg.Width != 1280 || cfg.Height != 720 {
		respond(w, nil, errors.New("object search requires a native 1280x720 Go2 JPEG"))
		return
	}
	if _, err = jpeg.Decode(bytes.NewReader(q.Image)); err != nil {
		respond(w, nil, errors.New("invalid Go2 camera JPEG"))
		return
	}
	view, err := job.owner.SearchCameraView(q.Received, after)
	if err != nil {
		respond(w, nil, err)
		return
	}
	job.mu.Lock()
	if job.ctx.Err() != nil || job.result.State != "running" || job.progress.ViewID != q.ViewID {
		job.mu.Unlock()
		respond(w, nil, errors.New("camera demand expired during validation"))
		return
	}
	job.accepted[q.ViewID] = true
	job.progress.ViewID = ""
	progress := job.progress
	job.result.ObjectSearch = &progress
	job.result.Message = "Camera frame accepted; checking it with the local vision model."
	updateActiveReceipt(session, job.request, job.result)
	select {
	case job.frames <- activeObjectFrame{image: q.Image, view: view}:
	default:
		job.mu.Unlock()
		respond(w, nil, errors.New("camera view is already queued"))
		return
	}
	result := job.result
	job.mu.Unlock()
	respond(w, result, nil)
}
