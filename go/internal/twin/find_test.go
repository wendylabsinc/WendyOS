package twin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// findFixture is a robot that executes every order instantly, and a camera
// that sees the object only at seeAt (viewpoint, heading).
type findFixture struct {
	mu                    sync.Mutex
	x, z                  float64
	viewpoint, heading    int
	orders                []string
	seeAt                 [2]int
	unranged, leaseLost   bool
	refuseWalks           bool
	barkErr               error
	exploreErr            error
	stopped               uint64
	destinations, barkCnt int
}

func (f *findFixture) Status() twinrobot.SiteRobotSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := twinrobot.SiteRobotSnapshot{Ready: true, Aligned: true, Pose: true, X: f.x, Z: f.z, LookState: 2, LookDirection: 1,
		Message: "Live LiDAR and odometry.", ControlWarning: "Could not disable onboard avoidance."}
	if f.leaseLost {
		state.Message = twinrobot.MovementLeaseLostMessage
	}
	return state
}
func (f *findFixture) Stop(sequence uint64) { f.mu.Lock(); f.stopped = sequence; f.mu.Unlock() }
func (f *findFixture) Destination(_ uint64, _ string, x, z float64, move bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuseWalks {
		return errors.New("No clear route.")
	}
	f.orders = append(f.orders, "walk")
	f.destinations++
	f.x, f.z = x, z
	return nil
}
func (f *findFixture) Look(uint64, int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orders = append(f.orders, "turn")
	f.heading++
	return nil
}
func (f *findFixture) ExploreTarget(visited []sitenav.Point) (float64, float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exploreErr != nil {
		return 0, 0, f.exploreErr
	}
	f.viewpoint++
	f.heading = 0
	return float64(2 * f.viewpoint), 0, nil
}
func (f *findFixture) LocateInView(u, bottom float64) (float64, float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unranged {
		return 0, 0, twinrobot.ErrNoViewRange
	}
	return f.x + 3, f.z, nil
}
func (f *findFixture) Bark() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orders = append(f.orders, "bark")
	f.barkCnt++
	return f.barkErr
}
func (f *findFixture) locate(context.Context, []byte, string) ([4]float64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seeAt == [2]int{f.viewpoint, f.heading} {
		return [4]float64{.4, .5, .2, .2}, true, nil
	}
	return [4]float64{}, false, nil
}

// runFind plays the client: it answers every camera request with a frame.
func runFind(t *testing.T, robot *findFixture, job findJob, feed bool) (*robotSession, []RoomObject) {
	t.Helper()
	oldSettle, oldTimeout := findSettle, findViewTimeout
	findSettle, findViewTimeout = 0, 300*time.Millisecond
	defer func() { findSettle, findViewTimeout = oldSettle, oldTimeout }()
	var saved []RoomObject
	job.root, job.frame, job.views = 1000000, "room/frame", make(chan findView, 1)
	if job.query == "" {
		job.query = "toolbox"
	}
	job.save = func(o RoomObject) error { saved = append(saved, o); return nil }
	session := &robotSession{robot: nil, findRoot: job.root, findViews: job.views, find: twinrobot.FindStatus{State: "searching"}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); (&Server{}).find(ctx, cancel, session, robot, robot, job) }()
	for {
		select {
		case <-done:
			return session, saved
		case <-time.After(time.Millisecond):
			session.findMu.Lock()
			wanted := session.find.WantsView
			session.findMu.Unlock()
			if feed && wanted != 0 {
				_ = session.acceptFindView(wanted, []byte{0xff, 0xd8, 0xff, 0xe0})
			}
		}
	}
}

func TestFindTurnsWalksOverRemembersAndBarks(t *testing.T) {
	robot := &findFixture{seeAt: [2]int{0, 2}}
	session, saved := runFind(t, robot, findJob{bark: true}, true)
	f := session.find
	if f.State != "found" || !f.Seen || !f.Located || !f.Barked || f.BarkFailure != "" || f.Views != 3 || f.Viewpoints != 1 || f.BoxView != 3 || f.WantsView != 0 {
		t.Fatalf("unexpected result: %+v", f)
	}
	if got := strings.Join(robot.orders, " "); got != "turn turn walk bark" {
		t.Fatalf("orders: %s", got)
	}
	// Stand off 0.8 m short of the object 3 m ahead.
	if len(saved) != 1 || saved[0].X != 3 || robot.x < 2.19 || robot.x > 2.21 {
		t.Fatalf("saved=%+v robot.x=%v", saved, robot.x)
	}
}

