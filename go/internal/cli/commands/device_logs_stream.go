package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// logStreamReceiver is the part of a StreamLogs client stream that
// consumeLogStream reads. Tests substitute a scripted fake.
type logStreamReceiver interface {
	Recv() (*agentpb.StreamLogsResponse, error)
}

// How `wendy device logs --no-follow` decides the agent's history replay is
// over. Vars so tests can shrink them.
//
// Agents that honour the request's NoFollow (#2103) close the stream once the
// replay is sent, which ends the read at once. Older agents ignore it and send
// no end-of-history marker. What every agent does do is send its whole replay
// first — the on-disk batches for --tail, then its in-memory recent batches — all
// back-to-back and flagged IsHistory, before entering the live loop, whose
// frames (new logs, and since WDY-2912 an empty heartbeat after 15 s of
// quiet) are never flagged. So:
//
//   - the first frame without IsHistory proves the replay is over; and
//   - a pause after a replayed frame is taken to mean the same. The replay is
//     a single burst (sub-millisecond gaps observed from a Jetson over USB),
//     so the pause needed is short: noFollowIdleGap, stretched to 3x the
//     longest pause seen so far within the replay when the link is slower
//     (LTE, a cloud tunnel), up to noFollowMaxIdleGap — see adaptiveIdleGap.
//     This can still cut history short on a link that stalls longer than
//     that mid-burst, so the command notes when a replay ended this way. It
//     is the trade-off for not waiting up to 15 s for a heartbeat that
//     pre-WDY-2912 agents never send.
//
// noFollowFirstFrameWait covers the agent reading its on-disk buffer before
// the first replayed frame (it scans segments newest-first until it has N
// matches, which can take seconds for a quiet app on a large buffer). When
// there is no history at all, it is how long --no-follow waits to conclude so.
var (
	noFollowFirstFrameWait = 10 * time.Second
	noFollowIdleGap        = 1500 * time.Millisecond
	noFollowMaxIdleGap     = 10 * time.Second
)

// agentRecentLogBatches is how many recent log batches a device agent keeps in
// memory and replays when no --tail is given (defaultMaxCachedLogs in the Go
// agent, maxCachedLogs in the Swift one; counted device-wide, before the app
// filter). It is the most a replay without --tail can hold.
const agentRecentLogBatches = 20

// adaptiveIdleGap is how long --no-follow waits after a replayed frame before
// taking the replay as over, given the longest pause seen so far between two
// replayed frames: 3x that pause, but at least noFollowIdleGap and at most
// noFollowMaxIdleGap (under the agents' 15 s heartbeat, which ends a replay
// definitively anyway).
func adaptiveIdleGap(longestPause time.Duration) time.Duration {
	return max(noFollowIdleGap, min(3*longestPause, noFollowMaxIdleGap))
}

// logReplayEnd says why a --no-follow read stopped.
type logReplayEnd int

const (
	replayEndedStream    logReplayEnd = iota // the agent closed the stream
	replayEndedCancelled                     // ctx was cancelled (Ctrl-C)
	replayEndedLive                          // a frame without IsHistory: the replay is definitely over
	replayEndedIdle                          // no frame for the idle gap after a replayed one
	replayEndedNoFrames                      // nothing at all within noFollowFirstFrameWait
)

// logReplayResult describes how a --no-follow read ended.
type logReplayResult struct {
	end     logReplayEnd
	history int           // replayed frames passed to handle
	idleGap time.Duration // the idle gap in force when it ended (replayEndedIdle)
}

// noFollowHint explains a --no-follow run that printed nothing, or notes one
// that ended on the idle-gap guess rather than a definitive live frame with
// fewer batches than it could hold; it returns "" when there is nothing to say. Without it an empty result, exit 0,
// reads as "this app has no logs" when the agent may simply not have
// replayed any.
func noFollowHint(res logReplayResult, tail int32) string {
	if res.end == replayEndedIdle && res.history > 0 {
		// A quiet app always ends on the gap, so only mention it when the
		// replay came up short of what it could hold: --tail N, or the
		// agent's cache without it. (Short can also just mean the app has
		// fewer stored logs; the note says "may".)
		replayMax := agentRecentLogBatches
		if tail > 0 {
			replayMax = int(tail)
		}
		if res.history < replayMax {
			return fmt.Sprintf("Replay ended after %s without new logs, with %d of up to %d batches; on a slow connection it may have been cut short.", res.idleGap, res.history, replayMax)
		}
		return ""
	}
	if res.history > 0 || res.end == replayEndedCancelled {
		return ""
	}
	if tail <= 0 {
		return "No log history received. Pass --tail N to replay the last N stored log batches; " +
			"device agents released before 2026-08-19 replay history only with --tail."
	}
	msg := "No log history received"
	if res.end == replayEndedNoFrames {
		msg += fmt.Sprintf(" within %s", noFollowFirstFrameWait)
	}
	return msg + ": the app may have no stored logs matching the filters, the device may still be reading them, " +
		"or its agent predates log replay (released before 2026-05-22)."
}

// consumeLogStream reads StreamLogs frames and passes each one to handle.
//
// With follow it runs until the stream ends. Without follow it returns once
// the history replay is over (see noFollowIdleGap) and only hands replayed
// frames to handle; the result says how the replay ended. In both modes a
// cancelled ctx — Ctrl-C — is a clean exit rather than a "receiving logs:
// ... Canceled" error.
//
// ctx must be the context the stream was opened with, and the caller must
// cancel it after consumeLogStream returns: that is what unblocks the
// background Recv on the !follow path.
func consumeLogStream(ctx context.Context, stream logStreamReceiver, follow bool, handle func(*agentpb.StreamLogsResponse)) (logReplayResult, error) {
	if follow {
		for {
			resp, err := stream.Recv()
			if err != nil {
				return logReplayResult{}, logStreamEndErr(ctx, err)
			}
			handle(resp)
		}
	}

	type frame struct {
		resp *agentpb.StreamLogsResponse
		err  error
	}
	frames := make(chan frame)
	go func() {
		for {
			resp, err := stream.Recv()
			select {
			case frames <- frame{resp, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var res logReplayResult
	var lastFrame time.Time
	var longestPause time.Duration
	idle := time.NewTimer(noFollowFirstFrameWait)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			res.end = replayEndedCancelled
			return res, nil
		case <-idle.C:
			res.end = replayEndedIdle
			if res.history == 0 {
				res.end = replayEndedNoFrames
			}
			return res, nil
		case f := <-frames:
			if f.err != nil {
				res.end = replayEndedStream
				if ctx.Err() != nil {
					res.end = replayEndedCancelled
				}
				return res, logStreamEndErr(ctx, f.err)
			}
			if !f.resp.GetIsHistory() {
				res.end = replayEndedLive
				return res, nil
			}
			handle(f.resp)
			now := time.Now()
			if res.history > 0 {
				longestPause = max(longestPause, now.Sub(lastFrame))
			}
			lastFrame = now
			res.history++
			res.idleGap = adaptiveIdleGap(longestPause)
			idle.Reset(res.idleGap)
		}
	}
}

// logStreamEndErr maps the error that ended a log stream to the command's
// result. The agent closing the stream (EOF) and the user interrupting it
// (ctx cancelled) are normal ends; anything else is a real failure.
func logStreamEndErr(ctx context.Context, err error) error {
	if errors.Is(err, io.EOF) || ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("receiving logs: %w", err)
}
