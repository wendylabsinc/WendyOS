package twin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
	"github.com/wendylabsinc/wendy/go/internal/twinvision"
)

type localizationCameraTestOwner struct {
	*activeOwnerTest
	viewMu  sync.Mutex
	viewErr error
}

func (o *localizationCameraTestOwner) SearchCameraView(int64, time.Time) (*twinrobot.SearchCameraView, error) {
	return &twinrobot.SearchCameraView{}, nil
}
func (o *localizationCameraTestOwner) CheckSearchCameraView(*twinrobot.SearchCameraView) error {
	o.viewMu.Lock()
	defer o.viewMu.Unlock()
	return o.viewErr
}
func cameraTestResult() twinvision.Result {
	return twinvision.Result{Success: 1, Source: 2, Matches: 30, Inliers: 25, FittedLens: 2, FX: 864, FY: 864, CX: 640, CY: 360, RMS: 1, Transform: [16]float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 2, 1, 3, 1}}
}
func cameraLocalizationFixture(t *testing.T) (*Server, *robotSession, *localizationCameraTestOwner, robotCommandRequest, string) {
	t.Helper()
	s, session, base, q, _ := activeFixture(t)
	base.ctx = context.Background()
	owner := &localizationCameraTestOwner{activeOwnerTest: base}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.Store = store
	room := room()
	room.Model = []byte("PK\x03\x04fixture")
	room.VisualMap, _ = json.Marshal(map[string]any{"version": 1, "frameCount": 1, "modelRevision": room.Site.ModelRevision, "modelSHA256": fmt.Sprintf("%x", sha256.Sum256(room.Model)), "landmarks": make([]byte, 120*131*4)})
	if _, err = s.Store.Put(room); err != nil {
		t.Fatal(err)
	}
	session.commandReceipts = map[string]robotCommandReceipt{q.RequestID: {request: q, result: twinrobot.CommandResult{State: "running"}}}
	s.cameraWorker = make(chan struct{}, 1)
	return s, session, owner, q, room.Site.ID + "/" + room.Site.ModelRevision
}
func waitCameraDemand(t *testing.T, session *robotSession, previous string) (*localizationCameraJob, string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		session.searchMu.Lock()
		job := session.localizationCamera
		session.searchMu.Unlock()
		if job != nil {
			job.mu.Lock()
			token, state := job.progress.ViewID, job.progress.State
			job.mu.Unlock()
			if state == "waiting" && token != "" && token != previous {
				return job, token
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("camera demand not published")
	return nil, ""
}
func submitCameraTestFrame(job *localizationCameraJob, captured time.Time) {
	job.frames <- localizationCameraFrame{frame: twinvision.Frame{Width: 1280, Height: 720, Codec: "H.264", Sequence: 1, Gray: make([]byte, 1280*720)}, view: &twinrobot.SearchCameraView{}, captured: captured}
}
func TestLocalizationCameraNeedsTwoFreshVerifiedFrames(t *testing.T) {
	s, session, owner, q, frame := cameraLocalizationFixture(t)
	calls := make(chan bool, 2)
	s.cameraSolver = func(_ context.Context, _ []byte, f twinvision.Frame, k [9]float64, p int32, verify bool) (twinvision.Result, error) {
		calls <- verify
		if p != 2 {
			t.Errorf("estimated Go2 preset mislabeled: %d", p)
		}
		if !verify && k[0] != 864.39938 {
			t.Errorf("unexpected Go2 lens %v", k)
		}
		return cameraTestResult(), nil
	}
	type result struct {
		hint bool
		at   float64
	}
	done := make(chan result, 1)
	go func() {
		v := s.initialLocalizationCamera(context.Background(), session, q, frame, owner)
		r := result{hint: v != nil}
		if v != nil {
			r.at = v.At
		}
		done <- r
	}()
	job, first := waitCameraDemand(t, session, "")
	submitCameraTestFrame(job, time.Now().Add(-time.Second))
	_, second := waitCameraDemand(t, session, first)
	if second == first {
		t.Fatal("reused first frame demand")
	}
	select {
	case <-done:
		t.Fatal("accepted a single camera view")
	default:
	}
	at := time.Now()
	submitCameraTestFrame(job, at)
	select {
	case r := <-done:
		if !r.hint || math.Abs(r.at-float64(at.UnixNano())/1e9) > 1e-6 {
			t.Fatalf("missing or redated hint %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("camera pair did not finish")
	}
	if <-calls || !<-calls {
		t.Fatal("missing verification pass")
	}
	if p := localizationCameraProgress(session, q.RequestID); p.State != "verified" || p.ViewID != "" {
		t.Fatalf("%+v", p)
	}
}
func TestLocalizationCameraFailsClosedAfterMovementDuringSolve(t *testing.T) {
	s, session, owner, q, frame := cameraLocalizationFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	s.cameraSolver = func(context.Context, []byte, twinvision.Frame, [9]float64, int32, bool) (twinvision.Result, error) {
		close(started)
		<-release
		return cameraTestResult(), nil
	}
	done := make(chan bool, 1)
	go func() { done <- s.initialLocalizationCamera(context.Background(), session, q, frame, owner) != nil }()
	job, _ := waitCameraDemand(t, session, "")
	submitCameraTestFrame(job, time.Now())
	<-started
	owner.viewMu.Lock()
	owner.viewErr = errors.New("Go2 moved after camera frame")
	owner.viewMu.Unlock()
	close(release)
	if <-done {
		t.Fatal("accepted camera result after robot moved")
	}
	if p := localizationCameraProgress(session, q.RequestID); p.State != "unavailable" || !strings.Contains(p.Message, "moved") {
		t.Fatalf("%+v", p)
	}
}
func TestLocalizationCameraFailureKeepsExactReasonForLiDARFallback(t *testing.T) {
	for _, mode := range []string{"missing-map", "worker-failure", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			s, session, owner, q, frame := cameraLocalizationFixture(t)
			if mode == "missing-map" {
				room, _ := s.Store.Frame(frame)
				full, _ := s.Store.Get(room.Site.ID)
				full.VisualMap = json.RawMessage(`{}`)
				s.Store, _ = NewStore(t.TempDir())
				full.Revision = 0
				if _, err := s.Store.Put(full); err != nil {
					t.Fatal(err)
				}
			}
			s.cameraSolver = func(context.Context, []byte, twinvision.Frame, [9]float64, int32, bool) (twinvision.Result, error) {
				return twinvision.Result{}, errors.New("native worker unavailable")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan bool, 1)
			go func() { done <- s.initialLocalizationCamera(ctx, session, q, frame, owner) != nil }()
			if mode != "missing-map" {
				job, _ := waitCameraDemand(t, session, "")
				if mode == "canceled" {
					cancel()
				} else {
					submitCameraTestFrame(job, time.Now())
				}
			}
			if <-done {
				t.Fatal("unexpected visual hint")
			}
			p := localizationCameraProgress(session, q.RequestID)
			expected := map[string]string{"missing-map": "room visual references", "worker-failure": "native worker unavailable", "canceled": "context canceled"}[mode]
			if p.State != "unavailable" || p.ViewID != "" || !strings.Contains(p.Message, expected) {
				t.Fatalf("%+v", p)
			}
		})
	}
}
func TestLocalizationCameraHintPreservesTimeAndConvertsBodyMount(t *testing.T) {
	now := time.Now()
	r := cameraTestResult()
	hint, err := localizationCameraHint(r, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(hint.Position.X-(2-.00003)) > 1e-9 || math.Abs(hint.Position.Y-(-3+.32715)) > 1e-9 || math.Abs(hint.Yaw+math.Pi/2) > 1e-9 {
		t.Fatalf("bad body hint %+v", hint)
	}
	for _, at := range []time.Time{time.Time{}, now.Add(-3 * time.Second), now.Add(time.Millisecond)} {
		if _, err := localizationCameraHint(r, at, now); err == nil {
			t.Fatalf("accepted invalid camera time %s", at)
		}
	}
	r.Transform[8], r.Transform[10] = 0, 0
	if _, err := localizationCameraHint(r, now, now); err == nil {
		t.Fatal("accepted vertical camera view")
	}
}
func TestLocalizationCameraViewRejectsWrongCommandDemandAndImage(t *testing.T) {
	s, session, owner, q, _ := cameraLocalizationFixture(t)
	job := &localizationCameraJob{ctx: context.Background(), request: q, owner: owner, progress: twinrobot.LocalizationCameraProgress{State: "waiting", ViewID: "expected"}, accepted: map[string]bool{}, frames: make(chan localizationCameraFrame, 1)}
	session.localizationCamera = job
	for _, mode := range []string{"request", "token", "size", "jpeg", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			input := activeObjectViewRequest{Serial: q.Serial, RequestID: q.RequestID, ViewID: "expected", CameraMode: "go2-front-native-1280x720", Image: []byte("not-jpeg")}
			switch mode {
			case "request":
				input.RequestID = "wrong"
			case "token":
				input.ViewID = "old"
			case "size":
				input.Image = make([]byte, 2<<20+1)
			case "canceled":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				job.ctx = ctx
			}
			raw, _ := json.Marshal(input)
			w := httptest.NewRecorder()
			s.localizationCameraView(w, httptest.NewRequest("POST", "/v1/robot/localize-view", bytes.NewReader(raw)))
			if w.Code == 200 {
				t.Fatalf("accepted %s", mode)
			}
			if len(job.frames) != 0 {
				t.Fatal("invalid image queued")
			}
		})
	}
}

func TestLocalizationCameraHTTPQueuesOnceAndPreservesOwnerCapture(t *testing.T) {
	s, session, owner, q, _ := cameraLocalizationFixture(t)
	job := &localizationCameraJob{ctx: context.Background(), request: q, owner: owner, progress: twinrobot.LocalizationCameraProgress{State: "waiting", ViewID: "expected"}, accepted: map[string]bool{}, frames: make(chan localizationCameraFrame, 1)}
	session.localizationCamera = job
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, image.NewGray(image.Rect(0, 0, 1280, 720)), nil); err != nil {
		t.Fatal(err)
	}
	input := activeObjectViewRequest{Serial: q.Serial, RequestID: q.RequestID, ViewID: "expected", CameraMode: "go2-front-native-1280x720", Image: jpegBytes.Bytes(), Received: time.Now().UnixNano()}
	raw, _ := json.Marshal(input)
	call := func() int {
		w := httptest.NewRecorder()
		s.localizationCameraView(w, httptest.NewRequest("POST", "/v1/robot/localize-view", bytes.NewReader(raw)))
		return w.Code
	}
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); statuses <- call() }()
	}
	wg.Wait()
	close(statuses)
	for code := range statuses {
		if code != 200 {
			t.Fatalf("duplicate submission status %d", code)
		}
	}
	if len(job.frames) != 1 {
		t.Fatalf("queued %d copies", len(job.frames))
	}
	queued := <-job.frames
	if len(queued.frame.Gray) != 1280*720 || queued.frame.Validate() != nil {
		t.Fatal("invalid decoded worker frame")
	}
	if queued.captured != queued.view.CapturedAt() || queued.captured == time.Unix(0, input.Received) {
		t.Fatal("trusted caller clock instead of owner-validated capture time")
	}
	if call() != 200 || len(job.frames) != 0 {
		t.Fatal("duplicate frame was requeued")
	}
	session.searchMu.Lock()
	session.localizationCamera = &localizationCameraJob{request: robotCommandRequest{RequestID: "new"}}
	session.searchMu.Unlock()
	if call() == 200 {
		t.Fatal("accepted view from superseded job")
	}
	if localizationCameraProgress(session, q.RequestID) != nil {
		t.Fatal("older receipt inherited newer job camera demand")
	}
}
func TestLocalizationCameraHintRejectsInsufficientFusionEvidence(t *testing.T) {
	for _, mode := range []string{"inliers", "residual"} {
		t.Run(mode, func(t *testing.T) {
			r := cameraTestResult()
			if mode == "inliers" {
				r.Inliers = 11
			} else {
				r.RMS = 5.1
			}
			now := time.Now()
			if _, err := localizationCameraHint(r, now, now); err == nil {
				t.Fatal("reported verified camera that fusion would discard")
			}
		})
	}
}
