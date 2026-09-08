package commands

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func newOSCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "os",
		Short: "Manage the WendyOS operating system",
	}

	cmd.AddCommand(newOSUpdateCmd())
	cmd.AddCommand(newOSUpdateStatusCmd())
	cmd.AddCommand(newOSListDrivesCmd())
	addOSInstallCmd(cmd)
	addOSDownloadCmd(cmd)
	addOSCacheCmd(cmd)
	return cmd
}

const (
	osUpdateUnsupportedMessage      = "This setup cannot be updated with wendy os update. Use this machine’s normal OS update tools instead. To use WendyOS OTA updates, install WendyOS on supported hardware with wendy os install."
	linuxOSUpdateUnsupportedMessage = "This Linux host has wendy-agent installed, but it cannot be updated with WendyOS OTA artifacts. Use the Linux distribution’s package manager, such as apt, dnf, or pacman, to update this machine."
	wendyOSMissingUpdaterMessage    = "This WendyOS image does not support OTA updates because the wendyos-update engine was not found on the device. Reinstall or upgrade to a WendyOS image with OTA support."
)

// firstWendyOSUpdateVersion is the first WendyOS release whose OTA update system
// is incompatible with the pre-0.17.0 stack. A device older than this cannot be
// updated over the air — its update backend does not understand the new
// artifact format — and must be reflashed with `wendy os install`.
const firstWendyOSUpdateVersion = "0.17.0"

func validateOSUpdateIdentity(versionResp *agentpb.GetAgentVersionResponse) error {
	if isWendyOSUpdateTarget(versionResp) {
		return nil
	}
	if osIsLinuxFamily(versionResp.GetOs()) {
		return errors.New(linuxOSUpdateUnsupportedMessage)
	}
	return errors.New(osUpdateUnsupportedMessage)
}

// osIsLinuxFamily reports whether the agent's reported OS is a Linux
// distribution. Since #1136 the agent reports the /etc/os-release ID (e.g.
// "ubuntu", "wendyos") instead of "linux", so equality with "linux" no longer
// identifies Linux hosts. Only darwin and windows agents are non-Linux; an
// empty/unknown OS is treated as non-Linux so it gets the generic message.
func osIsLinuxFamily(agentOS string) bool {
	switch agentOS {
	case "", "darwin", "windows":
		return false
	default:
		return true
	}
}

func validateOSUpdateTarget(versionResp *agentpb.GetAgentVersionResponse) error {
	if err := validateOSUpdateIdentity(versionResp); err != nil {
		return err
	}
	if !hasOTABackend(versionResp) {
		return errors.New(wendyOSMissingUpdaterMessage)
	}
	return nil
}

// requireReflashableOSVersion returns a reflash-guidance error when the device's
// current WendyOS version predates firstWendyOSUpdateVersion, whose OTA format
// the older update stack cannot apply. Both `wendy os update` and
// `wendy device update` call it before attempting any OTA, so a stranded device
// is refused loudly instead of failing mid-stream.
//
// It fails open: the "WendyOS-" display prefix is stripped, and a dev build (see
// version.IsDev) or an empty/unparseable version is allowed through. Only a
// version that clearly parses as older than 0.17.0 is blocked, so dev/CI images
// are never stranded. CompareVersions splits on "."/"-", so "0.16.0-nightly"
// sorts below 0.17.0 (blocked) while "0.17.0-nightly" sorts at/above it (allowed).
func requireReflashableOSVersion(osVersion string) error {
	normalized := strings.TrimPrefix(osVersion, "WendyOS-")
	if normalized == "" || version.IsDev(normalized) {
		return nil
	}
	if version.CompareVersions(normalized, firstWendyOSUpdateVersion) < 0 {
		return fmt.Errorf(
			"this device runs WendyOS %s. WendyOS %s introduces a new update system "+
				"with no backward compatibility, so it cannot be updated over the air.\n"+
				"Reflash it with `wendy os install` to continue receiving updates.",
			normalized, firstWendyOSUpdateVersion)
	}
	return nil
}

// hasOTABackend reports whether the device advertises an OS update backend the
// agent can drive: the in-house wendyos-update engine. Both `wendy os update`
// and `wendy device update` gate their OS-update step on this, so the two
// paths cannot drift on which backends count.
func hasOTABackend(versionResp *agentpb.GetAgentVersionResponse) bool {
	return agentVersionHasFeature(versionResp, "wendyos-update")
}

// requiredOTAFeatureForArtifact maps an artifact URL to the featureset entry of
// the only backend that can install it: .wendy → "wendyos-update", "" when the
// extension is unknown. Mirrors the agent's requiredUpdaterForArtifact.
func requiredOTAFeatureForArtifact(artifactURL string) string {
	path := artifactURL
	if u, err := url.Parse(artifactURL); err == nil && u.Path != "" {
		path = u.Path
	}
	if strings.HasSuffix(path, ".wendy") {
		return "wendyos-update"
	}
	return ""
}

// osUpdateStackMismatch reports whether the artifact's OTA stack is one the
// device cannot run, so the CLI can explain the situation instead of offering
// an update that is guaranteed to fail on the device. A device that advertises
// a featureset without the required backend is on an image that predates the
// wendyos-update stack: its partition layout is incompatible, so no OTA can
// cross it and the device must be reflashed. Devices that advertise no
// featureset at all are left to the agent's own selection error (older agents
// predate the featureset entries).
func osUpdateStackMismatch(versionResp *agentpb.GetAgentVersionResponse, artifactURL string) error {
	required := requiredOTAFeatureForArtifact(artifactURL)
	if required == "" || len(versionResp.GetFeatureset()) == 0 || agentVersionHasFeature(versionResp, required) {
		return nil
	}
	return errors.New("this OS update uses the wendyos-update stack, which this device does not support " +
		"(its image predates the wendyos-update stack, and the partition layout is incompatible); " +
		"reflash the device with a current WendyOS image to continue receiving OS updates")
}

// isWendyOSUpdateTarget reports whether the device is a WendyOS OTA target. The
// signals are WendyOS-specific and authoritative on their own: a "WendyOS-" os
// version (from /etc/wendyos/version.txt) or a device type (from
// /etc/wendyos/device-type), neither of which is ever present on a non-WendyOS
// host. It deliberately does NOT gate on the reported OS, which since #1136 is
// the os-release ID (e.g. "wendyos"), not "linux".
func isWendyOSUpdateTarget(versionResp *agentpb.GetAgentVersionResponse) bool {
	return strings.HasPrefix(versionResp.GetOsVersion(), "WendyOS-") ||
		versionResp.GetDeviceType() != ""
}

func agentVersionHasFeature(versionResp *agentpb.GetAgentVersionResponse, feature string) bool {
	for _, f := range versionResp.GetFeatureset() {
		if f == feature {
			return true
		}
	}
	return false
}

