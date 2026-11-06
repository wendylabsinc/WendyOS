package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// newDeviceBuildHostCmd manages whether a device will accept remote builds.
//
// This exists as an RPC-backed command rather than documentation telling people
// to touch a file over `wendy device shell`: handing out shell access to flip a
// boolean is a worse trade than a narrow, authenticated RPC that only a user
// certificate can call.
func newDeviceBuildHostCmd() *cobra.Command {
	cmd := &cobra.Command{
		Hidden: true,
		Use:    "build-host",
		Short:  "Manage whether this device accepts remote builds",
		Long: "A build host runs image builds submitted by `wendy run --build-host`.\n" +
			"The role is off by default: running builds for other people is something a\n" +
			"device opts into, not something it acquires by being reachable.",
	}
	cmd.AddCommand(
		newDeviceBuildHostSetCmd("enable", "Allow this device to run remote builds", true),
		newDeviceBuildHostSetCmd("disable", "Stop this device from running remote builds", false),
		newDeviceBuildHostStatusCmd(),
	)
	return cmd
}

func newDeviceBuildHostSetCmd(use, short string, enabled bool) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			target, err := resolveTarget(ctx, SuppressUpdateCheck())
			if err != nil {
				return err
			}
			defer target.Close()
			if target.Agent == nil {
				return fmt.Errorf("build-host settings apply to WendyOS devices only")
			}

			resp, err := target.Agent.BuildService.SetBuildHostEnabled(ctx,
				&agentpbv2.SetBuildHostEnabledRequest{Enabled: enabled})
			if err != nil {
				return err
			}
			if resp.GetEnabled() {
				cliSuccess("Build host enabled. Target it with %s.", tui.Value("wendy run --build-host <device>"))
				warnIfCannotBuild(ctx, target.Agent)
			} else {
				cliSuccess("Build host disabled. This device will refuse remote builds.")
			}
			return nil
		},
	}
}

// warnIfCannotBuild says so when the role was just enabled on a device that has
// no build engine.
//
// Enabling only writes the opt-in marker; the agent does not check for buildkitd
// and should not, since the role is a policy decision that can legitimately
// precede installing one. But the success line above tells the developer to go
// and target this host, and without this the next thing they see is a build
// failing with FailedPrecondition -- so the message would be a promise the
// device cannot keep.
//
// Advisory only. The role IS enabled, the capability probe is best-effort, and a
// probe that cannot answer must not make a successful enable look failed.
func warnIfCannotBuild(ctx context.Context, agent *grpcclient.AgentConnection) {
	caps, err := agent.BuildService.GetBuildCapabilities(ctx, &agentpbv2.GetBuildCapabilitiesRequest{})
	if err != nil {
		return
	}
	for _, line := range buildkitAbsentNotice(caps.GetBuildkitAvailable(), caps.GetOs()) {
		cliLogln("%s", line)
	}
}

// buildkitAbsentNotice returns the lines to print after enabling the role, or
// nothing when the device can actually build.
//
// darwin gets a different message because it is not a fixable omission: the Mac
// agent runs containers through Apple Container, which has no BuildKit under it,
// so telling someone to install a daemon would send them after something that
// does not exist for their platform.
func buildkitAbsentNotice(buildkitAvailable bool, os string) []string {
	if buildkitAvailable {
		return nil
	}
	if os == "darwin" {
		return []string{
			"Note: This device has no BuildKit: macOS runs containers through Apple Container, which has none, so it cannot serve remote builds.",
		}
	}
	return []string{
		"Note: This device has no BuildKit daemon yet, so builds sent here will be refused.",
		"      Install buildkitd and start it on unix:///run/buildkit/buildkitd.sock before targeting this host.",
	}
}

func newDeviceBuildHostStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether this device accepts remote builds, and what it can build",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			target, err := resolveTarget(ctx, SuppressUpdateCheck())
			if err != nil {
				return err
			}
			defer target.Close()
			if target.Agent == nil {
				return fmt.Errorf("build-host settings apply to WendyOS devices only")
			}

			caps, err := target.Agent.BuildService.GetBuildCapabilities(ctx,
				&agentpbv2.GetBuildCapabilitiesRequest{})
			if err != nil {
				return err
			}

			if jsonOutput {
				return writeBuildCapabilitiesJSON(cmd.OutOrStdout(), caps)
			}
			for _, line := range buildHostStatusLines(caps, deviceFlag) {
				cliLogln("%s", line)
			}
			return nil
		},
	}
}

