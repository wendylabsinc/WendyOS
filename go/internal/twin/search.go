package twin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
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
	session.searchTurns, session.searchMoves = 0, 0
	session.searchPhase = "matching"
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
	lastMatchIssue, blockedReason := "", ""
	blockedHeadings := map[int]bool{}
	details := func() string {
		parts := []string{lastReason}
		if lastMatchIssue != "" {
			parts = append(parts, lastMatchIssue)
		}
		if len(blockedHeadings) > 0 {
			heading := "headings"
			if len(blockedHeadings) == 1 {
				heading = "heading"
			}
			parts = append(parts, fmt.Sprintf("Nearby moves were blocked at %d %s: %s.", len(blockedHeadings), heading, strings.TrimSuffix(blockedReason, ".")))
		}
		return strings.Join(parts, " ")
	}
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
	fail := func(message string) {
		log.Printf("localization stopped: %s", message)
		session.robot.Stop(root + 999999)
		finish("failed", message, lastReport)
	}
	phase := func(value string) {
		session.searchMu.Lock()
		defer session.searchMu.Unlock()
		if session.searchRoot == root {
			session.searchPhase = value
		}
	}
	moves := 0
	attempted := map[int]bool{}
	waitingForScan := false
	recoveringMotion := false
	var scanAfter int64
	awaitObservations := func(turn int, message string, retryMove bool) {
		waitingForScan = true
		scanAfter = session.robot.Status().ScanReceivedUnixNanos
		settledAfter = time.Now().Add(2 * time.Second)
		if retryMove {
			delete(attempted, turn)
		}
		log.Printf("localization waiting turn=%d moves=%d: %s", turn, moves, message)
		session.searchMu.Lock()
		defer session.searchMu.Unlock()
		if session.searchRoot == root {
			session.searchPhase = "waiting"
			session.searchMessage = message
		}
	}
	maxTurns := twinrobot.LocalizationTurnCount
	if input.Explore {
		// Inspect each heading at most once; returning to the initial heading
		// adds no new viewpoint when all translations were blocked.
		maxTurns--
	}
searchLoop:
	for turn := 0; turn <= maxTurns; {
		for {
			if !wait() {
				fail("Position search ended. " + details())
				return
			}
			state := session.robot.Status()
			if state.Disconnected {
				fail(state.Message)
				return
			}
			if recoveringMotion && !state.MotionRecoveryAllowed {
				fail("Discovery cannot resume automatically after control or Stop confirmation was lost.")
				return
			}
			if state.MeasurementsReady && !state.Moving && !time.Now().Before(settledAfter) &&
				(!waitingForScan || state.ScanReceivedUnixNanos > scanAfter) {
				waitingForScan = false
				recoveringMotion = false
				break
			}
		}
		if ctx.Err() != nil {
			fail("Position search ended. " + details())
			return
		}
		phase("matching")
		sequence++
		report, err := session.robot.FuseLocalization(sequence, input)
		// The initial camera hint is checked against fresh paired odometry by
		// FuseLocalization. Never reuse that captured view after a search turn.
		input.Visual = nil
		if err != nil {
			lastMatchIssue = err.Error()
			if errors.Is(err, context.DeadlineExceeded) {
				lastMatchIssue = "Matching the latest observations exceeded its time budget."
			}
		} else {
			lastMatchIssue = ""
			lastReport = &report
			if report.Message != "" {
				lastReason = report.Message
			}
		}
		log.Printf("localization turn=%d candidates=%d accepted=%d rejected=%d reason=%q", turn, len(report.Candidates), report.Accepted, report.Rejected, details())
		session.searchMu.Lock()
		if session.searchRoot == root {
			session.searchMessage = details()
			if lastReport != nil {
				session.searchReport, _ = json.Marshal(lastReport)
			}
		}
		session.searchMu.Unlock()
		if err == nil && len(report.Candidates) > 0 {
			session.pendingMu.Lock()
			// A reset or newer request can finish while matching is in flight.
			// Never publish a candidate from the discarded alignment afterward.
			if ctx.Err() != nil || sequence <= session.pendingSequence {
				session.pendingMu.Unlock()
				if ctx.Err() != nil {
					fail("Position search ended. " + details())
				}
				return
			}
			session.pending = report
			session.pendingFrame = input.Frame
			session.pendingSequence = sequence
			session.pendingRoot = root
			session.pendingMu.Unlock()
			finish("complete", report.Message, &report)
			return
		}
		if report.WaitingForObservations {
			awaitObservations(turn, report.Message, false)
			continue
		}
		// Exhausting motion never starts another spin. Keep matching fresh
		// measurements at rest for the remainder of this bounded session.
		if input.Explore && moves == 3 {
			awaitObservations(turn, "Movement limit reached. Checking new observations while Go2 stays still.", false)
			continue
		}
		if input.Explore && !attempted[turn] {
			attempted[turn] = true
			if ctx.Err() != nil {
				fail("Position search ended. " + details())
				return
			}
			sequence++
			err := session.robot.DiscoveryStep(sequence, root)
			if errors.Is(err, twinrobot.ErrObservationUnavailable) {
				awaitObservations(turn, err.Error(), true)
				continue
			}
			if errors.Is(err, twinrobot.ErrDiscoveryBlocked) {
				blockedHeadings[turn] = true
				blockedReason = err.Error()
				log.Printf("discovery turn=%d move skipped: %v", turn, err)
			}
			if err != nil && !errors.Is(err, twinrobot.ErrDiscoveryBlocked) {
				fail(err.Error())
				return
			}
			if err == nil {
				moves++
				phase("advancing")
				session.searchMu.Lock()
				if session.searchRoot == root {
					session.searchMoves = moves
					session.searchMessage = "Moving to a nearby viewpoint on observed floor"
				}
				session.searchMu.Unlock()
				for {
					if !wait() {
						fail("Position search ended. " + details())
						return
					}
					state := session.robot.Status()
					if state.Disconnected || state.DiscoveryState == 3 {
						if !state.Disconnected && state.MotionRecoveryAllowed {
							recoveringMotion = true
							awaitObservations(turn, state.Message, true)
							continue searchLoop
						}
						fail(state.Message)
						return
					}
					if state.DiscoveryState == 2 && !state.Moving {
						break
					}
				}
				settledAfter = time.Now().Add(time.Second)
				phase("settling")
				continue // match the translated view before turning again
			}
		}
		if turn == maxTurns {
			message := "Checking new observations while Go2 stays still. " + lastReason
			if input.Explore && blockedReason != "" {
				message = "Waiting for observed clear floor before another nearby move."
			}
			awaitObservations(turn, message, input.Explore)
			continue
		}
		if ctx.Err() != nil {
			fail("Position search ended. " + details())
			return
		}
		phase("turning")
		sequence++
		if err = session.robot.Look(sequence, direction); err != nil {
			if errors.Is(err, twinrobot.ErrObservationUnavailable) || errors.Is(err, twinrobot.ErrDiscoveryBlocked) {
				awaitObservations(turn, err.Error(), input.Explore)
				continue
			}
			fail(err.Error())
			return
		}
		session.searchMu.Lock()
		if session.searchRoot == root {
			session.searchTurns = turn + 1
			session.searchMessage = "Turning and matching on Linux"
		}
		session.searchMu.Unlock()
		turn++
		for {
			if !wait() {
				fail("Position search ended. " + details())
				return
			}
			state := session.robot.Status()
			if state.LookState == 3 || state.Disconnected {
				if !state.Disconnected && state.MotionRecoveryAllowed {
					recoveringMotion = true
					awaitObservations(turn, state.Message, true)
					continue searchLoop
				}
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
