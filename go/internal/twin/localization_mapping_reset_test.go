package twin

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

func mappingResetRoom(t *testing.T, store *Store, sources []string) Room {
	t.Helper()
	r := room()
	r.Model = []byte("PK\x03\x04preserved-model")
	r.VisualMap = json.RawMessage(`{"version":1}`)
	camera, _ := json.Marshal(map[string]string{"id": uuid.NewString(), "gatewayID": "gateway", "sourceID": "camera"})
	r.Site.Cameras = []json.RawMessage{camera}
	r, err := store.Put(r)
	if err != nil {
		t.Fatal(err)
	}
	var points []Point
	for x := 0; x < 3; x++ {
		for z := 0; z < 3; z++ {
			points = append(points, Point{X: float64(x)/10 + .05, Z: float64(z)/10 + .05})
		}
	}
	for _, source := range sources {
		if err := store.Contribute(r.Site.ID, Contribution{Source: source, Sequence: 1, ModelRevision: r.Site.ModelRevision, Points: points}); err != nil {
			t.Fatal(err)
		}
	}
	r, err = store.Get(r.Site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Points) == 0 || len(r.FloorTiles) == 0 {
		t.Fatal("fixture lacks point and floor history")
	}
	return r
}

func TestResetLocalizationRemovesOnlySoleRobotMappingHistory(t *testing.T) {
	for _, test := range []struct {
		name      string
		sources   []string
		stopError bool
		cleared   bool
	}{
		{"sole-robot", []string{"go2:go2"}, false, true},
		{"mixed-sources", []string{"go2:go2", "camera"}, false, false},
		{"other-robot", []string{"go2:other"}, false, false},
		{"unacknowledged-stop", []string{"go2:go2"}, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _ := NewStore(t.TempDir())
			before := mappingResetRoom(t, store, test.sources)
			// No saved references: historical rendering still needs cleanup.
			err := store.resetLocalization(before.Site.ID+"/"+before.Site.ModelRevision, "go2", func() error {
				if test.stopError {
					return errors.New("STOP not acknowledged")
				}
				return nil
			})
			if (err != nil) != test.stopError {
				t.Fatal(err)
			}
			after, err := store.Get(before.Site.ID)
			if err != nil {
				t.Fatal(err)
			}
			if test.cleared {
				if len(after.Points) != 0 || len(after.FloorTiles) != 0 || len(after.Sources) != 0 || after.Revision != before.Revision+1 {
					t.Fatal("wrong-pose history survived reset")
				}
			} else if !reflect.DeepEqual(after.Points, before.Points) || !reflect.DeepEqual(after.Sources, before.Sources) || after.Revision != before.Revision {
				t.Fatal("reset erased unattributed history or changed data after failed STOP")
			}
			if !reflect.DeepEqual(after.Site, before.Site) || !reflect.DeepEqual(after.Model, before.Model) || !reflect.DeepEqual(after.VisualMap, before.VisualMap) || after.HasModel != before.HasModel || after.HasVisualMap != before.HasVisualMap {
				t.Fatal("reset changed the model, visual map or cameras")
			}
			state, err := store.State(before.Site.ID)
			if err != nil || test.cleared && (len(state.Points) != 0 || len(state.FloorTiles) != 0) {
				t.Fatal("state endpoint rebuilt discarded history", err)
			}
		})
	}
}

type blockedMappingRobot struct {
	robotControl
	frame             string
	captured, release chan struct{}
	sampled           bool
}

func (r *blockedMappingRobot) MappingSample() (string, uint64, []sitenav.Vertex) {
	if r.frame == "" {
		return "", 0, nil
	}
	frame := r.frame
	if !r.sampled {
		r.sampled = true
		close(r.captured)
		<-r.release
	}
	return frame, 2, []sitenav.Vertex{{X: 10, Y: 1, Z: 20}}
}

func (r *blockedMappingRobot) ResetLocalization(uint64, string) error {
	r.frame = ""
	return nil
}

func TestResetLocalizationDrainsPreviouslyCapturedMappingSample(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	before := mappingResetRoom(t, store, []string{"go2:go2"})
	frame := before.Site.ID + "/" + before.Site.ModelRevision
	robot := &blockedMappingRobot{frame: frame, captured: make(chan struct{}), release: make(chan struct{})}
	session := &robotSession{robot: robot, serial: "go2"}
	server := &Server{Store: store}
	mapped := make(chan struct{})
	go func() { server.contributeMapping(session, []Room{before}); close(mapped) }()
	<-robot.captured
	// The lock must cover capture itself, not merely the eventual store write.
	if session.pendingMu.TryLock() {
		session.pendingMu.Unlock()
		close(robot.release)
		<-mapped
		t.Fatal("captured old alignment was not protected from reset")
	}
	resetStarted, resetDone := make(chan struct{}), make(chan error, 1)
	go func() {
		close(resetStarted)
		resetDone <- server.resetLocalization(session, twinrobot.Request{Sequence: 1000000, Frame: frame})
	}()
	<-resetStarted
	close(robot.release)
	<-mapped
	if err := <-resetDone; err != nil {
		t.Fatal(err)
	}
	// A later mapping tick sees the cleared alignment and must contribute nothing.
	server.contributeMapping(session, []Room{before})
	after, err := store.State(before.Site.ID)
	if err != nil || len(after.Points) != 0 || len(after.FloorTiles) != 0 || len(after.Sources) != 0 {
		t.Fatal("pre-reset sample resurrected the wrong aligned cloud", err)
	}
}
