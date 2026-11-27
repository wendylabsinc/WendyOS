package twin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

type resetRobot struct {
	robotControl
	sequence uint64
	frame    string
	err      error
}

func (r *resetRobot) ResetLocalization(sequence uint64, frame string) error {
	if sequence <= r.sequence {
		return errors.New("superseded")
	}
	r.sequence, r.frame = sequence, frame
	return r.err
}

func resetTestRoom(t *testing.T, store *Store) Room {
	t.Helper()
	r, err := store.Put(room())
	if err != nil {
		t.Fatal(err)
	}
	frame := r.Site.ID + "/" + r.Site.ModelRevision
	for _, serial := range []string{"go2", "other-go2"} {
		if err := store.SaveReferences(frame, serial, []sitenav.Surface{{Position: sitenav.Vertex{X: 1}, Stable: true}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Contribute(r.Site.ID, Contribution{Source: "camera", Sequence: 1, ModelRevision: r.Site.ModelRevision, Points: []Point{{1, 2, 3}}}); err != nil {
		t.Fatal(err)
	}
	r, err = store.Get(r.Site.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResetLocalizationClearsOnlyRobotReferencesAndPendingSearch(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	before := resetTestRoom(t, store)
	other := resetTestRoom(t, store)
	frame := before.Site.ID + "/" + before.Site.ModelRevision
	server := &Server{Store: store}
	handler := server.Handler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	robot := &resetRobot{sequence: 2_000_003}
	session := &robotSession{robot: robot, serial: "go2", searchRoot: 2_000_000, searchCancel: cancel, searchState: "complete", searchReport: json.RawMessage(`{"candidates":[{}]}`), pendingSequence: 2_000_003, pendingRoot: 2_000_000, pendingFrame: frame, pending: twinrobot.SiteFusionReport{Candidates: []twinrobot.SiteRobotCandidate{{X: 10}}}}
	server.robots["handle"] = session
	body, _ := json.Marshal(twinrobot.Request{Operation: "reset-localization", Handle: "handle", Sequence: 3, Frame: frame})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1/robot", bytes.NewReader(body)))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var reply twinrobot.Response
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil || reply.SearchMessage == "" {
		t.Fatalf("reset reply: %v %+v", err, reply)
	}
	if ctx.Err() == nil || robot.sequence != 3_000_000 || robot.frame != frame || session.pendingSequence != 3_000_000 || session.pendingRoot != 0 || session.pendingFrame != "" || len(session.pending.Candidates) != 0 || session.searchRoot != 3_000_000 || session.searchReport != nil || session.searchState != "" {
		t.Fatal("reset failed to cancel and invalidate pending work")
	}
	after, err := store.Get(before.Site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := after.References["go2"]; exists {
		t.Fatal("wrong robot references survived")
	}
	if !reflect.DeepEqual(after.References["other-go2"], before.References["other-go2"]) || !reflect.DeepEqual(after.Points, before.Points) || !reflect.DeepEqual(after.Site, before.Site) || !reflect.DeepEqual(after.Sources, before.Sources) || after.Revision != before.Revision+1 {
		t.Fatal("reset changed unrelated room data")
	}
	otherAfter, _ := store.Get(other.Site.ID)
	if !reflect.DeepEqual(other, otherAfter) {
		t.Fatal("reset changed another room")
	}
}

func TestResetLocalizationFailurePreservesReferencesAndNewerSearch(t *testing.T) {
	for _, fault := range []string{"stale-search", "stale-robot", "stop-failure", "wrong-revision", "removed-room"} {
		t.Run(fault, func(t *testing.T) {
			store, _ := NewStore(t.TempDir())
			before := resetTestRoom(t, store)
			frame := before.Site.ID + "/" + before.Site.ModelRevision
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			robot := &resetRobot{}
			session := &robotSession{robot: robot, serial: "go2", searchRoot: 1, searchCancel: cancel, pendingSequence: 1, pendingFrame: frame}
			switch fault {
			case "stale-search":
				session.searchRoot = 3
			case "stale-robot":
				robot.sequence = 3
			case "stop-failure":
				robot.err = errors.New("STOP not acknowledged")
			case "wrong-revision":
				frame = before.Site.ID + "/outdated"
			case "removed-room":
				before.Removed = true
				if _, err := store.Put(before); err != nil {
					t.Fatal(err)
				}
				before, _ = store.Get(before.Site.ID)
			}
			err := (&Server{Store: store}).resetLocalization(session, twinrobot.Request{Sequence: 2, Frame: frame})
			if err == nil {
				t.Fatal("invalid reset succeeded")
			}
			after, _ := store.Get(before.Site.ID)
			if !reflect.DeepEqual(before, after) || ctx.Err() != nil || session.pendingSequence != 1 || session.pendingFrame == "" {
				t.Fatal("failed reset erased data or cancelled newer search")
			}
		})
	}
}