func TestFindExploresOtherPlacesAndStopsAtItsLimit(t *testing.T) {
	robot := &findFixture{seeAt: [2]int{2, 1}}
	session, _ := runFind(t, robot, findJob{}, true)
	if f := session.find; f.State != "found" || f.Viewpoints != 3 || f.Barked || robot.barkCnt != 0 {
		t.Fatalf("did not find at the third place: %+v", f)
	}
	robot = &findFixture{seeAt: [2]int{-1, -1}}
	session, saved := runFind(t, robot, findJob{bark: true}, true)
	if f := session.find; f.State != "not-found" || f.Viewpoints != findViewpointLimit || f.Views != findViewpointLimit*twinrobot.LocalizationTurnCount || f.Seen || len(saved) != 0 || robot.barkCnt != 0 {
		t.Fatalf("unbounded or false search: %+v", f)
	}
	// With nowhere new to stand, the search ends honestly after one place.
	robot = &findFixture{seeAt: [2]int{-1, -1}, exploreErr: errors.New("LiDAR shows no unexplored clear floor within reach")}
	session, _ = runFind(t, robot, findJob{}, true)
	if f := session.find; f.State != "not-found" || f.Viewpoints != 1 || !strings.Contains(f.Message, "1 place") || !strings.Contains(f.Message, "no unexplored clear floor") {
		t.Fatalf("unexpected: %+v", f)
	}
}

func TestFindStartsAtRememberedLocation(t *testing.T) {
	robot := &findFixture{seeAt: [2]int{0, 0}}
	session, _ := runFind(t, robot, findJob{known: &RoomObject{X: 5}}, true)
	if f := session.find; f.State != "found" || !f.Remembered || f.Views != 1 || robot.destinations != 2 {
		t.Fatalf("did not start where it was last seen: %+v walks=%d", f, robot.destinations)
	}
}

func TestFindReportsFaultsWithoutClaimingSuccess(t *testing.T) {
	// No camera frames: the client is not showing the live view.
	robot := &findFixture{}
	session, _ := runFind(t, robot, findJob{}, false)
	if f := session.find; f.State != "failed" || !strings.Contains(f.Message, "camera frames") || robot.stopped != 1999999 {
		t.Fatalf("missing frames: %+v stop=%d", f, robot.stopped)
	}
	// A movement that lost its operator lease ends the mission.
	robot = &findFixture{seeAt: [2]int{1, 0}, leaseLost: true}
	session, _ = runFind(t, robot, findJob{}, true)
	if f := session.find; f.State != "failed" || f.Message != findViewClosed {
		t.Fatalf("lease loss: %+v", f)
	}
	// Seen but unrangeable, and an agent without the bark command.
	robot = &findFixture{seeAt: [2]int{0, 0}, unranged: true, barkErr: status.Error(codes.InvalidArgument, "unknown Go2 command")}
	session, saved := runFind(t, robot, findJob{bark: true}, true)
	if f := session.find; f.State != "found" || f.Located || f.Barked || !strings.Contains(f.BarkFailure, "Update the agent") || !strings.Contains(f.Message, "could not measure") || len(saved) != 0 || robot.destinations != 0 {
		t.Fatalf("unranged sighting: %+v", f)
	}
	// Blocked approach: still found, and says why it stayed back.
	robot = &findFixture{seeAt: [2]int{0, 0}, refuseWalks: true}
	session, _ = runFind(t, robot, findJob{}, true)
	if f := session.find; f.State != "found" || !strings.Contains(f.Message, "could not walk closer: No clear route.") {
		t.Fatalf("blocked approach: %+v", f)
	}
}

func TestFindViewsAreMatchedToTheirRequest(t *testing.T) {
	session := &robotSession{findViews: make(chan findView, 1), find: twinrobot.FindStatus{State: "searching", WantsView: 4}}
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0}
	if session.acceptFindView(3, jpeg) == nil || session.acceptFindView(0, jpeg) == nil || session.acceptFindView(4, []byte("PNG!")) == nil {
		t.Fatal("accepted a stale, unrequested or non-JPEG frame")
	}
	if err := session.acceptFindView(4, jpeg); err != nil || len(session.findViews) != 1 {
		t.Fatalf("rejected the requested frame: %v", err)
	}
	session.find.State = "found"
	if session.acceptFindView(4, jpeg) == nil {
		t.Fatal("accepted a frame after the search ended")
	}
}