// buildHostStatusLines renders `build-host status`. device is the selector the
// developer used, so the fix it suggests can be pasted back as-is.
func buildHostStatusLines(caps *agentpbv2.GetBuildCapabilitiesResponse, device string) []string {
	var lines []string
	if caps.GetBuilderEnabled() {
		lines = append(lines, "Builder role: "+tui.Value("enabled"))
	} else {
		lines = append(lines, fmt.Sprintf("Builder role: %s (run `wendy device build-host enable`)", tui.Value("disabled")))
	}
	switch {
	case caps.GetBuildkitAvailable() && caps.GetBuildkitVersion() != "":
		lines = append(lines, fmt.Sprintf("BuildKit:     %s (%s)", tui.Value("available"), caps.GetBuildkitVersion()))
	case caps.GetBuildkitAvailable():
		lines = append(lines, "BuildKit:     "+tui.Value("available"))
	case caps.GetOs() == "darwin":
		// Say why on darwin: a bare "unavailable" on a Mac reads as a bug
		// rather than a property of how the Mac agent runs containers.
		lines = append(lines, fmt.Sprintf("BuildKit:     %s (macOS runs containers through Apple Container, which has none)", tui.Value("unavailable")))
	default:
		// A bare "unavailable" left the developer guessing what the agent
		// looks for; name the daemon and the socket, as the run preflight does.
		lines = append(lines, fmt.Sprintf("BuildKit:     %s (%s)", tui.Value("unavailable"), buildkitdInstallHint))
	}
	// Where the cache lands is worth seeing before a build, not after
	// the disk fills. Silent on an older agent, which reports nothing.
	if root := caps.GetBuildkitRoot(); root != "" && caps.GetBuildkitRootTotalBytes() > 0 {
		lines = append(lines, fmt.Sprintf("Cache:        %s (%s free of %s)", tui.Value(root),
			humanBytes(caps.GetBuildkitRootFreeBytes()), humanBytes(caps.GetBuildkitRootTotalBytes())))
	} else if caps.GetBuildkitAvailable() && caps.GetBuildkitRootInspectionSupported() {
		lines = append(lines, "Cache:        "+tui.Value("inspection failed"))
	}
	// Silent on an older agent: its zero internet_reachable means "did not
	// check", not "offline".
	if caps.GetInternetCheckSupported() {
		if caps.GetInternetReachable() {
			lines = append(lines, "Internet:     "+tui.Value("reachable"))
		} else {
			state := "Internet:     " + tui.Value("not reachable")
			if detail := caps.GetInternetDetail(); detail != "" {
				state += " (" + detail + ")"
			}
			lines = append(lines, state,
				"              Builds pull base images from the internet; "+buildHostInternetRemedy(device)+".")
		}
	}
	lines = append(lines, fmt.Sprintf("Platform:     %s/%s", caps.GetOs(), caps.GetCpuArchitecture()))
	if len(caps.GetNativePlatforms()) > 0 {
		lines = append(lines, fmt.Sprintf("Builds:       %s natively", formatPlatformList(caps.GetNativePlatforms())))
	}
	if len(caps.GetEmulatedPlatforms()) > 0 {
		lines = append(lines, fmt.Sprintf("Emulates:     %s", formatPlatformList(caps.GetEmulatedPlatforms())))
	}
	return lines
}

// writeBuildCapabilitiesJSON is `build-host status --json`: the agent's whole
// answer under its proto field names, so a field added to the response shows
// up here without a second schema to keep in step. Unpopulated fields are
// emitted, so internet_reachable: false is stated rather than left for a
// script to infer, and internet_check_supported says whether to believe it.
func writeBuildCapabilitiesJSON(out io.Writer, caps *agentpbv2.GetBuildCapabilitiesResponse) error {
	raw, err := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}.Marshal(caps)
	if err != nil {
		return err
	}
	// Re-indented by encoding/json: protojson varies its whitespace between
	// runs on purpose, and this makes the output stable.
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return err
	}
	buf.WriteByte('\n')
	_, err = out.Write(buf.Bytes())
	return err
}
