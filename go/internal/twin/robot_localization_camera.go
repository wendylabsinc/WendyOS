package twin

import (
	"bytes"
	"context"
	"errors"
	"image/color"
	"image/jpeg"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	"github.com/wendylabsinc/wendy/go/internal/twinvision"
)

type localizationCameraOwner interface {
	activeSearchOwner
	SearchCameraView(int64, time.Time) (*twinrobot.SearchCameraView, error)
	CheckSearchCameraView(*twinrobot.SearchCameraView) error
}
type localizationCameraFrame struct {
	frame    twinvision.Frame
	view     *twinrobot.SearchCameraView
	captured time.Time
}
type localizationCameraJob struct {
	mu       sync.Mutex
	ctx      context.Context
	request  robotCommandRequest
	owner    localizationCameraOwner
	progress twinrobot.LocalizationCameraProgress
	demandAt time.Time
	accepted map[string]bool
	frames   chan localizationCameraFrame
}

func localizationCameraProgress(session *robotSession, requestID string) *twinrobot.LocalizationCameraProgress {
	session.searchMu.Lock()
	job := session.localizationCamera
	session.searchMu.Unlock()
	if job == nil || job.request.RequestID != requestID {
		return nil
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	copy := job.progress
	return &copy
}
func (s *Server) cameraLocalizationProgress(session *robotSession, job *localizationCameraJob, state, message string) {
	job.mu.Lock()
	job.progress.State, job.progress.Message = state, message
	if state != "waiting" {
		job.progress.ViewID = ""
	}
	copy := job.progress
	job.mu.Unlock()
	session.searchMu.Lock()
	if session.localizationCamera == job {
		session.searchMessage = message
		session.searchPhase = "matching"
	}
	session.searchMu.Unlock()
	updateActiveReceipt(session, job.request, twinrobot.CommandResult{State: "running", Message: message, Snapshot: session.robot.Status(), LocalizationCamera: &copy})
}

// Match fresh camera frames before the shared turn-and-match search. The camera
// result only supports the LiDAR matcher; it never confirms a room on its own.
func (s *Server) initialLocalizationCamera(ctx context.Context, session *robotSession, q robotCommandRequest, frame string, owner activeSearchOwner) *sitenav.VisualHint {
	cameraOwner, ok := owner.(localizationCameraOwner)
	if !ok {
		return nil
	}
	job := &localizationCameraJob{ctx: ctx, request: q, owner: cameraOwner, accepted: map[string]bool{}, frames: make(chan localizationCameraFrame, 1)}
	session.searchMu.Lock()
	session.localizationCamera = job
	session.searchMu.Unlock()
	unavailable := func(err error) *sitenav.VisualHint {
		s.cameraLocalizationProgress(session, job, "unavailable", "Camera support unavailable: "+err.Error()+" Continuing with LiDAR geometry.")
		return nil
	}
	room, err := s.Store.Frame(frame)
	if err != nil {
		return unavailable(err)
	}
	room, err = s.Store.Get(room.Site.ID)
	if err != nil {
		return unavailable(err)
	}
	if room.Site.ID+"/"+room.Site.ModelRevision != frame {
		return unavailable(ErrConflict)
	}
	// Visual landmarks and their coordinates must belong to the exact saved model.
	landmarks, err := twinvision.Landmarks(room.VisualMap, room.Model, room.Site.ModelRevision)
	if err != nil {
		return unavailable(err)
	}
	first, err := s.awaitLocalizationCamera(session, job, "Waiting for a fresh Go2 view to match the room.")
	if err != nil {
		return unavailable(err)
	}
	// Same native Go2 community preset as the macOS UI, explicitly estimated.
	lens := [9]float64{864.39938, 863.73849, 639.19798, 373.28118, -.354630, .102054, -.001614, -.001249, 0}
	a, err := s.solveLocalizationCamera(session, job, landmarks, first, lens, 2, false)
	if err != nil {
		return unavailable(err)
	}
	second, err := s.awaitLocalizationCamera(session, job, "Waiting for a second fresh Go2 view to verify the room match.")
	if err != nil {
		return unavailable(err)
	}
	if !second.captured.After(first.captured.Add(500 * time.Millisecond)) {
		return unavailable(errors.New("camera verification frames are too close together"))
	}
	if err = cameraOwner.CheckSearchCameraView(first.view); err != nil {
		return unavailable(err)
	}
	provenance := int32(1)
	if a.FittedLens == 2 {
		provenance = 2
	}
	b, err := s.solveLocalizationCamera(session, job, landmarks, second, a.Lens(), provenance, true)
	if err != nil {
		return unavailable(err)
	}
	verified, err := twinvision.Verify(a, b, 1280, 720)
	if err != nil {
		return unavailable(err)
	}
	// Match the desktop's stricter estimated-lens agreement threshold.
	d2, trace := 0., 0.
	for i := 0; i < 3; i++ {
		d := a.Transform[12+i] - b.Transform[12+i]
		d2 += d * d
		for j := 0; j < 3; j++ {
			trace += a.Transform[4*i+j] * b.Transform[4*i+j]
		}
	}
	if math.Sqrt(d2) > .25 || math.Acos(math.Max(-1, math.Min(1, (trace-1)/2))) > 8*math.Pi/180 {
		return unavailable(errors.New("camera position did not agree between fresh frames"))
	}
	if err = cameraOwner.CheckSearchCameraView(first.view); err != nil {
		return unavailable(err)
	}
	if err = cameraOwner.CheckSearchCameraView(second.view); err != nil {
		return unavailable(err)
	}
	if ctx.Err() != nil {
		return unavailable(ctx.Err())
	}
	current, err := s.Store.Get(room.Site.ID)
	if err != nil {
		return unavailable(err)
	}
	if current.Site.ModelRevision != room.Site.ModelRevision || !bytes.Equal(current.Model, room.Model) || !bytes.Equal(current.VisualMap, room.VisualMap) {
		return unavailable(errors.New("room visual references changed during camera matching"))
	}
	hint, err := localizationCameraHint(verified, second.captured, time.Now())
	if err != nil {
		return unavailable(err)
	}
	s.cameraLocalizationProgress(session, job, "verified", "Camera room match verified on two fresh frames; checking it against LiDAR.")
	return hint
}

func localizationCameraHint(result twinvision.Result, captured, now time.Time) (*sitenav.VisualHint, error) {
	if result.Inliers < 12 || math.IsNaN(result.RMS) || math.IsInf(result.RMS, 0) || result.RMS < 0 || result.RMS > 5 {
		return nil, errors.New("camera match lacks the inliers or precision required for LiDAR fusion")
	}
	if captured.IsZero() || now.Sub(captured) < 0 || now.Sub(captured) >= 3*time.Second {
		return nil, errors.New("camera room match expired before LiDAR fusion")
	}
	t := result.Transform
	if math.Hypot(t[8], t[10]) <= .7 {
		return nil, errors.New("camera view is too steep to constrain robot heading")
	}
	// Optical axes mount (right, down, forward), same as Go2CameraProfile on macOS.
	offset := [3]float64{.00003, -.04297, .32715}
	body := [3]float64{t[12], t[13], t[14]}
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			body[i] -= t[j*4+i] * offset[j]
		}
	}
	return &sitenav.VisualHint{Position: sitenav.Point{X: body[0], Y: -body[2]}, Yaw: math.Atan2(-t[10], t[8]), At: float64(captured.UnixNano()) / 1e9, Inliers: int(result.Inliers), Residual: result.RMS}, nil
}