func TestNormalizeObjectAndStandOff(t *testing.T) {
	if normalizeObject("  The   Toolbox ") != "toolbox" || normalizeObject("a red\ncup") != "red cup" || normalizeObject("x\x00y") != "" || normalizeObject(strings.Repeat("a", 81)) != "" {
		t.Fatal("object names are not normalized")
	}
	if x, z, far := standOff(0, 0, 3, 4, 1); !far || x < 2.39 || x > 2.41 || z < 3.19 || z > 3.21 {
		t.Fatalf("stand-off point: %v %v %v", x, z, far)
	}
	if _, _, far := standOff(0, 0, .9, 0, .8); far {
		t.Fatal("walks when already beside the object")
	}
}

func TestVisionModelSelectionAndGrounding(t *testing.T) {
	var asked map[string]any
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"qwen3:4b"},{"name":"qwen3-vl:4b"}]}`))
		case "/api/show":
			var q struct{ Model string }
			json.NewDecoder(r.Body).Decode(&q)
			if q.Model == "qwen3-vl:4b" {
				w.Write([]byte(`{"capabilities":["completion","vision"]}`))
			} else {
				w.Write([]byte(`{"capabilities":["completion","tools"]}`))
			}
		case "/api/chat":
			json.NewDecoder(r.Body).Decode(&asked)
			w.Write([]byte(`{"done":true,"message":{"content":"{\"objects\":[{\"label\":\"toolbox\",\"box\":[100,200,300,600]}]}"}}`))
		}
	}))
	defer ollama.Close()
	address, _ := url.Parse(ollama.URL)
	port, _ := strconv.Atoi(address.Port())
	vision, err := selectPreferredVisionModel(context.Background(), ollama.Client(), uint32(port), "qwen3:4b")
	if err != nil || vision.model != "qwen3-vl:4b" {
		t.Fatalf("picked %q: %v", vision.model, err)
	}
	box, found, err := vision.locate(context.Background(), []byte{0xff, 0xd8}, `tool"box`)
	if err != nil || !found || box != [4]float64{.1, .2, .2, .4} {
		t.Fatalf("box=%v found=%v err=%v", box, found, err)
	}
	message := asked["messages"].([]any)[0].(map[string]any)
	if message["images"].([]any)[0] != "/9g=" || !strings.Contains(message["content"].(string), `"tool\"box"`) {
		t.Fatalf("request lost the image or quoted the query unsafely: %v", message)
	}
	for _, content := range []string{`{"objects":[{"label":"x","box":[5,5,5,5]}]}`, `{"objects":[{"label":"x","box":[0,0,2000,10]}]}`} {
		if _, found, err := parseGrounding(content); found || err == nil {
			t.Fatalf("%s: found=%v err=%v", content, found, err)
		}
	}
	if _, _, err := parseGrounding("I see a toolbox"); err == nil {
		t.Fatal("accepted prose as a detection")
	}
	if _, err := selectVisionModel(context.Background(), ollama.Client(), 70000, ""); err == nil {
		t.Fatal("accepted an invalid port")
	}
}

func TestSavedObjectsSurviveClientRoomEdits(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	room, err := store.Put(Room{Site: Site{Version: 1, ID: "11111111-1111-4111-8111-111111111111", Name: "Shop", ModelRevision: "22222222-2222-4222-8222-222222222222", MetresPerModelUnit: 1, Cameras: []json.RawMessage{}}})
	if err != nil {
		t.Fatal(err)
	}
	frame := room.Site.ID + "/" + room.Site.ModelRevision
	if err = store.SaveObject(frame, "toolbox", RoomObject{X: 1, Z: 2, SeenUnix: 3}); err != nil {
		t.Fatal(err)
	}
	room, _ = store.Frame(frame)
	room.Objects = nil // clients never send objects back
	room.Site.Name = "Workshop"
	if room, err = store.Put(room); err != nil || room.Objects["toolbox"] != (RoomObject{X: 1, Z: 2, SeenUnix: 3}) {
		t.Fatalf("client edit dropped the saved object: %+v %v", room.Objects, err)
	}
	if store.SaveObject(room.Site.ID+"/33333333-3333-4333-8333-333333333333", "toolbox", RoomObject{}) == nil {
		t.Fatal("saved into a changed room frame")
	}
}
