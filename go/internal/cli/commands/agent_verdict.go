package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// agentVerdict is the agent supervisor's word on one uploaded agent.
type agentVerdict struct {
	// Supervised is false on a device without the supervisor: nothing to wait for.
	Supervised bool
	Approved   bool
	// Running is the version the device runs now, and Role where it came from.
	Running string
	Role    string
	Reason  string
}

// agentRejectedError lets each caller choose to stop or to go on with the
// agent the device fell back to.
type agentRejectedError struct{ v agentVerdict }

// AgentVerdictPrefix starts every message that reports the device's verdict
// on a new agent; it is the device's own text, not a connection error.
const AgentVerdictPrefix = "the new agent failed its checks: "

func (e *agentRejectedError) Error() string {
	from := "the OS image"
	if e.v.Role != "image" {
		from = "/data"
	}
	return fmt.Sprintf(AgentVerdictPrefix+"%s; the device went back to agent %s from %s", e.v.Reason, e.v.Running, from)
}

// checkRefusalPrefix starts the agent's refusal of an upload that failed the
// supervisor's checks before anything switched (supervised_update.go).
const checkRefusalPrefix = "new agent failed its checks: "

// agentCheckRefusedError: the device refused the upload outright and still
// runs the agent it had, so a routine update can go on without it.
type agentCheckRefusedError struct{ reason string }

func (e *agentCheckRefusedError) Error() string {
	return AgentVerdictPrefix + e.reason
}

// agentUpdatesPausedError: the supervisor takes no agent update right now, while
// updates are paused or a trial runs. The device keeps its agent.
type agentUpdatesPausedError struct{ reason string }

func (e *agentUpdatesPausedError) Error() string { return e.reason }

// agentUpdatesPausedPrefixes start the agent's refusals in refuseUpdates
// (supervised_update.go).
var agentUpdatesPausedPrefixes = []string{"agent updates are off", "an agent update is being verified"}

var (
	// Covers a hung trial's deadline, its stop and the sweep of its leftovers,
	// before the previous agent starts.
	agentVerdictTimeout = 7 * time.Minute
	agentVerdictPoll    = 2 * time.Second
	// An agent that staged the upload names it until it exits, so one that
	// keeps naming another never got it; the grace covers its upload check.
	agentUnknownUploadGrace = 30 * time.Second
)

// awaitAgentVerdict waits for the supervisor's verdict on uploadedSHA. Each poll
// is also the remote confirm the trial waits for, so it uses the upload's path.
func awaitAgentVerdict(ctx context.Context, conn *grpcclient.AgentConnection, reconnect func(context.Context) (*grpcclient.AgentConnection, error), uploadedSHA string, waiting func()) (agentVerdict, *grpcclient.AgentConnection, error) {
	ctx, cancel := context.WithTimeout(ctx, agentVerdictTimeout)
	defer cancel()
	var last agentVerdict
	var unknownSince time.Time
	announced := false
	for {
		if conn != nil {
			callCtx, callCancel := context.WithTimeout(ctx, 10*time.Second)
			resp, err := conn.AgentService.GetAgentVersion(callCtx, &agentpb.GetAgentVersionRequest{})
			callCancel()
			switch {
			case err != nil:
				_ = conn.Close()
				conn = nil
			case resp.GetSupervision() == nil:
				return agentVerdict{Running: resp.GetVersion()}, conn, nil
			default:
				sup := resp.GetSupervision()
				last = agentVerdict{Supervised: true, Running: resp.GetVersion(), Role: sup.GetRole(), Reason: sup.GetReason()}
				// A verdict about an earlier upload is not this one's.
				switch {
				case sup.GetUpdateSha256() == uploadedSHA:
					unknownSince = time.Time{}
					switch sup.GetUpdateState() {
					case "approved":
						// An earlier approval of this sha says nothing while another binary runs.
						if bin := resp.GetBinarySha256(); bin != "" && bin != uploadedSHA {
							break
						}
						last.Approved = true
						return last, conn, nil
					case "rejected":
						return last, conn, &agentRejectedError{v: last}
					}
				case unknownSince.IsZero():
					unknownSince = time.Now()
				case time.Since(unknownSince) >= agentUnknownUploadGrace:
					return last, conn, fmt.Errorf("the device has no record of the new agent, so the upload did not reach it (did the device restart?); it runs %s. Run the update again", last.Running)
				}
				if !announced {
					announced = true
					waiting()
				}
			}
		}
		if err := sleepContext(ctx, agentVerdictPoll); err != nil {
			return last, conn, fmt.Errorf("the device did not decide on the new agent within %s; it runs %s", agentVerdictTimeout, last.Running)
		}
		if conn == nil {
			if c, err := reconnect(ctx); err == nil {
				conn = c
			}
		}
	}
}

