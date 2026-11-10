package commands

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	clitimesync "github.com/wendylabsinc/wendy/go/internal/cli/timesync"
	"github.com/wendylabsinc/wendy/go/internal/shared/terminaloutput"
	"github.com/wendylabsinc/wendy/go/internal/shared/timefmt"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// clockSkewThreshold is the minimum amount a device clock must lag the host
// clock before we relay a time proof. Large enough to ignore ordinary drift
// and round-trip noise; small enough to catch any meaningfully-wrong clock,
// including the 1970-epoch case from issue #1171.
const clockSkewThreshold = 2 * time.Minute

// clockFixTimeout bounds the whole detect-and-fix exchange so a flaky link
// never stalls a command.
const clockFixTimeout = 5 * time.Second

// clockNowFn is indirected for tests.
var clockNowFn = time.Now

// fetchProofPacketFn is indirected for tests.
var fetchProofPacketFn = func(ctx context.Context) ([]byte, error) {
	pkt, _, err := clitimesync.FetchProofPacket(ctx)
	return pkt, err
}

// maybeFixClock detects whether the connected device's clock lags the host
// clock by more than clockSkewThreshold and, if so, relays a verified
// Roughtime proof so the device advances its own clock. The host wall clock is
// used only to decide whether to relay — never sent as authoritative time.
//
// Best-effort: every failure path is silent (debug-only) and never affects the
// command. The whole exchange is bounded by clockFixTimeout.
func maybeFixClock(ctx context.Context, conn *grpcclient.AgentConnection) {
	if conn == nil || conn.TimeSyncService == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, clockFixTimeout)
	defer cancel()

	resp, err := conn.TimeSyncService.GetClock(ctx, &agentpbv2.GetClockRequest{})
	if err != nil {
		debugClock("GetClock failed: %v", err)
		return
	}
	skew := clockNowFn().Sub(time.Unix(0, resp.GetUnixNanos()))
	if skew <= clockSkewThreshold {
		return
	}

	pkt, err := fetchProofPacketFn(ctx)
	if err != nil {
		debugClock("fetch time proof failed: %v", err)
		return
	}
	syncResp, err := conn.TimeSyncService.SyncClock(ctx, &agentpbv2.SyncClockRequest{Proof: pkt})
	if err != nil {
		debugClock("SyncClock failed: %v", err)
		return
	}
	if syncResp.GetApplied() && !jsonOutput {
		fmt.Fprintf(terminaloutput.Stderr, "⏱  Device clock was %s behind — synchronized via Roughtime.\n", timefmt.Skew(skew))
	}
}

// debugClock logs only when WENDY_TLS_DEBUG is set, matching autoSyncTimeAndRetry.
func debugClock(format string, args ...any) {
	if os.Getenv("WENDY_TLS_DEBUG") != "" {
		fmt.Fprintf(terminaloutput.Stderr, "[clock] "+format+"\n", args...)
	}
}
