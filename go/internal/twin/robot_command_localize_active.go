package twin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
)

const activeLocalizationLimit = 5 * time.Minute

type activeSearchOwner interface {
	Status() twinrobot.SiteRobotSnapshot
	FuseLocalization(uint64, twinrobot.SiteFusionRequest) (twinrobot.SiteFusionReport, error)
	Look(uint64, int) error
	DiscoveryStep(uint64, uint64) error
	ConfirmLocalization(uint64, uint64, string, int) error
	Stop(uint64)
	Check() error
	Close()
}
type activeSearchOwnerFactory func(context.Context, robotControl) (activeSearchOwner, error)

func beginLocalizationSearchOwner(ctx context.Context, robot robotControl) (activeSearchOwner, error) {
	engine, ok := robot.(interface {
		BeginLocalizationSearch(context.Context) (*twinrobot.LocalizationSearchOwner, error)
	})
	if !ok {
		return nil, errors.New("This controller needs the movement-assisted localization update.")
	}
	return engine.BeginLocalizationSearch(ctx)
}

// The desktop search sees the same operations but private logical sequences.
// The owner performs all checks under the engine lock and makes an old search's
// cleanup Stop harmless to a newer desktop/chat operator.
type ownedLocalizationRobot struct {
	robotControl
	activeSearchOwner
	checkMu  sync.Mutex
	checkErr error
}

func (r *ownedLocalizationRobot) Status() twinrobot.SiteRobotSnapshot {
	state := r.activeSearchOwner.Status()
	if state.Disconnected {
		_ = r.Check()
	}
	return state
}

// Preserve the ownership failure before search's cleanup closes the owner.
// Otherwise Close's context cancellation would hide a newer operator command.
func (r *ownedLocalizationRobot) Check() error {
	r.checkMu.Lock()
	defer r.checkMu.Unlock()
	if r.checkErr == nil {
		r.checkErr = r.activeSearchOwner.Check()
	}
	return r.checkErr
}
func (r *ownedLocalizationRobot) FuseLocalization(seq uint64, input twinrobot.SiteFusionRequest) (twinrobot.SiteFusionReport, error) {
	return r.activeSearchOwner.FuseLocalization(seq, input)
}
func (r *ownedLocalizationRobot) Look(seq uint64, direction int) error {
	return r.activeSearchOwner.Look(seq, direction)
}
func (r *ownedLocalizationRobot) DiscoveryStep(seq, root uint64) error {
	return r.activeSearchOwner.DiscoveryStep(seq, root)
}
func (r *ownedLocalizationRobot) ConfirmLocalization(seq, match uint64, frame string, index int) error {
	return r.activeSearchOwner.ConfirmLocalization(seq, match, frame, index)
}
func (r *ownedLocalizationRobot) Stop(seq uint64) {
	_ = r.Check()
	r.activeSearchOwner.Stop(seq)
}
func (r *ownedLocalizationRobot) Close() { r.activeSearchOwner.Close() }