func (s *Server) awaitLocalizationCamera(session *robotSession, job *localizationCameraJob, message string) (localizationCameraFrame, error) {
	job.mu.Lock()
	job.progress.ViewID = uuid.NewString()
	job.demandAt = time.Now()
	job.mu.Unlock()
	s.cameraLocalizationProgress(session, job, "waiting", message)
	timer := time.NewTimer(25 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-job.ctx.Done():
			return localizationCameraFrame{}, job.ctx.Err()
		case <-timer.C:
			return localizationCameraFrame{}, errors.New("fresh camera frame did not arrive within 25 seconds")
		case <-tick.C:
			if err := job.owner.Check(); err != nil {
				return localizationCameraFrame{}, err
			}
			job.owner.Status()
		case frame := <-job.frames:
			return frame, nil
		}
	}
}
func (s *Server) solveLocalizationCamera(session *robotSession, job *localizationCameraJob, landmarks []byte, frame localizationCameraFrame, lens [9]float64, provenance int32, verification bool) (twinvision.Result, error) {
	s.cameraLocalizationProgress(session, job, "matching", "Matching Go2 camera features against saved room references.")
	ctx, cancel := context.WithTimeout(job.ctx, 45*time.Second)
	defer cancel()
	frame.frame.AgeMS = float64(time.Since(frame.captured)) / float64(time.Millisecond)
	type reply struct {
		result twinvision.Result
		err    error
	}
	done := make(chan reply, 1)
	go func() {
		r, e := s.solveCamera(ctx, landmarks, frame.frame, lens, provenance, verification)
		done <- reply{r, e}
	}()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return twinvision.Result{}, ctx.Err()
		case <-tick.C:
			job.owner.Status()
			if err := job.owner.CheckSearchCameraView(frame.view); err != nil {
				return twinvision.Result{}, err
			}
		case r := <-done:
			if r.err != nil {
				return r.result, r.err
			}
			if err := job.owner.CheckSearchCameraView(frame.view); err != nil {
				return r.result, err
			}
			return r.result, twinvision.Validate(r.result, 1280, 720)
		}
	}
}

