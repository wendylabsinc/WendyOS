package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/terminaloutput"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// An agent update can take a device off Wendy Cloud without anyone meaning to.
// A device enrolled with Cloud v1 (the legacy, numeric enrollment) holds no
// Cloud v2 identity. Agents up to cloudV1TunnelRemovedIn opened a Cloud v1
// tunnel for such a device; agents from that release on start a Cloud
// connection only for a Cloud v2 identity. Installing one of them on a Cloud
// v1 device therefore ends its Cloud access until it is enrolled again, which
// needs a direct connection. Every command that replaces a device's agent
// asks confirmCloudV1Disconnect first.

const (
	// cloudV1TunnelRemovedIn is the first agent release without the Cloud v1
	// tunnel (commit 228d31128, PR #2157).
	cloudV1TunnelRemovedIn = "2026.10.22-080351"

	allowCloudV1DisconnectFlag  = "allow-cloud-v1-disconnect"
	allowCloudV1DisconnectUsage = "Update even though it disconnects a device enrolled with Cloud v1 from Wendy Cloud until the device is enrolled again"

	// cloudV1EnrollmentProbeTimeout bounds the one enrollment question an
	// update asks the device. No answer in time means the update goes ahead
	// as it always has.
	cloudV1EnrollmentProbeTimeout = 5 * time.Second
)

// cloudV1Disconnect is an agent update that would take a device off Wendy
// Cloud.
type cloudV1Disconnect struct {
	device string
	// current and next are the agent version the device runs and the one
	// about to be installed. Either is empty when unknown: an agent that did
	// not answer, or a local binary.
	current, next string
	// throughCloud is set when this command itself reaches the device through
	// Wendy Cloud, the connection the update ends.
	throughCloud bool
}

// agentVersionHasCloudV1Tunnel reports whether agent version v still opens the
// Cloud v1 tunnel.
//
// A branch build ("<date>-dev") is judged by its build date, which the shared
// comparison would otherwise rank newest: nothing built before the tunnel was
// removed can be without it. A local "dev" build has no date and is taken to
// be current, as everywhere else in the CLI (version.IsDev). An agent that
// reports no version cannot be shown to be current, so it counts as having the
// tunnel. Both guesses are checked against the device's enrollment, and against
// the connection in use, before anything is said (see cloudV1DisconnectRisk).
func agentVersionHasCloudV1Tunnel(v string) bool {
	return version.CompareVersions(strings.TrimSuffix(v, "-dev"), cloudV1TunnelRemovedIn) < 0
}

// cloudV1DisconnectRisk reports whether replacing the agent on conn's device
// with version next would take the device off Wendy Cloud, and nil when it
// would not or when that cannot be established: a failed enrollment probe
// never stands in the way of an update. current is the device's version
// answer; next is empty for a binary of unknown version, which is treated as
// a current build.
//
// The versions are compared first, so the device is asked for its enrollment
// only when they leave something to lose.
func cloudV1DisconnectRisk(ctx context.Context, conn *grpcclient.AgentConnection, current *agentpb.GetAgentVersionResponse, next string) *cloudV1Disconnect {
	if conn == nil {
		return nil
	}
	// The native Mac agent is a separate program and still opens the Cloud v1
	// tunnel in its current releases.
	if strings.EqualFold(current.GetOs(), "darwin") {
		return nil
	}
	if next != "" && agentVersionHasCloudV1Tunnel(next) {
		return nil
	}
	// A Cloud v1 device reached through Wendy Cloud is running the tunnel,
	// whatever version it reports (a dev build, for one).
	throughCloud := osUpdateThroughCloud(ctx, conn)
	if !agentVersionHasCloudV1Tunnel(current.GetVersion()) && (!throughCloud || conn.IsCloudV2) {
		return nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, cloudV1EnrollmentProbeTimeout)
	defer cancel()
	resp, err := deviceProvisioning(probeCtx, conn)
	if err != nil {
		return nil
	}
	// Enrolled with no Cloud v2 identity: the exact condition under which a
	// current agent starts no Cloud connection.
	enrollment := resp.GetProvisioned()
	if enrollment == nil || enrollment.GetPrincipalUri() != "" {
		return nil
	}

	// A command run on the device itself reaches the agent over its local
	// socket, which is not a name anyone knows the device by.
	device := conn.Host
	if device == "" || strings.HasPrefix(device, "unix:") {
		device = "this device"
	}
	return &cloudV1Disconnect{device: device, current: current.GetVersion(), next: next, throughCloud: throughCloud}
}