func (s *Server) prepareActiveLocalization(requestContext context.Context, session *robotSession, q robotCommandRequest, controller robotCommander, begin activeSearchOwnerFactory) (twinrobot.CommandResult, func()) {
	reject := func(message string) (twinrobot.CommandResult, func()) {
		return twinrobot.CommandResult{State: "rejected", Message: message, Snapshot: session.robot.Status()}, nil
	}
	if err := requestContext.Err(); err != nil {
		return twinrobot.CommandResult{State: "canceled", Message: err.Error(), Snapshot: session.robot.Status()}, nil
	}
	if s.Context == nil || s.Context.Err() != nil {
		return reject("The robot gateway is unavailable.")
	}
	if controller.CommandActive() || session.robot.Status().Moving {
		return reject("A movement command is active. Resolve or stop it before starting movement-assisted localization.")
	}
	s.mu.Lock()
	pendingSetup := len(s.commandSetups[session.serial]) > 1
	s.mu.Unlock()
	session.commandMu.Lock()
	pendingCommand := false
	for id, receipt := range session.commandReceipts {
		if id != q.RequestID && receipt.result.State == "running" {
			pendingCommand = true
			break
		}
	}
	session.commandMu.Unlock()
	if pendingSetup || pendingCommand {
		return reject("Another robot command is pending. Resolve or stop it before starting movement-assisted localization.")
	}
	room, err := s.localizationRoom(q.Destination)
	if err != nil {
		return reject(err.Error())
	}
	geometry := room.LocalizationGeometry
	input := twinrobot.SiteFusionRequest{Frame: geometry.Frame, Architecture: geometry.Architecture, Furniture: geometry.Furniture, Reference: room.References[session.serial], Explore: true}
	if err := twinrobot.ValidateSearchGeometry(input); err != nil {
		return reject(err.Error())
	}
	// A successful initial response acknowledges a bounded background search.
	// Transport recovery can only read its receipt, never restart the movement.
	ownerContext, cancelOwner := context.WithTimeout(s.Context, activeLocalizationLimit)
	stopRequestCancel := context.AfterFunc(requestContext, cancelOwner)
	owner, err := begin(ownerContext, session.robot)
	if err != nil {
		stopRequestCancel()
		cancelOwner()
		return reject(err.Error())
	}
	if requestContext.Err() != nil {
		stopRequestCancel()
		cancelOwner()
		owner.Close()
		return twinrobot.CommandResult{State: "canceled", Message: requestContext.Err().Error(), Snapshot: session.robot.Status()}, nil
	}
	session.searchMu.Lock()
	if session.searchState == "searching" {
		session.searchMu.Unlock()
		stopRequestCancel()
		cancelOwner()
		owner.Close()
		return reject("A position search is already active.")
	}
	session.searchRoot++
	root := session.searchRoot
	session.searchState, session.searchPhase, session.searchMessage = "searching", "matching", "Starting movement-assisted localization"
	session.searchTurns, session.searchMoves = 0, 0
	session.searchCancel = cancelOwner
	session.searchMu.Unlock()
	initial := twinrobot.CommandResult{State: "running", Message: "Movement-assisted localization started: matching current observations, then bounded turns and nearby viewpoints as needed. Limit five minutes, at most six turns and three viewpoint moves. Stop cancels it; a running receipt is not confirmed alignment.", Snapshot: session.robot.Status()}
	launch := func() {
		// Disarm synchronously before the HTTP handler returns and its context is
		// normally canceled. A canceled unacknowledged start never launches motion.
		if !stopRequestCancel() || requestContext.Err() != nil {
			cancelOwner()
			owner.Close()
			s.finishActiveLocalization(session, q, root, twinrobot.CommandResult{State: "canceled", Message: "Movement-assisted localization was canceled before startup.", Snapshot: session.robot.Status()})
			return
		}
		go s.runActiveLocalization(ownerContext, cancelOwner, session, q, root, input, owner)
	}
	return initial, launch
}

func (s *Server) runActiveLocalization(ctx context.Context, cancel context.CancelFunc, session *robotSession, q robotCommandRequest, root uint64, input twinrobot.SiteFusionRequest, owner activeSearchOwner) {
	defer cancel()
	// Search's cancel-on-completion applies only to its loop. Its separate owner
	// remains valid long enough to confirm the unique, corroborated candidate.
	searchContext, cancelSearch := context.WithCancel(ctx)
	defer cancelSearch()
	job := &robotSession{robot: &ownedLocalizationRobot{robotControl: session.robot, activeSearchOwner: owner}, serial: session.serial, searchRoot: 1, searchState: "searching", searchPhase: "matching"}
	done := make(chan struct{})
	go func() { defer close(done); s.search(searchContext, cancelSearch, job, 1, input) }()
	progress := time.NewTicker(500 * time.Millisecond)
	defer progress.Stop()
	for {
		select {
		case <-progress.C:
			job.searchMu.Lock()
			message, phase, turns, moves := job.searchMessage, job.searchPhase, job.searchTurns, job.searchMoves
			job.searchMu.Unlock()
			session.searchMu.Lock()
			if session.searchRoot == root {
				session.searchMessage, session.searchPhase = message, phase
				session.searchTurns, session.searchMoves = turns, moves
			}
			session.searchMu.Unlock()
			updateActiveReceipt(session, q, twinrobot.CommandResult{State: "running", Message: fmt.Sprintf("Movement-assisted localization: %s (%d turns, %d viewpoint moves).", message, turns, moves), Snapshot: session.robot.Status()})
		case <-done:
			result := s.concludeActiveLocalization(ctx, job, input, owner)
			owner.Close()
			result.Snapshot = session.robot.Status()
			s.finishActiveLocalization(session, q, root, result)
			return
		}
	}
}

