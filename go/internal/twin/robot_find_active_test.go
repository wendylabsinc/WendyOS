package twin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
)

type objectSearchTestOwner struct {
	mu           sync.Mutex
	state        twinrobot.SiteRobotSnapshot
	moves, turns int
	closed       bool
	fault        error
	blocked      bool
	rejectView   bool
}

func (o *objectSearchTestOwner) Status() twinrobot.SiteRobotSnapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.state
}
func (o *objectSearchTestOwner) Check() error { o.mu.Lock(); defer o.mu.Unlock(); return o.fault }
func (o *objectSearchTestOwner) Close()       { o.mu.Lock(); defer o.mu.Unlock(); o.closed = true }
func (o *objectSearchTestOwner) ObjectSearchLook(_ uint64, direction int) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fault != nil {
		return o.fault
	}
	o.turns++
	o.state.LookState = 2
	o.state.LookDirection = 1
	return nil
}
func (o *objectSearchTestOwner) ObjectSearchStep(uint64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fault != nil {
		return o.fault
	}
	if o.blocked {
		return twinrobot.ErrDiscoveryBlocked
	}
	o.moves++
	o.state.DiscoveryState = 2
	return nil
}
func (o *objectSearchTestOwner) SearchCameraView(received int64, after time.Time) (*twinrobot.SearchCameraView, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fault != nil {
		return nil, o.fault
	}
	if o.rejectView || !time.Unix(0, received).After(after) {
		return nil, errors.New("stale or unmatched view")
	}
	return nil, nil
}
func (o *objectSearchTestOwner) CheckSearchCameraView(*twinrobot.SearchCameraView) error {
	return o.Check()
}

func (o *objectSearchTestOwner) FinishSearchCameraView(view *twinrobot.SearchCameraView) error {
	if err := o.CheckSearchCameraView(view); err != nil {
		return err
	}
	o.Close()
	return nil
}

type objectSearchTestVision struct {
	mu            sync.Mutex
	calls, seenAt int
	started       chan struct{}
	release       chan struct{}
}

func (v *objectSearchTestVision) locate(ctx context.Context, _ []byte, _ string) ([4]float64, bool, error) {
	v.mu.Lock()
	v.calls++
	number := v.calls
	started, release := v.started, v.release
	v.started = nil
	v.mu.Unlock()
	if started != nil {
		close(started)
	}
	if release != nil {
		<-release
	} // Deliberately return a late success even after cancellation.
	return [4]float64{.1, .2, .3, .4}, v.seenAt > 0 && number >= v.seenAt, nil
}
func activeObjectFixture(t *testing.T, vision *objectSearchTestVision) (*Server, *robotSession, *activeObjectJob, *objectSearchTestOwner) {
	t.Helper()
	life, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	robot := &setupTestRobot{state: twinrobot.SiteRobotSnapshot{MeasurementsReady: true}}
	session := &robotSession{robot: robot, serial: "go2", commandReceipts: map[string]robotCommandReceipt{}}
	server := &Server{Context: life, observers: map[string]*robotSession{"go2": session}}
	owner := &objectSearchTestOwner{state: twinrobot.SiteRobotSnapshot{MeasurementsReady: true}}
	request := robotCommandRequest{Serial: "go2", Action: "find_active", RequestID: "find-this-object", Query: "blue chair"}
	requestCtx, requestDone := context.WithCancel(context.Background())
	initial, launch := server.prepareActiveObjectSearchUsing(requestCtx, session, request, vision, func(context.Context, robotControl) (activeObjectOwner, error) { return owner, nil })
	if launch == nil || initial.State != "running" {
		t.Fatal(initial)
	}
	session.commandReceipts[request.RequestID] = robotCommandReceipt{request: request, result: initial}
	session.activeObjectMu.Lock()
	job := session.activeObject
	session.activeObjectMu.Unlock()
	t.Cleanup(job.cancel)
	launch()
	requestDone() // Normal HTTP completion must not cancel the search.
	return server, session, job, owner
}

var objectTestJPEG struct {
	sync.Once
	data []byte
}