// osAlreadyCurrent reports whether the device's current OS version is at or
// ahead of the latest available version. The "WendyOS-" display prefix is
// stripped before comparing so that "WendyOS-0.10.4" and "0.12.0-nightly"
// compare correctly. Returns false when either version is unknown.
func osAlreadyCurrent(currentOSVersion, latestVersion string, nightly bool) bool {
	if currentOSVersion == "" || latestVersion == "" {
		return false
	}
	normalized := strings.TrimPrefix(currentOSVersion, "WendyOS-")
	return nightly && latestVersion == normalized ||
		!nightly && version.CompareVersions(latestVersion, normalized) <= 0
}

// osUpdateShouldSkipAlreadyCurrent reports whether `os update`'s auto-detect
// path should treat the device as already up to date and skip flashing. It
// never short-circuits for --pr requests (prNumber > 0): a PR's resolved
// version tag ("pr-N") is constant across rebuilds, so honoring
// osAlreadyCurrent there would silently no-op a re-test after pushing a new
// commit to the same PR and re-running `os update --pr N`. Non-PR behavior is
// unchanged — it defers entirely to osAlreadyCurrent.
func osUpdateShouldSkipAlreadyCurrent(prNumber int, currentOSVersion, latestVersion string, nightly bool) bool {
	return prNumber == 0 && osAlreadyCurrent(currentOSVersion, latestVersion, nightly)
}

// osUpdateAction is the decision for the OS-update step of `device update`.
type osUpdateAction int

const (
	osActionAlreadyCurrent osUpdateAction = iota // device is already at/ahead of latest
	osActionApply                                // apply without prompting (--yes)
	osActionPrompt                               // interactive: ask the user
	osActionReportOnly                           // non-interactive, no --yes: report and skip
)

// decideOSUpdate chooses how the OS-update step behaves when a newer OS may be
// available. It is pure so it can be unit-tested; the caller is responsible for
// running the interactive prompt when the result is osActionPrompt. A --pr
// request (prNumber > 0) never resolves to osActionAlreadyCurrent: a PR's
// version tag ("pr-N") is constant across rebuilds, so treating a matching tag
// as current would silently no-op a re-test after a new push to the same PR
// (same rationale as osUpdateShouldSkipAlreadyCurrent, which implements the
// suppression).
func decideOSUpdate(prNumber int, currentOSVersion, latestVersion string, nightly, assumeYes, interactive bool) osUpdateAction {
	if osUpdateShouldSkipAlreadyCurrent(prNumber, currentOSVersion, latestVersion, nightly) {
		return osActionAlreadyCurrent
	}
	switch {
	case assumeYes:
		return osActionApply
	case interactive:
		return osActionPrompt
	default:
		return osActionReportOnly
	}
}