func (s *Server) concludeActiveLocalization(ctx context.Context, job *robotSession, input twinrobot.SiteFusionRequest, owner activeSearchOwner) twinrobot.CommandResult {
	result := func(state, message string) twinrobot.CommandResult {
		return twinrobot.CommandResult{State: state, Message: message}
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return result("failed", "Movement-assisted localization reached its five-minute limit without confirming an alignment.")
		}
		return result("canceled", "Movement-assisted localization was canceled.")
	}
	if err := job.robot.(*ownedLocalizationRobot).Check(); errors.Is(err, twinrobot.ErrLocalizationSearchSuperseded) {
		return result("superseded", err.Error())
	}
	job.searchMu.Lock()
	state, message := job.searchState, job.searchMessage
	job.searchMu.Unlock()
	if state != "complete" {
		return result("failed", message)
	}
	job.pendingMu.Lock()
	report, frame, match := job.pending, job.pendingFrame, job.pendingSequence
	job.pendingMu.Unlock()
	selected := report.Tracking.Selected
	if report.WaitingForObservations || len(report.Candidates) != 1 || selected < 0 || selected >= len(report.Tracking.Fits) || !report.Tracking.Fits[selected].Valid || frame != input.Frame {
		return result("rejected", "The search did not produce one reliable position. No alignment was confirmed.")
	}
	err := s.Store.withFrame(frame, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := owner.Check(); err != nil {
			return err
		}
		return owner.ConfirmLocalization(match+1, match, frame, 0)
	})
	if err != nil {
		if ctx.Err() != nil {
			return result("canceled", ctx.Err().Error())
		}
		if errors.Is(err, twinrobot.ErrLocalizationSearchSuperseded) {
			return result("superseded", err.Error())
		}
		return result("rejected", "The candidate could not be confirmed: "+err.Error())
	}
	snapshot := job.robot.Status()
	if !snapshot.Aligned || snapshot.Frame != frame {
		return result("failed", "The controller did not confirm the selected room alignment.")
	}
	message = "Go2's position was confirmed after movement-assisted localization."
	if err := s.Store.SaveReferences(frame, job.serial, report.Reference); err != nil {
		message += " The alignment is active, but its reference observations could not be saved: " + err.Error()
	}
	return result("completed", message)
}

func updateActiveReceipt(session *robotSession, q robotCommandRequest, result twinrobot.CommandResult) {
	session.commandMu.Lock()
	defer session.commandMu.Unlock()
	if old, ok := session.commandReceipts[q.RequestID]; ok && old.request == q {
		old.result = result
		session.commandReceipts[q.RequestID] = old
	}
}
func (s *Server) finishActiveLocalization(session *robotSession, q robotCommandRequest, root uint64, result twinrobot.CommandResult) {
	session.searchMu.Lock()
	if session.searchRoot == root {
		session.searchState = "failed"
		if result.State == "completed" {
			session.searchState = "complete"
		}
		session.searchMessage = result.Message
		session.searchPhase = ""
		session.searchCancel = nil
	}
	session.searchMu.Unlock()
	updateActiveReceipt(session, q, result)
}
