package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/chunkupload"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// chunkPushResumeAttempts bounds how many times pushLayersResumingTunnelDrops
// will attempt the chunk push before giving up. Each retry costs a fresh
// QueryChunks capability probe (plus whatever per-layer QueryChunks round
// trips a genuinely mid-layer drop leaves outstanding), so a small cap keeps
// a tunnel that's actually gone for good from hanging the deploy forever,
// while still covering the common case of a single drop and resume.
const chunkPushResumeAttempts = 3

// retryableTunnelError reports whether err looks like a transport-level
// tunnel drop that reconnect-and-resume can recover from, as opposed to a
// failure the retry loop must leave alone:
//   - Unimplemented means the agent has no chunk-diff support at all; it must
//     bubble straight up so deployByChunkDiff's caller falls back to the
//     registry-push ladder instead of retrying a push that can never
//     succeed. (Same check as isUnimplementedRPCError, reused here so the two
//     stay in lockstep.)
//   - context cancellation (Canceled or DeadlineExceeded) is the user or a
//     deadline asking to stop, never a transport problem — retrying would
//     fight the cancellation instead of honoring it.
//   - imageBuildFailedError means the image never left the build stage; it
//     has nothing to do with the tunnel and reconnecting cannot help.
//
// Retryable: a gRPC Unavailable status, bare or wrapped (the transport
// reporting the tunnel is down), and an EOF-shaped stream death — io.EOF or
// io.ErrUnexpectedEOF, bare or wrapped — the shape a broken HTTP/2 stream
// surfaces as when the drop outruns the tunnel's own status framing.
func retryableTunnelError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if isImageBuildFailure(err) {
		return false
	}
	if isUnimplementedRPCError(err) {
		return false
	}
	// Mirrors isUnimplementedRPCError's unwrap-and-check-Code walk rather than
	// relying solely on status.Code's own (version-dependent) unwrapping.
	for current := err; current != nil; current = errors.Unwrap(current) {
		if status.Code(current) == codes.Unavailable {
			return true
		}
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// pushLayersResumingTunnelDrops runs pushLayersWithProgress and, when the
// connection is reconnectable (conn.Reconnect != nil — set only for cloud
// tunnels, see grpcclient.AgentConnection.Reconnect) and the failure looks
// like a tunnel drop (retryableTunnelError), reconnects and retries the push
// instead of failing the whole deploy over a mid-transfer disconnect.
//
// The retry's own QueryChunks calls see whatever the device already staged
// before the drop, so only the still-unsent bytes move again. And because
// pushLayersWithProgress builds a fresh chunkPushProgress aggregator on every
// call, the retry's own summary reports "device already has N/M chunks" —
// making the resume visible instead of silently re-diffing from zero
// (WDY-2431 tie-in).
//
// Scope is deliberately push-phase only: nothing here re-runs once
// RunContainer's Started response has been seen, so a drop after Started
// never re-triggers a redeploy. The unpack phase downstream has its own
// idle-safe keepalive (WDY-2433), so tunnel starvation there is already
// handled by a different mechanism.
//
// A stall (chunkupload.ErrStalled, from cfg's watchdog) is retried even on a
// LAN connection with no Reconnect closure at all: reconnectAgentAfterRestart
// handles both cases, the stall is remembered via rememberChunkUploadStall so
// later pushes to the same device skip straight to gzip, and the retry itself
// always uses gzip regardless of what cfg started as.
//
// Returns the *grpcclient.AgentConnection that performed the returned
// result — conn itself if the first attempt succeeded (or nothing was
// retryable), or the last reconnected connection otherwise. The caller must
// close the returned connection iff it differs from conn; connections
// reconnected away from mid-loop (neither the original conn nor the one
// finally returned) are closed here.
//
// prepareFor, when non-nil, builds the device-side image-preparation func for
// a given client. It is a factory rather than a bare imagePrepareFunc so each
// retry's PrepareImage call rides the CURRENT (possibly reconnected)
// connection instead of the one that just dropped.
func pushLayersResumingTunnelDrops(ctx context.Context, conn *grpcclient.AgentConnection, layers []localLayer, prepareFor func(agentpb.WendyContainerServiceClient) imagePrepareFunc, cfg chunkUploadConfig) (*grpcclient.AgentConnection, []*agentpb.RunContainerLayerHeader, error) {
	cur := conn
	for attempt := 1; attempt <= chunkPushResumeAttempts; attempt++ {
		var prepare imagePrepareFunc
		if prepareFor != nil {
			prepare = prepareFor(cur.ContainerService)
		}
		headers, err := pushLayersWithProgress(ctx, cur.ContainerService, layers, prepare, cfg)
		if err == nil {
			return cur, headers, nil
		}
		stalled := errors.Is(err, chunkupload.ErrStalled)
		if stalled {
			// Best effort: a memory we cannot write only means the next
			// deploy tries uncompressed again. Remembered unconditionally
			// here — including on the final attempt, whose exhausted-return
			// below would otherwise skip it (M7): a stall the CLI is about to
			// give up on is exactly the one most worth not repeating.
			_ = rememberChunkUploadStall(cfg.stallKey, time.Now())
		}
		if attempt == chunkPushResumeAttempts || !(stalled || (cur.Reconnect != nil && retryableTunnelError(err))) {
			return cur, nil, err
		}

		var (
			next *grpcclient.AgentConnection
			rerr error
		)
		if stalled {
			cliNotice("No chunk upload progress for %s; reconnecting and resending with gzip — chunks already staged on the device are skipped, and this device stays on gzip for %d days (delete <user cache dir>/wendy/chunk-upload-stalls.json or set WENDY_CHUNK_COMPRESSION=none to reset) (attempt %d/%d)...", cfg.stallTimeout, chunkStallMemoryDays, attempt+1, chunkPushResumeAttempts)
			cfg = gzipChunkUploadConfig
			next, rerr = reconnectAgentAfterRestart(ctx, cur)
		} else {
			cliNotice("Connection dropped mid-transfer (%v); reconnecting to resume — chunks already staged on the device are skipped (attempt %d/%d)...", err, attempt+1, chunkPushResumeAttempts)
			next, rerr = cur.Reconnect(ctx)
		}
		if rerr != nil {
			if stalled {
				// The reconnect failure must not swallow the stall that
				// triggered it (M3): callers (and errors.Is(err,
				// chunkupload.ErrStalled) checks) still need to see it.
				return cur, nil, fmt.Errorf("chunk upload stalled (%w), and reconnecting failed: %v", chunkupload.ErrStalled, rerr)
			}
			return cur, nil, fmt.Errorf("reconnecting to resume the chunk push: %w", rerr)
		}
		if cur != conn {
			cur.Close()
		}
		cur = next
	}
	// Unreachable: every iteration above returns before the loop can exit
	// normally (either on success, or on the final attempt's failure).
	return cur, nil, fmt.Errorf("chunk push: exhausted %d attempts", chunkPushResumeAttempts)
}