func (s *Server) localizationCameraView(w http.ResponseWriter, r *http.Request) {
	var q activeObjectViewRequest
	if !decode(w, r, &q, 3<<20) {
		return
	}
	s.mu.Lock()
	session := s.observers[q.Serial]
	s.mu.Unlock()
	if session == nil {
		respond(w, nil, errors.New("localization session is unavailable"))
		return
	}
	session.searchMu.Lock()
	job := session.localizationCamera
	session.searchMu.Unlock()
	if job == nil || job.request.RequestID != q.RequestID || q.ViewID == "" {
		respond(w, nil, errors.New("camera view does not belong to this localization"))
		return
	}
	job.mu.Lock()
	if job.accepted[q.ViewID] {
		job.mu.Unlock()
		respond(w, activeReceiptResult(session, q.RequestID), nil)
		return
	}
	valid := job.ctx.Err() == nil && job.progress.State == "waiting" && job.progress.ViewID == q.ViewID
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
		respond(w, nil, errors.New("localization requires a native 1280x720 Go2 JPEG"))
		return
	}
	picture, err := jpeg.Decode(bytes.NewReader(q.Image))
	if err != nil {
		respond(w, nil, err)
		return
	}
	view, err := job.owner.SearchCameraView(q.Received, after)
	if err != nil {
		respond(w, nil, err)
		return
	}
	gray := make([]byte, 1280*720)
	for y := 0; y < 720; y++ {
		for x := 0; x < 1280; x++ {
			gray[y*1280+x] = color.GrayModel.Convert(picture.At(x, y)).(color.Gray).Y
		}
	}
	frame := localizationCameraFrame{frame: twinvision.Frame{Width: 1280, Height: 720, Codec: "H.264", Sequence: uint64(q.Received), Gray: gray}, view: view, captured: view.CapturedAt()}
	session.searchMu.Lock()
	current := session.localizationCamera == job
	session.searchMu.Unlock()
	job.mu.Lock()
	if job.accepted[q.ViewID] {
		job.mu.Unlock()
		respond(w, activeReceiptResult(session, q.RequestID), nil)
		return
	}
	if !current || job.ctx.Err() != nil || job.progress.State != "waiting" || job.progress.ViewID != q.ViewID {
		job.mu.Unlock()
		respond(w, nil, errors.New("camera demand expired during validation"))
		return
	}
	select {
	case job.frames <- frame:
	default:
		job.mu.Unlock()
		respond(w, nil, errors.New("camera view is already queued"))
		return
	}
	job.accepted[q.ViewID] = true
	job.progress.ViewID = ""
	job.progress.State = "matching"
	job.progress.Message = "Fresh camera frame accepted for room matching."
	copy := job.progress
	job.mu.Unlock()
	result := twinrobot.CommandResult{State: "running", Message: copy.Message, Snapshot: session.robot.Status(), LocalizationCamera: &copy}
	// The loop may already have published the next phase. Do not overwrite it.
	respond(w, result, nil)
}
func activeReceiptResult(session *robotSession, id string) twinrobot.CommandResult {
	session.commandMu.Lock()
	defer session.commandMu.Unlock()
	return session.commandReceipts[id].result
}