func newOSUpdateCmd() *cobra.Command {
	var artifactURL string
	var nightly bool
	var prNumber int
	var noDrivers bool
	var driversDir string

	cmd := &cobra.Command{
		Use:   "update [artifact-path]",
		Short: "Update WendyOS on the target device",
		Long: `Update WendyOS using an OS update artifact. Provide a local file path or
directory as a positional argument, or use --artifact-url for a remote URL.

When a local file is provided, the CLI serves it via a temporary HTTP server
so the device can download it directly.

The device uses its in-house wendyos-update engine to apply the update.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			// Determine the artifact URL: local path, remote URL, or manifest picker.
			if len(args) > 0 && artifactURL != "" {
				return fmt.Errorf("provide either a local artifact path or --artifact-url, not both")
			}

			if noDrivers && driversDir != "" {
				return fmt.Errorf("--no-drivers cannot be combined with --drivers-dir")
			}

			if prNumber > 0 {
				if len(args) > 0 || artifactURL != "" {
					return fmt.Errorf("--pr cannot be combined with a local artifact path or --artifact-url")
				}
				fmt.Fprintln(cmd.ErrOrStderr(), tui.WarningMessage("PR images are unhardened debug builds (passwordless root, SSH on). Do not use in production."))
			}

			conn, err := connectToAgent(ctx, SuppressUpdateCheck())
			if err != nil {
				return err
			}
			// Use a closure so the defer always closes the current conn even if
			// it is replaced by the agent pre-update step below.
			defer func() { conn.Close() }()

			// Step 1: Validate that this target is in the WendyOS OTA family before
			// updating the agent as a prerequisite for the OS-image update path.
			versionResp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
			if err != nil {
				return fmt.Errorf("querying device version: %w", err)
			}
			if err := validateOSUpdateIdentity(versionResp); err != nil {
				return err
			}
			// Refuse OTA on a pre-0.17.0 device before touching the agent: its
			// update stack cannot apply the new artifact format, so a reflash is
			// the only path forward. Checked here (before ensureAgentUpToDate and
			// any artifact resolution) so it covers auto, --artifact-url, and
			// local-path updates without wasting an agent update.
			if err := requireReflashableOSVersion(versionResp.GetOsVersion()); err != nil {
				return err
			}
			// Same rule for a local artifact path: a device reached over loopback
			// sits behind a port forward, and the only address this side could
			// advertise is one that inside the guest is the guest.
			if len(args) > 0 && isLoopbackHost(conn.Host) {
				return fmt.Errorf("%s reaches this machine over loopback, so it cannot fetch a locally served artifact; "+
					"pass --pr or --artifact-url and let the device download it directly", conn.Host)
			}

			// Step 2: Ensure the agent is at the latest release before updating the OS.
			conn, err = ensureAgentUpToDate(ctx, conn, versionResp, nightly)
			if err != nil {
				return err
			}
			// Re-query after the potential agent restart.
			versionResp, err = conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
			if err != nil {
				return fmt.Errorf("querying device version after agent update: %w", err)
			}

			if err := validateOSUpdateTarget(versionResp); err != nil {
				return err
			}

			// Step 3: Show current OS version. It is also the baseline for
			// detecting a post-reboot rollback.
			preUpdateOSVersion := versionResp.GetOsVersion()
			if preUpdateOSVersion != "" {
				fmt.Printf("Current OS version: %s\n", preUpdateOSVersion)
			}

			// Only set when the manifest resolved the target; a local artifact or
			// --artifact-url leaves it empty and limits the pre-flight to a warning.
			var targetVersion string

			// No artifact provided — auto-detect from the reported device type.
			if len(args) == 0 && artifactURL == "" {
				deviceType := versionResp.GetDeviceType()
				storageMedium := versionResp.GetStorageMedium()
				otaURL, latestVer, autoErr := func() (string, string, error) {
					if deviceType == "" {
						return "", "", fmt.Errorf("device type not reported")
					}
					if prNumber > 0 {
						return getPROTAInfoForDeviceType(prNumber, deviceType, storageMedium)
					}
					return getLatestOTAInfoForDeviceType(deviceType, storageMedium, nightly)
				}()

				if autoErr != nil {
					// A --pr update must resolve to that PR's build or fail outright —
					// falling back to the interactive (non-PR) device picker below would
					// silently serve a stable/nightly artifact instead of the requested
					// PR build.
					if prNumber > 0 {
						return autoErr
					}
					// Device type is missing or not in the update catalog — fall back to
					// a device picker so the user can force the correct device type.
					// The latest version (or latest nightly with --nightly) is then chosen
					// automatically, consistent with the normal auto-detect path.
					if deviceType == "" {
						fmt.Println("Warning: this device did not report a device type, so the update target cannot be selected automatically.")
					} else {
						fmt.Printf("Warning: device type %q is not recognized in the update catalog.\n", deviceType)
					}
					fmt.Println("Please select the correct device type to continue.")
					fmt.Println()
					picked, pickErr := pickDeviceTypeAndGetLatestOTA(nightly)
					if pickErr != nil {
						return pickErr
					}
					artifactURL = picked
				} else {
					if osVer := versionResp.GetOsVersion(); osVer != "" && latestVer != "" {
						if osUpdateShouldSkipAlreadyCurrent(prNumber, osVer, latestVer, nightly) {
							fmt.Printf("OS is already at the latest version (%s).\n", osVer)
							return nil
						}
						fmt.Printf("Latest OS version: %s\n", latestVer)
					}
					artifactURL = otaURL
					targetVersion = latestVer
				}
			}

			// If a local path is provided, resolve and serve it.
			if len(args) > 0 {
				localPath, err := resolveArtifactPath(args[0])
				if err != nil {
					return err
				}

				// Determine the local IP reachable by the device.
				localIP, err := localIPForHost(conn.Host)
				if err != nil {
					return fmt.Errorf("determining local IP for device %s: %w", conn.Host, err)
				}

				servedURL, cleanup, err := serveLocalArtifact(localPath, localIP)
				if err != nil {
					return err
				}
				defer cleanup()
				artifactURL = servedURL
				fmt.Printf("Serving artifact at: %s\n", artifactURL)
				// A device on loopback has a port forward between us and it, so it
				// has no WiFi to report yet is not offline: it reaches the internet
				// through the host's NAT and can fetch the artifact itself.
			} else if artifactURL != "" && !isLoopbackHost(conn.Host) && !deviceHasWiFi(ctx, conn) {
				// Device has no WiFi connection — it cannot reach GCP directly.
				// Download the artifact on the Mac and serve it over a local HTTP
				// server so the device can fetch it from the Mac instead.
				fmt.Println("Device is not connected to WiFi — downloading artifact to serve locally...")
				localPath, err := downloadArtifactToTemp(artifactURL)
				if err != nil {
					return fmt.Errorf("downloading artifact: %w", err)
				}

				localIP, err := localIPForHost(conn.Host)
				if err != nil {
					return fmt.Errorf("determining local IP for device %s: %w", conn.Host, err)
				}

				servedURL, cleanup, err := serveLocalArtifact(localPath, localIP)
				if err != nil {
					return err
				}
				defer cleanup()
				artifactURL = servedURL
				fmt.Printf("Serving artifact at: %s\n", artifactURL)
			}

			if artifactURL == "" {
				return fmt.Errorf("provide a local artifact path or --artifact-url")
			}

			// Refuse a doomed update before anything streams: a .wendy artifact
			// cannot be installed by a device whose image predates the
			// wendyos-update stack.
			if err := osUpdateStackMismatch(versionResp, artifactURL); err != nil {
				return err
			}

			if driversDir != "" {
				if err := checkDriversDir(conn, driversDir); err != nil {
					return err
				}
			}

			// Last point before anything transfers. Staging a rebuild that fails is
			// the case where the device loses a driver it has today, so the operator
			// decides rather than finding out after the reboot.
			pf := warnDriverAddonsBeforeUpdate(ctx, conn, versionResp.GetDeviceType(), targetVersion, driversDir, prNumber, !noDrivers)
			if !noDrivers && pf.blocking() {
				if err := confirmDriverPreflight(pf); err != nil {
					return err
				}
			}

			if err := streamOSUpdate(ctx, conn, artifactURL, ""); err != nil {
				return err
			}

			deviceHost := conn.Host
			fmt.Println("WendyOS update applied. Device is rebooting...")
			if err := waitForDeviceOnline(ctx, deviceHost); err != nil {
				return err
			}
			fmt.Println("Device is back online.")
			return reportOSUpdateOutcome(ctx, deviceHost, preUpdateOSVersion)
		},
	}

	cmd.Flags().StringVar(&artifactURL, "artifact-url", "", "OS update artifact URL (remote)")
	cmd.Flags().BoolVar(&nightly, "nightly", false, "Use the latest nightly (prerelease) build for both agent and OS")
	cmd.Flags().StringVar(&driversDir, "drivers-dir", "",
		"Stage driver add-ons for the target kernel from local .raw files in this directory, instead of the registry (for networks with no internet access)")
	cmd.Flags().BoolVar(&noDrivers, "no-drivers", false,
		"Update even though driver add-ons will not be staged for the target kernel; skips staging and the confirmation")
	cmd.Flags().IntVar(&prNumber, "pr", 0, "OTA-update to the image built by wendyos-builder PR #N (debug build; mutually exclusive with a positional artifact path and --artifact-url)")

	return cmd
}

// streamOSUpdate starts an UpdateOS stream for artifactURL on conn and reports
// progress: a spinner when interactive, a silent drain otherwise. It does not
// wait for the post-update reboot.
func streamOSUpdate(ctx context.Context, conn *grpcclient.AgentConnection, artifactURL, updaterBackend string) error {
	stream, err := conn.AgentService.UpdateOS(ctx, &agentpb.UpdateOSRequest{
		ArtifactUrl:    artifactURL,
		UpdaterBackend: updaterBackend,
	})
	if err != nil {
		return fmt.Errorf("starting OS update: %w", err)
	}

	if !isInteractiveTerminal() {
		return drainOSUpdateStream(stream)
	}

	spin := tui.NewSpinner("Downloading update...")
	p := tui.NewProgressProgram(spin)

	go func() {
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				p.Send(tui.SpinnerDoneMsg{})
				return
			}
			if err != nil {
				p.Send(tui.SpinnerDoneMsg{Err: err})
				return
			}
			if progress := resp.GetProgress(); progress != nil {
				p.Send(tui.SpinnerUpdateMsg{Label: progressLabel(progress.GetPhase(), progress.GetPercent())})
			}
			if completed := resp.GetCompleted(); completed != nil {
				p.Send(tui.SpinnerDoneMsg{})
				return
			}
			if failed := resp.GetFailed(); failed != nil {
				p.Send(tui.SpinnerDoneMsg{Err: fmt.Errorf("update failed: %s", failed.GetErrorMessage())})
				return
			}
		}
	}()

	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}
	spinModel, ok := finalModel.(tui.SpinnerModel)
	if !ok {
		return fmt.Errorf("TUI error: unexpected model type %T", finalModel)
	}
	if !spinModel.Done() {
		return ErrUserCancelled
	}
	if _, spinErr := spinModel.Result(); spinErr != nil {
		return spinErr
	}
	return nil
}

// resolveArtifactPath resolves a local file path or directory to an OS update
// artifact. A direct file path is returned as-is (any extension). A directory
// is searched for a .wendy artifact the device can install.
func resolveArtifactPath(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving path: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("artifact not found: %w", err)
	}

	if !info.IsDir() {
		return absPath, nil
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		return "", fmt.Errorf("reading directory: %w", err)
	}

	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".wendy") {
			fmt.Printf("Found artifact: %s\n", name)
			return filepath.Join(absPath, name), nil
		}
	}

	return "", fmt.Errorf("no .wendy artifact found in directory: %s", absPath)
}

// artifactSuffix returns the OS-update artifact extension to use when caching
// or serving artifactURL locally: ".wendy", whether or not the URL itself
// carries that extension. wendyos-update keys off the artifact's extension,
// so the extension must survive a local download+serve round-trip — serving
// an artifact under the wrong name makes wendyos-update reject it.
func artifactSuffix(artifactURL string) string {
	return ".wendy"
}

// artifactURLPath generates a short hash prefix for the URL path.
func artifactURLPath(filePath string) string {
	h := sha256.New()
	h.Write([]byte(filePath))
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// pickDeviceTypeAndGetLatestOTA shows a device-type picker and returns the OTA
// artifact URL for the latest stable release (or latest nightly when nightly is
// true). No version picker is shown — the version is chosen automatically.
func pickDeviceTypeAndGetLatestOTA(nightly bool) (string, error) {
	fmt.Println("Fetching available devices...")

	devices, err := getAvailableDevices()
	if err != nil {
		return "", fmt.Errorf("fetching device manifest: %w", err)
	}

	var items []tui.PickerItem
	for _, dev := range devices {
		if dev.Manifest == nil {
			continue
		}
		hasOTA := false
		for _, v := range dev.Manifest.Versions {
			if v.OTAUpdatePath != "" {
				hasOTA = true
				break
			}
		}
		if !hasOTA {
			continue
		}
		latestLabel := dev.LatestVersion
		if nightly && dev.NightlyVersion != "" {
			latestLabel = dev.NightlyVersion
		}
		items = append(items, tui.PickerItem{
			Name:        dev.Name,
			Description: fmt.Sprintf("(latest: %s)", latestLabel),
			Value:       dev.Key,
		})
	}
	if len(items) == 0 {
		return "", fmt.Errorf("no devices with OTA update support found in manifest")
	}

	fmt.Println()
	key, err := pickFromItems("Select a device type", items)
	if err != nil {
		return "", err
	}

	otaURL, latestVer, err := getLatestOTAInfoForDeviceType(key, "", nightly)
	if err != nil {
		return "", fmt.Errorf("resolving OTA artifact for %q: %w", key, err)
	}
	fmt.Printf("Latest OS version: %s\n", latestVer)
	return otaURL, nil
}

// pollDeviceOnline blocks until the device at addr responds to
// GetAgentVersion or the context deadline is reached.
func pollDeviceOnline(ctx context.Context, addr string) error {
	// Give the device a few seconds to begin rebooting before polling.
	select {
	case <-ctx.Done():
		return fmt.Errorf("timed out waiting for device to come back online")
	case <-time.After(5 * time.Second):
	}
	for {
		probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
		conn, err := connectWithAutoTLS(probeCtx, addr)
		probeCancel()
		if err == nil {
			probeCtx2, probeCancel2 := context.WithTimeout(ctx, 3*time.Second)
			_, probeErr := conn.AgentService.GetAgentVersion(probeCtx2, &agentpb.GetAgentVersionRequest{})
			probeCancel2()
			conn.Close()
			if probeErr == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for device to come back online")
		case <-time.After(3 * time.Second):
		}
	}
}

// waitForDeviceOnline polls the device until it responds to GetAgentVersion,
// or until a 10-minute timeout expires. Shows a spinner when running
// interactively; polls silently otherwise. The budget must cover the rollback
// path: two reboots plus the post-update healthcheck timeouts.
func waitForDeviceOnline(ctx context.Context, host string) error {
	addr := hostPort(host, defaultAgentPort)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	if !isInteractiveTerminal() {
		return pollDeviceOnline(ctx, addr)
	}

	spin := tui.NewSpinner("Waiting for device to come back online...")
	p := tui.NewProgressProgram(spin)
	go func() {
		if err := pollDeviceOnline(ctx, addr); err != nil {
			p.Send(tui.SpinnerDoneMsg{Err: err})
		} else {
			p.Send(tui.SpinnerDoneMsg{})
		}
	}()
	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}
	_, spinErr := finalModel.(tui.SpinnerModel).Result()
	return spinErr
}

// osUpdateResultMaxAge guards against mistaking a record left over from a
// previous update attempt for the one that just completed.
const osUpdateResultMaxAge = 30 * time.Minute

// reportOSUpdateOutcome queries the freshly booted device for the outcome of
// the update (healthcheck verdict, rollback details) and prints it. It
// returns a non-nil error when the update did not stick, so the command exits
// non-zero.
func reportOSUpdateOutcome(ctx context.Context, host, preUpdateOSVersion string) error {
	addr := hostPort(host, defaultAgentPort)

	var resp *agentpb.GetOSUpdateStatusResponse
	var rpcErr error
	var postUpdateOSVersion string

	// The agent already answers GetAgentVersion (waitForDeviceOnline), so a
	// short retry window is enough to absorb transient connection hiccups.
	deadline := time.Now().Add(15 * time.Second)
	for {
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, err := connectWithAutoTLS(callCtx, addr)
		if err == nil {
			resp, rpcErr = conn.AgentService.GetOSUpdateStatus(callCtx, &agentpb.GetOSUpdateStatusRequest{})
			if ver, verErr := conn.AgentService.GetAgentVersion(callCtx, &agentpb.GetAgentVersionRequest{}); verErr == nil {
				postUpdateOSVersion = ver.GetOsVersion()
			}
			conn.Close()
		} else {
			rpcErr = err
		}
		cancel()

		// Unimplemented is definitive (older agent); anything else transient
		// is retried until the deadline.
		if rpcErr == nil || status.Code(rpcErr) == codes.Unimplemented || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	msg, outcomeErr := evaluateOSUpdateOutcome(resp, rpcErr, preUpdateOSVersion, postUpdateOSVersion, time.Now())
	fmt.Println(msg)
	reportDriverAddonsAfterUpdate(ctx, host)
	return outcomeErr
}

// evaluateOSUpdateOutcome turns the device's update-status report (or the
// failure to obtain one) into a user-facing message and, when the update did
// not stick, an error. Pure function, unit-tested.
func evaluateOSUpdateOutcome(
	resp *agentpb.GetOSUpdateStatusResponse,
	rpcErr error,
	preUpdateOSVersion, postUpdateOSVersion string,
	now time.Time,
) (string, error) {
	usable := rpcErr == nil && resp.GetHasResult() &&
		resp.GetOutcome() != agentpb.GetOSUpdateStatusResponse_OUTCOME_UNSPECIFIED &&
		now.Sub(time.Unix(resp.GetCreatedAtUnix(), 0)) <= osUpdateResultMaxAge

	if !usable {
		// The device cannot report healthcheck results for this update — the
		// new OS image may bundle an agent without healthcheck support, which
		// commits without verification. Fall back to comparing OS versions.
		switch {
		case postUpdateOSVersion == "":
			return "The update outcome could not be verified; check the device with `wendy status`.", nil
		case postUpdateOSVersion == preUpdateOSVersion:
			return fmt.Sprintf("Warning: the device is still running %s — the update was likely rolled back. "+
					"This device's agent cannot report healthcheck details; see `journalctl -u wendyos-agent` on the device.",
					postUpdateOSVersion),
				errors.New("OS version unchanged after update; the device likely rolled back")
		default:
			return fmt.Sprintf("Update applied; device is now running %s. "+
				"(Post-update health verification is not supported by this device's agent.)", postUpdateOSVersion), nil
		}
	}

	switch resp.GetOutcome() {
	case agentpb.GetOSUpdateStatusResponse_OUTCOME_COMMITTED:
		// Both versions come from wendyOSVersion() on the device, so they are
		// directly comparable. A mismatch means the record describes a commit
		// to an OS the device is not running — most likely a record from an
		// earlier attempt, or an update that did not survive the reboot.
		if postUpdateOSVersion != "" && resp.GetNewOsVersion() != "" && postUpdateOSVersion != resp.GetNewOsVersion() {
			return fmt.Sprintf("Warning: the device reports a committed update to %s but is running %s — "+
					"the status may belong to an earlier update. Check the device with `wendy status`.",
					resp.GetNewOsVersion(), postUpdateOSVersion),
				errors.New("OS update status does not match the running OS version")
		}
		runningVersion := postUpdateOSVersion
		if runningVersion == "" {
			runningVersion = resp.GetNewOsVersion()
		}
		return fmt.Sprintf("Update verified: critical services healthy. Device is running %s.", runningVersion), nil

	case agentpb.GetOSUpdateStatusResponse_OUTCOME_ROLLED_BACK:
		var b strings.Builder
		rolledBackTo := resp.GetOldOsVersion()
		if rolledBackTo == "" {
			rolledBackTo = postUpdateOSVersion
		}
		reason := classifyOSRollback(resp.GetServices(), resp.GetNote())
		switch reason {
		case rollbackReasonNotBooted:
			fmt.Fprintf(&b, "The new OS did not boot; the device fell back to %s and the update was rolled back.\n", rolledBackTo)
		case rollbackReasonHealthchecks:
			fmt.Fprintf(&b, "Update failed post-reboot healthchecks and was rolled back to %s.\n", rolledBackTo)
		default:
			fmt.Fprintf(&b, "Update was rolled back to %s.\n", rolledBackTo)
		}
		writeFailedServices(&b, resp.GetServices())
		// When the updater ran its own health gate (wendyos-update health.d),
		// there are no per-service results — the reason is carried in the note.
		if note := resp.GetNote(); note != "" {
			fmt.Fprintf(&b, "Reason: %s\n", note)
		}
		if re := resp.GetRollbackError(); re != "" {
			fmt.Fprintf(&b, "Rollback error: %s\n", re)
		}
		return strings.TrimRight(b.String(), "\n"), rolledBackError(reason)

	case agentpb.GetOSUpdateStatusResponse_OUTCOME_ROLLBACK_FAILED:
		var b strings.Builder
		reason := classifyOSRollback(resp.GetServices(), resp.GetNote())
		switch reason {
		case rollbackReasonNotBooted:
			b.WriteString("The new OS did not boot, and the automatic rollback could not be performed. " +
				"The device may be in a degraded state.\n")
		case rollbackReasonHealthchecks:
			b.WriteString("Update failed post-reboot healthchecks, and the automatic rollback could not be performed. " +
				"The device may be in a degraded state.\n")
		default:
			b.WriteString("The update did not stick, and the automatic rollback could not be performed. " +
				"The device may be in a degraded state.\n")
		}
		writeFailedServices(&b, resp.GetServices())
		// When the updater ran its own health gate (wendyos-update health.d),
		// there are no per-service results — the reason is carried in the note.
		if note := resp.GetNote(); note != "" {
			fmt.Fprintf(&b, "Reason: %s\n", note)
		}
		if re := resp.GetRollbackError(); re != "" {
			fmt.Fprintf(&b, "Rollback error: %s\n", re)
		}
		return strings.TrimRight(b.String(), "\n"), rollbackFailedError(reason)

	default: // OUTCOME_COMMIT_FAILED
		msg := "The update could not be committed: no health verdict was rendered. " +
			"The device retries the commit on its next agent restart; if it is never committed, the OS reverts on the next reboot."
		if note := resp.GetNote(); note != "" {
			msg += "\nReason: " + note
		}
		return msg, errors.New("OS update commit failed")
	}
}

// newOSUpdateStatusCmd reports the device's record of its most recent OS update
// without performing one. It is the only way to read why a commit failed when
// the device has no shell access: the gate persists the reason and re-records
// it on each agent restart while the commit keeps failing.
func newOSUpdateStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update-status",
		Short: "Show the result of the most recent WendyOS update",
		Long: `Report the device's record of its most recent OS update attempt
(committed, rolled back, or commit-failed), including the captured failure
reason. Useful for diagnosing an update without shell access to the device.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx, SuppressUpdateCheck())
			if err != nil {
				return err
			}
			defer conn.Close()

			// Ask for the live wendyos-update engine snapshot too. The persisted
			// record is written by the agent's boot-time gate and can be absent
			// (e.g. a delegated-health commit that never wrote one), but the
			// engine status is authoritative on every wendyos-update device and
			// is what lets a nightly commit be told apart from a rollback without
			// shell access — nightlies keep the same WendyOS-x.y.z version string.
			resp, err := conn.AgentService.GetOSUpdateStatus(ctx, &agentpb.GetOSUpdateStatusRequest{IncludeEngineStatus: true})
			if err != nil {
				return osUpdateStatusError(ctx, conn.AgentService, err)
			}
			if jsonOutput {
				out, mErr := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(resp)
				if mErr != nil {
					return fmt.Errorf("encoding OS update status as JSON: %w", mErr)
				}
				fmt.Println(string(out))
				return nil
			}
			fmt.Println(formatOSUpdateStatus(resp))
			if es := resp.GetEngineStatus(); es != nil {
				fmt.Println()
				fmt.Print(formatEngineStatus(es))
			}
			return nil
		},
	}
}