func activeObjectJPEG() []byte {
	objectTestJPEG.Do(func() {
		var out bytes.Buffer
		_ = jpeg.Encode(&out, image.NewRGBA(image.Rect(0, 0, 1280, 720)), nil)
		objectTestJPEG.data = out.Bytes()
	})
	return objectTestJPEG.data
}
func submitObjectTestView(server *Server, job *activeObjectJob, token string, received int64) *httptest.ResponseRecorder {
	data, _ := json.Marshal(activeObjectViewRequest{Serial: "go2", RequestID: job.request.RequestID, ViewID: token, Image: activeObjectJPEG(), Received: received, CameraMode: "go2-front-native-1280x720"})
	w := httptest.NewRecorder()
	server.activeObjectView(w, httptest.NewRequest(http.MethodPost, "/v1/robot/find-view", bytes.NewReader(data)))
	return w
}
func awaitObjectResult(t *testing.T, server *Server, job *activeObjectJob, feed bool) twinrobot.CommandResult {
	t.Helper()
	deadline := time.After(12 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		job.mu.Lock()
		result := job.result
		job.mu.Unlock()
		if result.State != "running" {
			return result
		}
		if feed && result.ObjectSearch != nil && result.ObjectSearch.ViewID != "" {
			w := submitObjectTestView(server, job, result.ObjectSearch.ViewID, time.Now().UnixNano())
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
		}
		select {
		case <-deadline:
			t.Fatal("object search did not finish")
		case <-tick.C:
		}
	}
}
func awaitObjectToken(t *testing.T, job *activeObjectJob) string {
	t.Helper()
	var token string
	waitSetupCondition(t, func() bool { job.mu.Lock(); defer job.mu.Unlock(); token = job.progress.ViewID; return token != "" })
	return token
}
func TestActiveObjectSearchFindsAfterMeasuredViewpointWithoutRoom(t *testing.T) {
	vision := &objectSearchTestVision{seenAt: 2}
	server, _, job, owner := activeObjectFixture(t, vision)
	// Retrying the same identity must return the receipt before model setup.
	retry := setupCommandCall(server, context.Background(), nil, job.request)
	if retry.Code != 200 {
		t.Fatal(retry.Body.String())
	}
	result := awaitObjectResult(t, server, job, true)
	retry = setupCommandCall(server, context.Background(), nil, job.request)
	if retry.Code != 200 || !strings.Contains(retry.Body.String(), `"state":"completed"`) {
		t.Fatal(retry.Body.String())
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if result.State != "completed" || !result.ObjectSearch.Seen || result.ObjectSearch.Navigable || result.Snapshot.Aligned || result.Snapshot.Frame != "" || owner.moves != 1 || owner.turns != 0 || !owner.closed {
		t.Fatalf("unexpected search result %+v owner%+v", result, owner)
	}
}
func TestActiveObjectSearchStopsAtMovementAndTurnLimits(t *testing.T) {
	server, _, job, owner := activeObjectFixture(t, &objectSearchTestVision{})
	result := awaitObjectResult(t, server, job, true)
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if result.State != "failed" || result.ObjectSearch.Seen || owner.moves != 3 || owner.turns != 6 || !owner.closed {
		t.Fatalf("unbounded search: %s moves%d turns%d", result.Message, owner.moves, owner.turns)
	}
}
func TestActiveObjectSearchRejectsStaleForeignAndDuplicateFrames(t *testing.T) {
	vision := &objectSearchTestVision{seenAt: 1, started: make(chan struct{}), release: make(chan struct{})}
	started := vision.started
	server, _, job, _ := activeObjectFixture(t, vision)
	token := awaitObjectToken(t, job)
	if w := submitObjectTestView(server, job, "wrong-token", time.Now().UnixNano()); w.Code == 200 {
		t.Fatal("accepted foreign view")
	}
	if w := submitObjectTestView(server, job, token, time.Now().Add(-time.Minute).UnixNano()); w.Code == 200 {
		t.Fatal("accepted stale view")
	}
	if w := submitObjectTestView(server, job, token, time.Now().UnixNano()); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	<-started
	w := submitObjectTestView(server, job, token, time.Now().UnixNano())
	if w.Code != 200 || strings.Contains(w.Body.String(), `"view_id"`) {
		t.Fatal("accepted token remained demanded", w.Body.String())
	}
	close(vision.release)
	result := awaitObjectResult(t, server, job, false)
	vision.mu.Lock()
	defer vision.mu.Unlock()
	if result.State != "completed" || vision.calls != 1 {
		t.Fatal("duplicate frame reran inference", result, vision.calls)
	}
}
func TestActiveObjectSearchDiscardsLateVisionAfterStopOrSupersession(t *testing.T) {
	for _, superseded := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop", true: "superseded"}[superseded], func(t *testing.T) {
			vision := &objectSearchTestVision{seenAt: 1, started: make(chan struct{}), release: make(chan struct{})}
			started := vision.started
			server, _, job, owner := activeObjectFixture(t, vision)
			token := awaitObjectToken(t, job)
			if w := submitObjectTestView(server, job, token, time.Now().UnixNano()); w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			<-started
			want := "canceled"
			if superseded {
				owner.mu.Lock()
				owner.fault = twinrobot.ErrLocalizationSearchSuperseded
				owner.mu.Unlock()
				want = "superseded"
			} else {
				w := setupCommandCall(server, context.Background(), nil, robotCommandRequest{Action: "stop", Serial: "go2", RequestID: "stop-search-now"})
				if w.Code != 200 {
					t.Fatal(w.Body.String())
				}
			}
			close(vision.release)
			result := awaitObjectResult(t, server, job, false)
			owner.mu.Lock()
			defer owner.mu.Unlock()
			if result.State != want || result.ObjectSearch.Seen || owner.moves+owner.turns != 0 || !owner.closed {
				t.Fatal("late vision escaped cancellation", result)
			}
		})
	}
}

func TestActiveObjectSearchRejectsOldViewAfterNewJob(t *testing.T) {
	server, session, job, _ := activeObjectFixture(t, &objectSearchTestVision{})
	token := awaitObjectToken(t, job)
	replacement := &activeObjectJob{request: robotCommandRequest{RequestID: "different-search"}, frames: make(chan activeObjectFrame, 1)}
	session.activeObjectMu.Lock()
	session.activeObject = replacement
	session.activeObjectMu.Unlock()
	w := submitObjectTestView(server, job, token, time.Now().UnixNano())
	if w.Code == 200 || len(replacement.frames) != 0 {
		t.Fatal("old camera token entered replacement search")
	}
	job.cancel()
	result := awaitObjectResult(t, server, job, false)
	if result.State != "canceled" {
		t.Fatal(result)
	}
}
