package twin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
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
type commandRobotOpener func(context.Context, string) (robotControl, error)

func (s *Server) robotCommand(w http.ResponseWriter, r *http.Request) {
	s.robotCommandUsing(w, r, s.openCommandRobot)
}

func (s *Server) robotCommandUsing(w http.ResponseWriter, r *http.Request, open commandRobotOpener) {
	s.robotCommandUsingSearch(w, r, open, beginLocalizationSearchOwner)
}

func (s *Server) robotCommandUsingSearch(w http.ResponseWriter, r *http.Request, open commandRobotOpener, begin activeSearchOwnerFactory) {
	var q robotCommandRequest
	if !decode(w, r, &q, 4096) {
		return
	}
	if err := validateRobotCommand(q); err != nil {
		respond(w, nil, err)
		return
	}
	if q.Action == "connect" {
		setupContext, done := s.beginCommandSetup(r.Context(), q.Serial)
		defer done()
		session, created, err := s.ensureCommandRobotRecovery(setupContext, q.Serial, open, true)
		if err != nil {
			respond(w, nil, err)
			return
		}
		message := "Robot observation session already exists."
		if created {
			message = "Observations started. Check robot_status for current measurements before movement."
		}
		respond(w, twinrobot.CommandResult{State: "completed", Message: message, Snapshot: session.robot.Status()}, nil)
		return
	}
	if q.Action == "stop" {
		s.cancelCommandSetups(q.Serial)
	}
	autoEligible := q.Action == "turn" || q.Action == "forward" || q.Action == "go_to" || q.Action == "localize" || q.Action == "localize_active"
	if autoEligible {
		// Register before checking observations so Stop orders setup and dispatch
		// cancellation consistently, even when requests arrive together.
		setupContext, done := s.beginCommandSetup(r.Context(), q.Serial)
		defer done()
		r = r.WithContext(setupContext)
	}
	s.mu.Lock()
	session := s.observers[q.Serial]
	s.mu.Unlock()
	if q.Action == "receipt" {
		// Internal transport recovery is a lookup, never a replay. An observer
		// replacement may have lost its receipts, so absence means unknown.
		result := twinrobot.CommandResult{State: "unknown", Message: "No command receipt exists in the current observation session; the prior outcome is unknown. The lookup sent no new command."}
		if session != nil {
			session.commandMu.Lock()
			if receipt, exists := session.commandReceipts[q.RequestID]; exists {
				result = receipt.result
			}
			session.commandMu.Unlock()
		}
		respond(w, result, nil)
		return
	}
	// Resolve an old identity before reconnecting. Retrying a lost command
	// response must never create a fresh controller or execute that goal again.
	if session != nil && q.Action != "stop" && q.Action != "status" {
		session.commandMu.Lock()
		old, exists := session.commandReceipts[q.RequestID]
		session.commandMu.Unlock()
		if exists {
			if old.request != q {
				respond(w, nil, errors.New("request_id was already used for a different command"))
			} else {
				respond(w, old.result, nil)
			}
			return
		}
	}
	autoSetup := autoEligible && (session == nil || session.robot.Status().Disconnected)
	if autoSetup {
		var err error
		session, _, err = s.ensureCommandRobot(r.Context(), q.Serial, open)
		if err != nil {
			respond(w, nil, err)
			return
		}
	}
	if session == nil {
		message := "No robot observations are active. Use action connect to inspect the robot."
		if q.Action == "go_to_object" {
			message = "Object evidence is missing; find the object again before approaching."
		}
		if q.Action == "stop" {
			message = "Pending command startup canceled. No observation session exists to confirm a robot stop."
		}
		respond(w, nil, errors.New(message))
		return
	}
	state := session.robot.Status()
	if q.Action == "status" {
		places := map[string]Waypoint{}
		if state.Frame != "" {
			if room, err := s.Store.Frame(state.Frame); err == nil {
				places = room.Waypoints
			}
		}
		respond(w, map[string]any{"snapshot": state, "destinations": places}, nil)
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
	if session.commandRetired {
		session.commandMu.Unlock()
		respond(w, nil, errors.New("robot observations were replaced; retry with the same request_id on the current session"))
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
	idleLocalization := q.Action == "localize"
	waitForMeasurements := autoSetup
	if !state.Moving && !controller.CommandActive() {
		if idleLocalization {
			waitForMeasurements = waitForMeasurements || !state.IdleMeasurementsReady
		} else if autoEligible {
			waitForMeasurements = waitForMeasurements || !state.MeasurementsReady
		}
	}
	if waitForMeasurements {
		if err := waitCommandMeasurements(r.Context(), session.robot, idleLocalization); err != nil {
			outcome := "rejected"
			if r.Context().Err() != nil {
				outcome = "canceled"
			}
			finish(twinrobot.CommandResult{State: outcome, Message: err.Error(), Snapshot: session.robot.Status()})
			return
		}
		state = session.robot.Status()
	}
	if err := r.Context().Err(); err != nil {
		finish(twinrobot.CommandResult{State: "canceled", Message: err.Error(), Snapshot: session.robot.Status()})
		return
	}
	session.searchMu.Lock()
	searching := session.searchState == "searching"
	if q.Action == "stop" && session.searchCancel != nil {
		session.searchCancel()
	}
	session.searchMu.Unlock()
	if searching {
		reject("A search is active. Stop it before issuing another goal.")
		return
	}
	if q.Action == "localize" {
		finish(s.localizeCommand(r.Context(), session, q, controller))
		return
	}
	if q.Action == "localize_active" {
		initial, launch := s.prepareActiveLocalization(r.Context(), session, q, controller, begin)
		finish(initial)
		if launch != nil {
			launch()
		}
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
		var target Waypoint
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
			if err := s.Store.SaveWaypoint(state.Frame, name, Waypoint{X: state.X, Z: state.Z, SeenUnix: time.Now().Unix()}); err != nil {
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

// validateRobotCommand runs before opening subscriptions or preparing control.
// Controller checks remain authoritative for live geometry and readiness.
func validateRobotCommand(q robotCommandRequest) error {
	if !unitree.ValidSerial(q.Serial) {
		return errors.New("invalid robot serial")
	}
	switch q.Action {
	case "connect", "status":
		return nil
	case "stop", "turn", "forward", "go_to", "remember", "go_to_object", "receipt", "localize", "localize_active":
	default:
		return errors.New("choose connect, status, stop, turn, forward, go_to, go_to_object, remember, localize, or localize_active")
	}
	if len(q.RequestID) < 8 || len(q.RequestID) > 80 {
		return errors.New("request_id must contain 8 to 80 characters; reuse it for retries")
	}
	if q.Action == "turn" && (q.Degrees == 0 || q.Degrees < -180 || q.Degrees > 180) {
		return errors.New("turn by 1 to 180 degrees; positive is left, negative is right")
	}
	if q.Action == "forward" && (math.IsNaN(q.DistanceMeters) || math.IsInf(q.DistanceMeters, 0) || q.DistanceMeters < .1 || q.DistanceMeters > 2) {
		return errors.New("forward distance must be 0.1 to 2 meters")
	}
	if (q.Action == "localize" || q.Action == "localize_active") && (len(strings.TrimSpace(q.Destination)) > 80 || strings.ContainsFunc(q.Destination, unicode.IsControl)) {
		return errors.New("room name must contain at most 80 printable characters")
	}
	if q.Action == "go_to" || q.Action == "remember" {
		name := strings.TrimSpace(q.Destination)
		if name == "" || len(name) > 80 || strings.ContainsFunc(name, unicode.IsControl) {
			return errors.New("name a destination using 1 to 80 printable characters")
		}
	}
	return nil
}

func (s *Server) openCommandRobot(ctx context.Context, serial string) (robotControl, error) {
	if s.Conn == nil {
		return nil, errors.New("robot gateway is unavailable")
	}
	return twinrobot.Open(ctx, s.Conn, serial)
}

// Connecting starts observations and the same STOP/avoidance preparation used
// by the map. It never replaces another operator or invents a map alignment.
func (s *Server) ensureCommandRobot(ctx context.Context, serial string, open commandRobotOpener) (*robotSession, bool, error) {
	return s.ensureCommandRobotRecovery(ctx, serial, open, false)
}

func (s *Server) ensureCommandRobotRecovery(ctx context.Context, serial string, open commandRobotOpener, explicit bool) (*robotSession, bool, error) {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	session := s.observers[serial]
	s.mu.Unlock()
	if session != nil && !session.robot.Status().Disconnected {
		return session, false, nil
	}
	if open == nil {
		return nil, false, errors.New("robot observation reconnect is unavailable")
	}
	previous := session
	var receipts map[string]robotCommandReceipt
	published := false
	if previous != nil {
		if err := retireDisconnectedCommands(previous, explicit); err != nil {
			return nil, false, err
		}
		defer func() {
			if !published {
				previous.commandMu.Lock()
				previous.commandRetired = false
				previous.commandMu.Unlock()
			}
		}()
		previous.commandMu.Lock()
		receipts = make(map[string]robotCommandReceipt, len(previous.commandReceipts))
		for id, receipt := range previous.commandReceipts {
			receipts[id] = receipt
		}
		previous.commandMu.Unlock()
		previous.pendingMu.Lock()
		if retire, ok := previous.robot.(interface{ Retire() }); ok {
			retire.Retire()
		} else {
			previous.robot.Close()
		}
		previous.pendingMu.Unlock()
	}
	if s.Context == nil || s.Context.Err() != nil {
		return nil, false, errors.New("robot gateway is unavailable")
	}
	// The observer outlives this HTTP request after successful setup. While
	// opening, request cancellation (including Stop) also cancels its transport.
	life, cancel := context.WithCancel(s.Context)
	stopCancel := context.AfterFunc(ctx, cancel)
	robot, err := open(life, serial)
	stopped := stopCancel()
	if err != nil || !stopped || ctx.Err() != nil {
		cancel()
		if robot != nil {
			robot.Close()
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err == nil {
			err = context.Canceled
		}
		return nil, false, err
	}
	if previous != nil && explicit {
		controller, ok := robot.(robotCommander)
		if !ok || controller.ExecuteCommand(ctx, twinrobot.Command{Action: "stop"}).State != "completed" {
			cancel()
			robot.Close()
			return nil, false, errors.New("reconnected robot did not acknowledge Stop; previous command outcomes remain unresolved")
		}
	}
	session = &robotSession{robot: robot, serial: serial, commandReceipts: receipts}
	s.mu.Lock()
	if ctx.Err() != nil {
		s.mu.Unlock()
		cancel()
		robot.Close()
		return nil, false, ctx.Err()
	}
	// Old desktop sequence counters and candidate handles cannot target the
	// replacement controller. A desktop client must explicitly open it again.
	for handle, old := range s.robots {
		if old == previous {
			delete(s.robots, handle)
		}
	}
	if s.observers == nil {
		s.observers = map[string]*robotSession{}
	}
	s.observers[serial] = session
	published = true
	s.mu.Unlock()
	return session, true, nil
}

// Only a definitively dead, idle observer may be replaced. Running or unknown
// receipts are never converted to success, canceled, or a replayable empty map.
func retireDisconnectedCommands(session *robotSession, explicit bool) error {
	state := session.robot.Status()
	if !state.Disconnected || state.Moving {
		return errors.New("robot observer is not disconnected and idle")
	}
	if controller, ok := session.robot.(robotCommander); !ok || controller.CommandActive() {
		return errors.New("robot command is still active; stop it and resolve its receipt before reconnecting")
	}
	session.searchMu.Lock()
	searching := session.searchState == "searching"
	session.searchMu.Unlock()
	if searching {
		return errors.New("robot search is still active; stop it before reconnecting")
	}
	session.commandMu.Lock()
	defer session.commandMu.Unlock()
	for _, receipt := range session.commandReceipts {
		switch receipt.result.State {
		case "completed", "failed", "rejected", "canceled", "superseded":
		case "running":
			return errors.New("a robot command is still finishing; stop it and wait for its receipt before reconnecting")
		default:
			if !explicit {
				return errors.New("a prior robot command outcome is still running or unknown; explicitly connect to stop and recover observations without replaying it")
			}
		}
	}
	session.commandRetired = true
	return nil
}

type commandSetup struct{ cancel context.CancelFunc }

func (s *Server) beginCommandSetup(ctx context.Context, serial string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	setup := &commandSetup{cancel: cancel}
	s.mu.Lock()
	if s.commandSetups == nil {
		s.commandSetups = map[string]map[*commandSetup]struct{}{}
	}
	if s.commandSetups[serial] == nil {
		s.commandSetups[serial] = map[*commandSetup]struct{}{}
	}
	s.commandSetups[serial][setup] = struct{}{}
	s.mu.Unlock()
	return ctx, func() {
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.commandSetups[serial], setup)
		if len(s.commandSetups[serial]) == 0 {
			delete(s.commandSetups, serial)
		}
	}
}

func (s *Server) cancelCommandSetups(serial string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for setup := range s.commandSetups[serial] {
		setup.cancel()
	}
}

func waitCommandMeasurements(ctx context.Context, robot robotControl, idle bool) error {
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(25 * time.Millisecond)
	defer poll.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		state := robot.Status()
		if state.Disconnected {
			return errors.New("robot disconnected while starting observations")
		}
		ready, reason := state.MeasurementsReady, state.ReadinessMessage
		if idle {
			ready, reason = state.IdleMeasurementsReady, state.IdleReadinessMessage
		}
		if ready {
			return nil
		}
		if reason == "" {
			reason = "waiting for fresh odometry and LiDAR"
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("fresh measurements did not arrive within three seconds: %s", reason)
		case <-poll.C:
		}
	}
}
