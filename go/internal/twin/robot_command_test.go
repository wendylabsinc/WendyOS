package twin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
)

type commandRobot struct {
	robotControl
	state    twinrobot.SiteRobotSnapshot
	commands []twinrobot.Command
}

func (r *commandRobot) Status() twinrobot.SiteRobotSnapshot { return r.state }
func (r *commandRobot) CommandActive() bool                 { return false }
func (r *commandRobot) ExecuteCommand(_ context.Context, q twinrobot.Command) twinrobot.CommandResult {
	r.commands = append(r.commands, q)
	return twinrobot.CommandResult{State: "completed", Snapshot: r.state}
}

func TestRobotCommandNamedDestinationsAndRetry(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	room, err := store.Put(room())
	if err != nil {
		t.Fatal(err)
	}
	r := &commandRobot{state: twinrobot.SiteRobotSnapshot{Frame: room.Site.ID + "/" + room.Site.ModelRevision, Pose: true, Aligned: true, X: 1.25, Z: 2.5}}
	s := &Server{Store: store}
	h := s.Handler()
	s.observers["go2"] = &robotSession{robot: r, serial: "go2"}
	call := func(q robotCommandRequest) *httptest.ResponseRecorder {
		b, _ := json.Marshal(q)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/robot/command", bytes.NewReader(b)))
		return w
	}
	remember := robotCommandRequest{Serial: "go2", Action: "remember", RequestID: "remember-1", Destination: "Kitchen"}
	if w := call(remember); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	q := robotCommandRequest{Serial: "go2", Action: "go_to", RequestID: "navigate-1", Destination: "kitchen"}
	for i := 0; i < 2; i++ {
		if w := call(q); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	if len(r.commands) != 1 || r.commands[0].X != 1.25 || r.commands[0].Z != 2.5 || r.commands[0].Frame != r.state.Frame {
		t.Fatalf("wrong target or duplicate movement: %+v", r.commands)
	}
	q.Destination = "garage"
	if w := call(q); w.Code != 400 {
		t.Fatal("reused request ID changed destination")
	}
	q.RequestID = "navigate-2"
	if w := call(q); !bytes.Contains(w.Body.Bytes(), []byte(`"state":"rejected"`)) {
		t.Fatal(w.Body.String())
	}
	if len(r.commands) != 1 {
		t.Fatal("unknown destination moved")
	}
	q = robotCommandRequest{Serial: "go2", Action: "status"}
	if w := call(q); !bytes.Contains(w.Body.Bytes(), []byte("Kitchen")) {
		t.Fatal(w.Body.String())
	}
}

func TestRobotCommandRejectsStaleMapAndSearch(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	r := &commandRobot{}
	s := &Server{Store: store}
	h := s.Handler()
	s.observers["go2"] = &robotSession{robot: r, serial: "go2"}
	for _, q := range []robotCommandRequest{
		{Serial: "go2", Action: "remember", RequestID: "remember-1", Destination: "Kitchen"},
		{Serial: "go2", Action: "go_to", RequestID: "navigate-1", Destination: "Kitchen"},
	} {
		b, _ := json.Marshal(q)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/robot/command", bytes.NewReader(b)))
		if !bytes.Contains(w.Body.Bytes(), []byte(`"state":"rejected"`)) {
			t.Fatal(w.Body.String())
		}
	}
	if len(r.commands) != 0 {
		t.Fatal("unlocalized movement")
	}
}
