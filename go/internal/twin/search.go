package twin

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
	"log"
	"time"
)

func (s *Server) startSearch(session *robotSession, q twinrobot.Request) error {
	var input twinrobot.SiteFusionRequest
	if err := json.Unmarshal(q.Fusion, &input); err != nil {
		return err
	}
	if err := twinrobot.ValidateSearchGeometry(input); err != nil {
		return err
	}
	room, err := s.Store.Frame(input.Frame)
	if err != nil {
		return err
	}
	input.Reference = room.References[session.serial]
	// A camera contribution is valid only at its captured pose, not after a turn.
	input.Visual = nil
	ctx, cancel := context.WithTimeout(s.Context, 5*time.Minute)
	session.searchMu.Lock()
	if q.Sequence <= session.searchRoot {
		session.searchMu.Unlock()
		cancel()
		return errors.New("search superseded")
	}
	session.searchCancel = cancel
	session.searchRoot = q.Sequence
	session.searchState = "searching"
	session.searchMessage = "Checking robot observations"
	session.searchTurns = 0
	session.searchReport = nil
	session.searchMu.Unlock()
	go s.search(ctx, cancel, session, q.Sequence, input)
	return nil
}
func (s *Server) search(ctx context.Context, cancel context.CancelFunc, session *robotSession, root uint64, input twinrobot.SiteFusionRequest) {
	defer cancel()
	sequence := root
	direction := 0
	settledAfter := time.Time{}
	var lastReport *twinrobot.SiteFusionReport
	lastReason := "No reliable position match"
	finish := func(state, message string, report *twinrobot.SiteFusionReport) {
		session.searchMu.Lock()
		defer session.searchMu.Unlock()
		if session.searchRoot != root {
			return
		}
		session.searchState = state
		session.searchMessage = message
		if report != nil {
			session.searchReport, _ = json.Marshal(report)
		}
	}
	wait := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
			return true
		}
	}
	fail := func(message string) { session.robot.Stop(root + 999999); finish("failed", message, lastReport) }
	for turn := 0; turn <= 12; turn++ {
		for {
			if !wait() {
				fail("Position search ended")
				return
			}
			state := session.robot.Status()
			if state.Disconnected {
				fail(state.Message)
				return
			}
			if state.MeasurementsReady && !state.Moving && !time.Now().Before(settledAfter) {
				break
			}
		}
		sequence++
		report, err := session.robot.FuseLocalization(sequence, input)
		if err != nil {
			lastReason = err.Error()
		} else {
			lastReport = &report
			if report.Message != "" {
				lastReason = report.Message
			}
		}
		log.Printf("localization turn=%d candidates=%d accepted=%d rejected=%d reason=%q", turn, len(report.Candidates), report.Accepted, report.Rejected, lastReason)
		session.searchMu.Lock()
		if session.searchRoot == root {
			session.searchMessage = lastReason
			if lastReport != nil {
				session.searchReport, _ = json.Marshal(lastReport)
			}
		}
		session.searchMu.Unlock()
		if err == nil && len(report.Candidates) > 0 {
			session.pendingMu.Lock()
			session.pending = report
			session.pendingFrame = input.Frame
			session.pendingSequence = sequence
			session.pendingRoot = root
			session.pendingMu.Unlock()
			finish("complete", report.Message, &report)
			return
		}
		if turn == 12 {
			fail("No reliable position match after a full rotation. " + lastReason)
			return
		}
		if ctx.Err() != nil {
			fail("Position search ended")
			return
		}
		sequence++
		if err = session.robot.Look(sequence, direction); err != nil {
			fail(err.Error())
			return
		}
		session.searchMu.Lock()
		if session.searchRoot == root {
			session.searchTurns = turn + 1
			session.searchMessage = "Turning and matching on Linux"
		}
		session.searchMu.Unlock()
		for {
			if !wait() {
				fail("Position search ended")
				return
			}
			state := session.robot.Status()
			if state.LookState == 3 || state.Disconnected {
				fail(state.Message)
				return
			}
			if state.LookState == 2 && !state.Moving {
				if direction == 0 && (state.LookDirection == 1 || state.LookDirection == -1) {
					direction = state.LookDirection
				}
				settledAfter = time.Now().Add(time.Second)
				break
			}
		}
	}
}