// confirmCloudV1Disconnect is the gate in front of an agent update. It returns
// nil when the update may go ahead: nothing would be lost, the person at the
// terminal agreed, or allow (--allow-cloud-v1-disconnect) was given. Without a
// terminal, in JSON mode included, it refuses instead of asking. current and
// next are as for cloudV1DisconnectRisk.
func confirmCloudV1Disconnect(ctx context.Context, conn *grpcclient.AgentConnection, current *agentpb.GetAgentVersionResponse, next string, allow bool) error {
	risk := cloudV1DisconnectRisk(ctx, conn, current, next)
	if risk == nil {
		return nil
	}
	if allow {
		if !jsonOutput {
			fmt.Fprintln(terminaloutput.Stderr, tui.WarningMessage(fmt.Sprintf(
				"This update will disconnect %s from Wendy Cloud until it is enrolled again (--%s).", risk.device, allowCloudV1DisconnectFlag)))
		}
		return nil
	}
	if jsonOutput || !isInteractiveTerminal() {
		return risk.refusal()
	}
	fmt.Fprintln(terminaloutput.Stderr, risk.warning())
	if !confirmDefaultNoFn("Update the agent anyway?") {
		fmt.Fprintf(terminaloutput.Stderr, "Update cancelled. Nothing was changed on %s.\n", risk.device)
		return ErrUserCancelled
	}
	return nil
}

// cloudV1ReenrollCommands are the commands that put a Cloud v1 device back on
// Wendy Cloud, in order. They need a direct connection: enrollment is not
// available through Wendy Cloud.
var cloudV1ReenrollCommands = []string{
	"wendy device unenroll --device <device-address>",
	"wendy cloud login",
	"wendy device enroll --device <device-address>",
}

// why says what the update changes, in one paragraph.
func (r *cloudV1Disconnect) why() string {
	current, next := "The agent it runs now", "The agent being installed"
	if r.current != "" {
		current += " (" + r.current + ")"
	}
	if r.next != "" {
		next += " (" + r.next + ")"
	}
	return "It is enrolled with the older Wendy Cloud (Cloud v1). " + current + " keeps it connected. " +
		next + " connects to Wendy Cloud only after the device has been enrolled again."
}

// warning is what a person reads before the prompt.
func (r *cloudV1Disconnect) warning() string {
	paragraphs := []string{
		r.why() + " Until then you can reach " + r.device + " only directly, over the local network or USB.",
	}
	if r.throughCloud {
		paragraphs = append(paragraphs, "You are connected to "+r.device+" through Wendy Cloud right now. "+
			"That connection stops working when the new agent starts, so this command will not be able to confirm the update or reach the device again.")
	}
	paragraphs = append(paragraphs, "To put it back on Wendy Cloud, connect to it directly (local network or USB; this cannot be done through Wendy Cloud) and enroll it again:")

	var b strings.Builder
	b.WriteString(tui.WarningMessage("This update will disconnect "+r.device+" from Wendy Cloud.") + "\n")
	for _, paragraph := range paragraphs {
		b.WriteString("\n" + indentLines(tui.WrapHint(paragraph, 76), "  ") + "\n")
	}
	b.WriteString("\n")
	for _, command := range cloudV1ReenrollCommands {
		b.WriteString("    " + tui.Command(command) + "\n")
	}
	b.WriteString("\n" + indentLines(tui.WrapHint(
		"'wendy device unenroll' uses your Cloud v1 sign-in; restore an expired one with 'wendy cloud login --legacy'.", 76), "  ") + "\n")
	b.WriteString("  Moving a device to Cloud v2: " + tui.Path(docsBaseURL+"/cloud-migration") + "\n")
	return b.String()
}

// refusal is the error for a run with nobody to ask. Its steps become
// next_steps in JSON mode.
func (r *cloudV1Disconnect) refusal() error {
	message := "Not updated: this would disconnect " + r.device + " from Wendy Cloud. " + r.why()
	if r.throughCloud {
		message += " This command reaches " + r.device + " through Wendy Cloud, the connection that would stop working."
	}
	return withNextSteps(errors.New(message),
		"To update anyway, pass --"+allowCloudV1DisconnectFlag+". Do it when you can reach the device directly (local network or USB): enrolling it again cannot be done through Wendy Cloud.",
		"Then enroll it again: "+strings.Join(cloudV1ReenrollCommands, "; ")+".",
	)
}

// offerSkippedNotice replaces the update offer made on connect.
func (r *cloudV1Disconnect) offerSkippedNotice() string {
	return "Not offering the update here: it would disconnect " + r.device + " from Wendy Cloud, because the device is enrolled with the older Wendy Cloud (Cloud v1). " +
		"Run 'wendy device update' to see what that means and update it anyway."
}

// discoverCloudV1Note is the line the device tables show instead of updating
// a device the update would take off Wendy Cloud. The update key runs inside
// the table, where nothing can be asked, so it names the command that asks.
// It returns "" when the update may go ahead.
func discoverCloudV1Note(ctx context.Context, conn *grpcclient.AgentConnection, current *agentpb.GetAgentVersionResponse, next, device, command string) string {
	if cloudV1DisconnectRisk(ctx, conn, current, next) == nil {
		return ""
	}
	return fmt.Sprintf("Not updated: this would disconnect %s from Wendy Cloud. Run '%s' to see why and confirm.", device, command)
}

func indentLines(text, indent string) string {
	return indent + strings.ReplaceAll(text, "\n", "\n"+indent)
}
