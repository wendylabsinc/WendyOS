package twin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
)

type robotCommander interface {
	ExecuteCommand(context.Context, twinrobot.Command) twinrobot.CommandResult
	CommandActive() bool
}

type robotCommandRequest struct {
	EvidenceID     string  `json:"evidence_id,omitempty"`
	Serial         string  `json:"serial"`
	Action         string  `json:"action"`
	RequestID      string  `json:"request_id,omitempty"`
	Destination    string  `json:"destination,omitempty"`
	Degrees        int     `json:"degrees,omitempty"`
	DistanceMeters float64 `json:"distance_meters,omitempty"`
}

// The chat transport reaches this through the selected agent's authenticated
// container stream. It shares the map's existing observer and controller.
func (s *Server) robotCommand(w http.ResponseWriter, r *http.Request) {
	var q robotCommandRequest
	if !decode(w, r, &q, 4096) {
		return
	}
	if q.Action == "connect" {
		s.connectCommandRobot(w, r, q.Serial)
		return
	}
	s.mu.Lock()
	session := s.observers[q.Serial]
	s.mu.Unlock()
	if session == nil {
		respond(w, nil, errors.New("use robot_command with action connect to start this Go2's observations"))
		return
	}
	state := session.robot.Status()
	if q.Action == "status" {
		places := map[string]RoomObject{}
		if state.Frame != "" {
			if room, err := s.Store.Frame(state.Frame); err == nil {
				places = room.Waypoints
			}
		}
		respond(w, map[string]any{"snapshot": state, "destinations": places}, nil)
		return
	}
	if q.Action != "stop" && q.Action != "turn" && q.Action != "go_to" && q.Action != "remember" && q.Action != "forward" && q.Action != "go_to_object" {
		respond(w, nil, errors.New("choose status, stop, turn, forward, go_to, go_to_object, or remember"))
		return
	}
	if len(q.RequestID) < 8 || len(q.RequestID) > 80 {
		respond(w, nil, errors.New("request_id must contain 8 to 80 characters; reuse it for retries"))
		return
	}
	controller, ok := session.robot.(robotCommander)
	if !ok {
		respond(w, nil, errors.New("robot controller does not support chat commands"))
		return
	}
	// Stop is always live, including when history is full. Repeating Stop never
	// starts movement, and must not return an old receipt while a new goal runs.
	if q.Action == "stop" {
		session.searchMu.Lock()
		if session.searchCancel != nil {
			session.searchCancel()
		}
		session.searchMu.Unlock()
		session.cancelFind()
		respond(w, controller.ExecuteCommand(r.Context(), twinrobot.Command{Action: "stop"}), nil)
		return
	}
	// Receipts are retained for this observer's lifetime. Refuse new IDs once
	// full rather than evicting an old motion receipt and executing a late retry.
	session.commandMu.Lock()
	if old, exists := session.commandReceipts[q.RequestID]; exists {
		session.commandMu.Unlock()
		if old.request != q {
			respond(w, nil, errors.New("request_id was already used for a different command"))
			return
		}
		respond(w, old.result, nil)
		return
	}
	if len(session.commandReceipts) >= 256 {
		session.commandMu.Unlock()
		respond(w, nil, errors.New("command history is full; reopen the robot view before sending another goal"))
		return
	}
	if session.commandReceipts == nil {
		session.commandReceipts = map[string]robotCommandReceipt{}
	}
	session.commandReceipts[q.RequestID] = robotCommandReceipt{request: q, result: twinrobot.CommandResult{State: "running", Message: "Command in progress."}}
	session.commandMu.Unlock()
	finish := func(result twinrobot.CommandResult) {
		session.commandMu.Lock()
		session.commandReceipts[q.RequestID] = robotCommandReceipt{request: q, result: result}
		session.commandMu.Unlock()
		respond(w, result, nil)
	}
	reject := func(message string) {
		finish(twinrobot.CommandResult{State: "rejected", Message: message, Snapshot: session.robot.Status()})
	}
	session.searchMu.Lock()
	searching := session.searchState == "searching"
	if q.Action == "stop" && session.searchCancel != nil {
		session.searchCancel()
	}
	session.searchMu.Unlock()
	session.findMu.Lock()
	finding := session.find.State == "searching"
	session.findMu.Unlock()
	if q.Action == "stop" {
		session.cancelFind()
	} else if searching || finding {
		reject("A search is active. Stop it before issuing another goal.")
		return
	}
	command := twinrobot.Command{Action: q.Action, Degrees: q.Degrees, DistanceMeters: q.DistanceMeters}
	if q.Action == "go_to_object" {
		var err error
		command, err = objectEvidenceCommand(session, q.EvidenceID)
		if err != nil {
			reject(err.Error())
			return
		}
	}
	if q.Action == "go_to" || q.Action == "remember" {
		name := strings.TrimSpace(q.Destination)
		if name == "" || len(name) > 80 || strings.ContainsFunc(name, unicode.IsControl) {
			reject("Name a destination using 1 to 80 printable characters.")
			return
		}
		if !state.Pose || !state.Aligned || state.Frame == "" || state.Moving {
			reject("Go2 needs a current map position and must be stationary.")
			return
		}
		room, err := s.Store.Frame(state.Frame)
		if err != nil {
			reject(err.Error())
			return
		}
		found := false
		var target RoomObject
		for label, place := range room.Waypoints {
			if strings.EqualFold(label, name) {
				if found {
					reject("That destination name is ambiguous.")
					return
				}
				found, target, name = true, place, label
			}
		}
		if q.Action == "remember" {
			if controller.CommandActive() {
				reject("A robot command is active.")
				return
			}
			if err := s.Store.SaveWaypoint(state.Frame, name, RoomObject{X: state.X, Z: state.Z, SeenUnix: time.Now().Unix()}); err != nil {
				reject(err.Error())
				return
			}
			finish(twinrobot.CommandResult{State: "completed", Message: "Saved destination " + name + ".", Snapshot: state})
			return
		}
		if !found {
			reject("No saved destination named " + name + ". Use robot_status to list destinations, or remember a current position first.")
			return
		}
		command.Action, command.Frame, command.X, command.Z = "navigate", state.Frame, target.X, target.Z
	}
	finish(controller.ExecuteCommand(r.Context(), command))
}

type robotCommandReceipt struct {
	request robotCommandRequest
	result  twinrobot.CommandResult
}

// Connecting starts observations and the same STOP/avoidance preparation used
// by the map. It never replaces another operator or invents a map alignment.
func (s *Server) connectCommandRobot(w http.ResponseWriter, r *http.Request, serial string) {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	s.mu.Lock()
	session := s.observers[serial]
	s.mu.Unlock()
	if session != nil {
		respond(w, twinrobot.CommandResult{State: "completed", Message: "Robot observation session already exists.", Snapshot: session.robot.Status()}, nil)
		return
	}
	if s.Conn == nil || s.Context == nil || r.Context().Err() != nil {
		respond(w, nil, errors.New("robot gateway is unavailable"))
		return
	}
	robot, err := twinrobot.Open(s.Context, s.Conn, serial)
	if err != nil {
		respond(w, nil, err)
		return
	}
	s.mu.Lock()
	s.observers[serial] = &robotSession{robot: robot, serial: serial}
	s.mu.Unlock()
	respond(w, twinrobot.CommandResult{State: "completed", Message: "Observations started. Check robot_status for current measurements before movement.", Snapshot: robot.Status()}, nil)
}