// osUpdateStatusError maps a GetOSUpdateStatus failure to what the user is
// told. An Unimplemented RPC means the connected agent predates the
// GetOSUpdateStatus RPC — except on a Mac agent, where OS update status is
// permanently unsupported (there is no WendyOS OTA on macOS at all), so
// telling the user to "update the agent first" would be misleading: no future
// agent update will make this RPC available there. macOSBetaUnsupportedFeatureError
// re-probes GetAgentVersion to tell the two cases apart.
func osUpdateStatusError(ctx context.Context, agent agentpb.WendyAgentServiceClient, err error) error {
	if status.Code(err) == codes.Unimplemented {
		if macErr := macOSBetaUnsupportedFeatureError(ctx, agent, err, "OS update status"); macErr != nil {
			return macErr
		}
		return fmt.Errorf("this device's agent does not report OS update status; update the agent first")
	}
	return fmt.Errorf("querying OS update status: %w", err)
}

// formatOSUpdateStatus renders a persisted OS-update record as-is, with no
// staleness or version inference (unlike evaluateOSUpdateOutcome, which judges
// a just-completed update). This keeps a past commit failure diagnosable after
// the fact.
func formatOSUpdateStatus(resp *agentpb.GetOSUpdateStatusResponse) string {
	if resp == nil || !resp.GetHasResult() {
		return "No OS update has been recorded on this device."
	}

	var b strings.Builder
	switch resp.GetOutcome() {
	case agentpb.GetOSUpdateStatusResponse_OUTCOME_COMMITTED:
		b.WriteString("Last OS update: committed (healthchecks passed).\n")
	case agentpb.GetOSUpdateStatusResponse_OUTCOME_ROLLED_BACK:
		switch classifyOSRollback(resp.GetServices(), resp.GetNote()) {
		case rollbackReasonNotBooted:
			b.WriteString("Last OS update: rolled back — the new OS did not boot.\n")
		case rollbackReasonHealthchecks:
			b.WriteString("Last OS update: rolled back after failed healthchecks.\n")
		default:
			b.WriteString("Last OS update: rolled back.\n")
		}
	case agentpb.GetOSUpdateStatusResponse_OUTCOME_ROLLBACK_FAILED:
		switch classifyOSRollback(resp.GetServices(), resp.GetNote()) {
		case rollbackReasonNotBooted:
			b.WriteString("Last OS update: the new OS did not boot and the rollback could not be performed.\n")
		case rollbackReasonHealthchecks:
			b.WriteString("Last OS update: healthchecks failed and the rollback could not be performed.\n")
		default:
			b.WriteString("Last OS update: the update did not stick and the rollback could not be performed.\n")
		}
	case agentpb.GetOSUpdateStatusResponse_OUTCOME_COMMIT_FAILED:
		b.WriteString("Last OS update: the commit did not complete (no health verdict was rendered); it will be retried.\n")
	default:
		b.WriteString("Last OS update: outcome unknown.\n")
	}

	if v := resp.GetOldOsVersion(); v != "" {
		fmt.Fprintf(&b, "  Previous version: %s\n", v)
	}
	if v := resp.GetNewOsVersion(); v != "" {
		fmt.Fprintf(&b, "  Update version:   %s\n", v)
	}
	writeFailedServices(&b, resp.GetServices())
	if note := resp.GetNote(); note != "" {
		fmt.Fprintf(&b, "Reason: %s\n", note)
	}
	if re := resp.GetRollbackError(); re != "" {
		fmt.Fprintf(&b, "Rollback error: %s\n", re)
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatEngineStatus renders the live wendyos-update engine snapshot: the
// booted slot and its health, and any pending (un-committed) update. This is
// the ground truth for "did the update stick?" on a device with no shell
// access, and unlike the OS version string it distinguishes a committed
// nightly from a rolled-back one.
func formatEngineStatus(es *agentpb.OSUpdateEngineStatus) string {
	var b strings.Builder
	b.WriteString("Engine status (wendyos-update):\n")
	if c := es.GetConnector(); c != "" {
		fmt.Fprintf(&b, "  Connector:    %s\n", c)
	}
	if cs := es.GetCurrentSlot(); cs != "" {
		fmt.Fprintf(&b, "  Booted slot:  %s\n", cs)
	}
	for _, s := range es.GetSlots() {
		marker := " "
		if s.GetBooted() {
			marker = "*"
		}
		fmt.Fprintf(&b, "  %s slot %s", marker, s.GetSlot())
		if h := s.GetRootfsHealth(); h != "" {
			fmt.Fprintf(&b, " health=%s", h)
		}
		if d := s.GetDistro(); d != "" {
			fmt.Fprintf(&b, " distro=%s", d)
		}
		if r := s.GetRetries(); r != "" {
			fmt.Fprintf(&b, " retries=%s", r)
		}
		if n := s.GetNote(); n != "" {
			fmt.Fprintf(&b, " note=%q", n)
		}
		b.WriteString("\n")
	}
	if p := es.GetPending(); p != nil {
		fmt.Fprintf(&b, "  Pending update: %s", p.GetArtifactName())
		if v := p.GetArtifactVersion(); v != "" {
			fmt.Fprintf(&b, " (%s)", v)
		}
		if ph := p.GetPhase(); ph != "" {
			fmt.Fprintf(&b, " phase=%s", ph)
		}
		if ts := p.GetTargetSlot(); ts != "" {
			fmt.Fprintf(&b, " target_slot=%s", ts)
		}
		b.WriteString("\n")
	}
	if d := es.GetDiagnostics(); len(d) > 0 {
		b.WriteString("  Diagnostics (raw):\n")
		for _, k := range sortedKeys(d) {
			fmt.Fprintf(&b, "    %s = %s\n", k, d[k])
		}
	}
	return b.String()
}

// sortedKeys returns a map's keys in lexical order so diagnostic output is
// stable across calls (Go map iteration order is randomized).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// osRollbackReason classifies why an OS update was rolled back. The CLI used to
// state "critical services failed healthchecks" for every rollback, which is
// false whenever the update backend runs its own health gate: wendyos-update's
// boot verifier marks a deployment failed *before* commit when the new slot
// never booted, so /etc/wendyos-update/health.d never runs at all. Reporting a
// healthcheck failure then points the reader at the wrong layer entirely
// (WDY-2200).
type osRollbackReason int

const (
	// rollbackReasonUnknown: the record does not say why. The CLI reports the
	// rollback and the captured reason without naming a cause of its own.
	rollbackReasonUnknown osRollbackReason = iota
	// rollbackReasonHealthchecks: a critical service failed, so the agent's own
	// CheckAll rejected the update. This is a genuine healthcheck failure.
	rollbackReasonHealthchecks
	// rollbackReasonNotBooted: the new OS never produced a healthy boot — the
	// firmware fell back to the old slot, or the deployment was already marked
	// failed — so no healthcheck was ever reached.
	rollbackReasonNotBooted
)

// updaterNeverBootedMarkers are substrings of a wendyos-update commit rejection
// that mean the new slot never booted. They are matched on the note because the
// exit code cannot distinguish these cases: the CLI contract's exit 4 covers
// both platform-verification and health.d failures, while an already-marked-
// failed deployment exits 1. An unmatched note yields rollbackReasonUnknown,
// which asserts nothing — the property that matters is that the CLI never claims
// a healthcheck failure it cannot substantiate.
//
// A structured reason field on the agent's status response would remove this
// matching entirely; until then, matching the note keeps every already-deployed
// agent honest, because they all report it.
var updaterNeverBootedMarkers = []string{
	"is marked failed",  // engine.Commit: pending update <x> is marked failed
	"firmware fallback", // engine.Commit / VerifyBoot: booted slot != target slot
	"never swapped",     // engine.Commit: written but never swapped
}

// updaterHealthcheckMarker matches engine.HookError for the gating health phase,
// which formats as `health hook "<name>" failed: <err>`. The delegated path
// reports no per-service results, so without this a genuine
// /etc/wendyos-update/health.d rejection would fall through to
// rollbackReasonUnknown and discard the one thing the record does establish.
const updaterHealthcheckMarker = "health hook"

// classifyOSRollback determines why an update was rolled back. A failed service
// is decisive: only the agent-run CheckAll path populates service results, and
// it records only failures it actually observed. The never-booted markers are
// checked before the health marker because the boot verifier marks a deployment
// failed before commit ever reaches health.d, so a record carrying both is a
// never-booted one.
func classifyOSRollback(services []*agentpb.GetOSUpdateStatusResponse_ServiceResult, note string) osRollbackReason {
	for _, svc := range services {
		if svc.GetStatus() == agentpb.GetOSUpdateStatusResponse_ServiceResult_STATUS_FAILED {
			return rollbackReasonHealthchecks
		}
	}
	lowered := strings.ToLower(note)
	for _, marker := range updaterNeverBootedMarkers {
		if strings.Contains(lowered, marker) {
			return rollbackReasonNotBooted
		}
	}
	if strings.Contains(lowered, updaterHealthcheckMarker) {
		return rollbackReasonHealthchecks
	}
	return rollbackReasonUnknown
}

// rolledBackError is the exit error for a rolled-back update, worded to match
// what the record actually supports.
func rolledBackError(reason osRollbackReason) error {
	switch reason {
	case rollbackReasonNotBooted:
		return errors.New("OS update rolled back: the new OS did not boot")
	case rollbackReasonHealthchecks:
		return errors.New("OS update rolled back: critical services failed healthchecks")
	default:
		return errors.New("OS update rolled back")
	}
}

// rollbackFailedError is the exit error for an update that failed and could not
// be rolled back.
func rollbackFailedError(reason osRollbackReason) error {
	switch reason {
	case rollbackReasonNotBooted:
		return errors.New("the new OS did not boot and automatic rollback did not run")
	case rollbackReasonHealthchecks:
		return errors.New("OS update healthchecks failed and automatic rollback did not run")
	default:
		return errors.New("OS update did not stick and automatic rollback did not run")
	}
}

func writeFailedServices(b *strings.Builder, services []*agentpb.GetOSUpdateStatusResponse_ServiceResult) {
	header := false
	for _, svc := range services {
		if svc.GetStatus() != agentpb.GetOSUpdateStatusResponse_ServiceResult_STATUS_FAILED {
			continue
		}
		if !header {
			b.WriteString("Failed services:\n")
			header = true
		}
		fmt.Fprintf(b, "  - %s: %s\n", svc.GetUnit(), svc.GetReason())
	}
}

func localIPForHost(host string) (string, error) {
	// Strip port if present.
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	// dialHost is what we pass to net.Dial (may include an IPv6 zone).
	dialHost := host

	// Detect whether the host is already an IP literal. For IPv6 link-local
	// addresses the zone identifier (e.g. "%en0") must be stripped before
	// calling net.ParseIP, but preserved for dialing.
	var parsedIP net.IP
	if i := strings.Index(host, "%"); i != -1 {
		parsedIP = net.ParseIP(host[:i])
	} else {
		parsedIP = net.ParseIP(host)
	}

	if parsedIP == nil {
		// Not an IP literal — resolve via DNS, falling back to an mDNS browse
		// for ".local" names. The shipped CGO_ENABLED=0 binary can't resolve
		// ".local" via the OS resolver, so without this fallback `wendy os`
		// commands targeting a ".local" host fail on Linux/Windows (issue #1155).
		ip := resolveHostMDNSFallback(context.Background(), host)
		if ip == "" {
			return "", fmt.Errorf("resolving %s: no addresses found%s", host, mdnsLocalHint(host))
		}
		// An mDNS-discovered IPv6 link-local address carries a zone (e.g.
		// fe80::1%en0): keep it for dialing but strip it before ParseIP.
		dialHost = ip
		ipForParse := ip
		if i := strings.Index(ip, "%"); i != -1 {
			ipForParse = ip[:i]
		}
		parsedIP = net.ParseIP(ipForParse)
		if parsedIP == nil {
			return "", fmt.Errorf("resolving %s: invalid address %q", host, ip)
		}
	}

	network := "udp4"
	if parsedIP.To4() == nil {
		network = "udp6"
	}
	dialAddr := net.JoinHostPort(dialHost, fmt.Sprintf("%d", defaultAgentPort))

	// Use UDP dial to determine which local IP would be used to reach the target.
	// No actual packets are sent — this just queries the routing table.
	conn, err := net.Dial(network, dialAddr)
	if err != nil {
		return "", fmt.Errorf("determining route to %s: %w", dialHost, err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	if localAddr.Zone != "" {
		return localAddr.IP.String() + "%" + localAddr.Zone, nil
	}
	return localAddr.IP.String(), nil
}

// ipForURL converts a local IP (possibly "fe80::1%en0") to the host component
// for an HTTP URL. IPv6 zone IDs are percent-encoded per RFC 6874 so the raw
// '%' does not produce an invalid URL.
func ipForURL(ip string) string {
	if i := strings.Index(ip, "%"); i != -1 {
		return ip[:i] + "%25" + ip[i+1:]
	}
	return ip
}

// drainOSUpdateStream reads all messages from an UpdateOS stream without a
// TUI, printing phase label changes to stderr. Used when stdout is not a TTY.
func drainOSUpdateStream(stream agentpb.WendyAgentService_UpdateOSClient) error {
	var lastPhase string
	lastDecile := int32(-1)
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if progress := resp.GetProgress(); progress != nil {
			phase, percent := progress.GetPhase(), progress.GetPercent()
			// Print on every phase change, and at each 10% step within a phase,
			// so non-interactive logs show steady progress without flooding.
			decile := percent / 10
			if phase != lastPhase || (percent > 0 && decile != lastDecile) {
				fmt.Fprintln(os.Stderr, progressLabel(phase, percent))
				lastPhase = phase
				lastDecile = decile
			}
		}
		if resp.GetCompleted() != nil {
			return nil
		}
		if failed := resp.GetFailed(); failed != nil {
			return fmt.Errorf("update failed: %s", failed.GetErrorMessage())
		}
	}
}

// phaseLabel converts an update-progress phase string to a user-friendly
// spinner label.
func phaseLabel(phase string) string {
	switch phase {
	case "downloading":
		return "Downloading update..."
	case "installing":
		return "Installing update..."
	case "finalizing":
		return "Finalizing..."
	default:
		if phase != "" {
			return strings.ToUpper(phase[:1]) + phase[1:] + "..."
		}
		return "Updating WendyOS..."
	}
}

// progressLabel renders a phase plus its percentage when the agent reports one,
// e.g. "Installing update (42%)". A percent of 0 is treated as "unknown" and
// the bare phase label is shown. This surfaces real progress during the long
// download/install so the operation doesn't look hung.
func progressLabel(phase string, percent int32) string {
	label := phaseLabel(phase)
	if percent > 0 {
		return strings.TrimSuffix(label, "...") + fmt.Sprintf(" (%d%%)", percent)
	}
	return label
}

// ensureAgentUpToDate checks the agent version on the device against the latest
// available version, resolved GCS-first with a GitHub releases fallback (see
// resolveAgentVersion). If the device is behind, it downloads the latest binary,
// uploads it (causing the agent to restart), waits for it to come back, and
// returns a fresh connection. If the agent is already current or the check fails
// non-fatally, the original connection is returned unchanged.
func ensureAgentUpToDate(ctx context.Context, conn *grpcclient.AgentConnection, versionResp *agentpb.GetAgentVersionResponse, nightly bool) (*grpcclient.AgentConnection, error) {
	agentVer := versionResp.GetVersion()
	arch := versionResp.GetCpuArchitecture()
	osName := versionResp.GetOs()

	fmt.Printf("Agent version: %s — checking for updates...\n", agentVer)

	latestVer, _, err := resolveAgentVersion(nightly)
	if err != nil {
		fmt.Printf("Could not check for agent updates: %v\n", err)
		return conn, nil
	}

	// For nightly builds, update whenever the device isn't already running that
	// exact tag — a semver comparison would incorrectly treat nightly pre-release
	// tags as older than a stable release of the same base version.
	alreadyCurrent := nightly && latestVer == agentVer ||
		!nightly && version.CompareVersions(latestVer, agentVer) <= 0
	if alreadyCurrent {
		fmt.Printf("Agent is up to date (%s)\n", agentVer)
		return conn, nil
	}

	fmt.Printf("Updating agent: %s → %s\n", agentVer, latestVer)
	if err := performAgentUpdate(ctx, conn, osName, arch, nightly); err != nil {
		return nil, fmt.Errorf("agent update failed: %w", err)
	}
	conn.Close()

	fmt.Print("Waiting for agent to restart...")
	newConn, err := reconnectAgentAfterRestart(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("agent did not come back after update: %w", err)
	}
	fmt.Println(" done.")
	return newConn, nil
}

// serveLocalArtifact starts a temporary HTTP server bound to localIP that
// serves the file at localPath. It returns the URL at which the file is
// accessible and a cleanup function that shuts down the server.
func serveLocalArtifact(localPath, localIP string) (string, func(), error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(localIP, "0"))
	if err != nil {
		return "", nil, fmt.Errorf("starting file server: %w", err)
	}

	_, portStr, _ := net.SplitHostPort(listener.Addr().String())
	fileName := filepath.Base(localPath)
	escapedFileName := url.PathEscape(fileName)
	urlPath := artifactURLPath(localPath)

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		listener.Close()
		return "", nil, fmt.Errorf("generating token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	servedURL := "http://" + net.JoinHostPort(ipForURL(localIP), portStr) + "/" + urlPath + "/" + token + "/" + escapedFileName

	mux := http.NewServeMux()
	mux.HandleFunc("/"+urlPath+"/"+token+"/"+escapedFileName, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, localPath)
	})
	server := &http.Server{Handler: mux}
	go server.Serve(listener) //nolint:errcheck

	cleanup := func() {
		server.Close()
		listener.Close()
	}
	return servedURL, cleanup, nil
}

// downloadArtifactToTemp downloads a remote artifact URL to a temporary file,
// showing a progress bar. The caller is responsible for removing the file.
func downloadArtifactToTemp(artifactURL string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Get(artifactURL) //nolint:noctx
	if err != nil {
		return "", fmt.Errorf("downloading: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	cacheDir, err := osCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolving cache dir: %w", err)
	}
	// Preserve the artifact's real extension (.wendy) so the locally served
	// filename matches the artifact type; the device's install backend keys
	// off it.
	tmpFile, err := os.CreateTemp(cacheDir, "wendyos-*"+artifactSuffix(artifactURL))
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}

	total := resp.ContentLength
	prog := tui.NewProgress("Downloading artifact...")
	p := tui.NewProgressProgram(prog)

	go func() {
		var downloaded int64
		buf := make([]byte, 64*1024)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				if _, writeErr := tmpFile.Write(buf[:n]); writeErr != nil {
					p.Send(tui.ProgressDoneMsg{Err: writeErr})
					return
				}
				downloaded += int64(n)
				if total > 0 {
					p.Send(tui.ProgressUpdateMsg{
						Percent: float64(downloaded) / float64(total),
						Written: downloaded,
						Total:   total,
					})
				}
			}
			if readErr == io.EOF {
				p.Send(tui.ProgressDoneMsg{})
				return
			}
			if readErr != nil {
				p.Send(tui.ProgressDoneMsg{Err: readErr})
				return
			}
		}
	}()

	finalModel, err := p.Run()
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("progress TUI: %w", err)
	}

	// Comma-ok rather than a bare assertion: a bubbletea program can return a
	// different final model (e.g. when it is killed), and panicking mid-download
	// would leave the temp file behind with no diagnosable error.
	model, ok := finalModel.(tui.ProgressModel)
	if !ok {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("progress TUI returned an unexpected model %T", finalModel)
	}
	if model.Err() != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", model.Err()
	}

	tmpFile.Close()
	return tmpFile.Name(), nil
}

// deviceHasWiFi returns true if the device reports an active WiFi connection.
// On error (e.g. older firmware that doesn't support the call) it returns true
// so we fall back to the GCP URL rather than breaking the update flow.
func deviceHasWiFi(ctx context.Context, conn *grpcclient.AgentConnection) bool {
	status, err := conn.AgentService.GetWiFiStatus(ctx, &agentpb.GetWiFiStatusRequest{})
	if err != nil {
		return true
	}
	return status.GetConnected()
}