// reportAgentVerdict waits for the verdict on the agent with sha, re-dialling
// the device conn reached (never discovery, which could pick another device);
// announce tells the user the check takes time.
func reportAgentVerdict(ctx context.Context, conn *grpcclient.AgentConnection, sha string, announce func()) (agentVerdict, *grpcclient.AgentConnection, error) {
	same := conn
	reconnect := func(ctx context.Context) (*grpcclient.AgentConnection, error) {
		return reconnectAgentAfterRestart(ctx, same)
	}
	return awaitAgentVerdict(ctx, conn, reconnect, sha, announce)
}

// printRejectedJSON gives --json callers (and the MCP tool) a rejection, at a
// trial or at the check before it; running is the agent the device runs now.
func printRejectedJSON(err error, running string) {
	var rejected *agentRejectedError
	var refused *agentCheckRefusedError
	if !errors.As(err, &rejected) && !errors.As(err, &refused) {
		return
	}
	msg := err.Error()
	if refused != nil {
		msg += "; the device kept agent " + running
	}
	b, _ := json.Marshal(map[string]string{"status": "rejected", "message": msg, "version": running})
	fmt.Println(string(b))
}

// cloudBinaryHint tells the user what becomes of a --binary agent once an OS
// update the CLI does not wait for reboots the device.
func cloudBinaryHint(binaryPath string, supervised bool) string {
	if supervised {
		return fmt.Sprintf("The OS update is rebooting the device. The agent supervisor keeps your --binary agent only "+
			"if it is newer than the new image's agent, and checks it again on the new OS. Once the device is back online, "+
			"check with 'wendy device info', and re-run 'wendy device update --binary %s' if it runs another agent.", binaryPath)
	}
	return fmt.Sprintf("The OS update is rebooting the device; its new image ships a bundled agent. "+
		"Re-run 'wendy device update --binary %s' once it is back online to restore your dev agent.", binaryPath)
}

func announceAgentCheck(w io.Writer) func() {
	return func() {
		fmt.Fprintln(w, "Checking the new agent on the device (usually about 15 seconds)...")
	}
}

// confirmAgentOrContinue waits for the verdict on a routine update and keeps
// going with whichever agent the device runs; it fails only when the device
// cannot be reached again.
func confirmAgentOrContinue(ctx context.Context, conn *grpcclient.AgentConnection, sha string, w io.Writer) (*grpcclient.AgentConnection, error) {
	_, conn, err := reportAgentVerdict(ctx, conn, sha, announceAgentCheck(w))
	if err != nil {
		if conn == nil {
			return nil, err
		}
		fmt.Fprintf(w, "%v\nContinuing with the agent the device runs now.\n", err)
	}
	return conn, nil
}

// keptAcrossOSUpdate reports whether the supervisor kept the --binary agent
// over an OS update, as it does for a dev build or one newer than the new
// image's agent. Otherwise the agent is uploaded again, which starts a trial.
func keptAcrossOSUpdate(resp *agentpb.GetAgentVersionResponse, sha string) (message string, ok bool) {
	if resp.GetSupervision() == nil || resp.GetBinarySha256() != sha {
		return "", false
	}
	return fmt.Sprintf("Your --binary agent (version %s) was kept across the OS update.", resp.GetVersion()), true
}
