package commands

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// A driver add-on is built for one exact kernel, so an OS update that bumps it
// stops the add-on merging — visible only in the device journal. The pre-flight
// stages each rebuild ahead of the reboot and reports what it could not stage;
// the caller decides whether to go ahead. The post-update report is advisory.

var driverExtensionsForFn = driverExtensionsFor

// driverListTimeout bounds reading the installed set. Short: the answer gates an
// OS update, so a slow reply has to become a decision rather than a hang.
const driverListTimeout = 5 * time.Second

// driverStageTimeout bounds one add-on fetch. Generous because the agent
// downloads the image, but bounded so a stalled registry cannot hold up an OTA.
const driverStageTimeout = 10 * time.Minute

// driverPreflight is what the pre-update check concluded. Anything recorded here
// means the device is about to update without a driver it has today.
type driverPreflight struct {
	// unreadable is set when the installed set could not be read at all, which is
	// the worst case: there may be no add-ons, or there may be a critical one.
	unreadable string
	unstaged   []unstagedAddon
}

type unstagedAddon struct{ name, reason string }

func (p driverPreflight) blocking() bool {
	return p.unreadable != "" || len(p.unstaged) > 0
}

// installedDriverAddons reads the device's add-ons. Every inability to inspect
// the store is an error: older agents may hide the service before enrollment,
// while the device retains add-ons installed before it was reset.
// "Could not ask" and "has none" must never look alike to an OTA pre-flight.
func installedDriverAddons(ctx context.Context, conn *grpcclient.AgentConnection) (*agentpbv2.ListDriversResponse, error) {
	if conn == nil || conn.DriverService == nil {
		return nil, fmt.Errorf("the device's driver service is unavailable on this connection")
	}
	callCtx, cancel := context.WithTimeout(ctx, driverListTimeout)
	defer cancel()
	resp, err := conn.DriverService.ListDrivers(callCtx, &agentpbv2.ListDriversRequest{})
	if status.Code(err) == codes.Unimplemented {
		return nil, driverServiceErr(err)
	}
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// warnDriverAddonsBeforeUpdate names the installed add-ons and what this update
// may do to them. An empty targetVersion (a local artifact or --artifact-url
// resolves no manifest) limits it to the generic warning.
func warnDriverAddonsBeforeUpdate(ctx context.Context, conn *grpcclient.AgentConnection, deviceType, targetVersion, driversDir string, pr int, stageDrivers bool) driverPreflight {
	var pf driverPreflight
	dl, err := installedDriverAddons(ctx, conn)
	if err != nil {
		pf.unreadable = err.Error()
		cliLogln("%s", tui.WarningMessage("Could not read the installed driver add-ons: "+err.Error()))
		return pf
	}
	installed := dl.GetInstalled()

	if len(installed) > 0 {
		names := make([]string, 0, len(installed))
		for _, d := range installed {
			names = append(names, d.GetName())
		}
		cliLogln("%s", tui.WarningMessage(fmt.Sprintf("%s installed: %s.",
			countAddons(len(names)), strings.Join(names, ", "))))
		// Stated as a rule, not as a fact about these add-ons: some may already be
		// stale, which is precisely the state this warning exists for.
		cliLogln("An add-on is built for one exact kernel and stops loading if this update changes it.")
		for _, d := range installed {
			if driverStale(d, dl.GetKernelVersion()) {
				cliLogln("  %s: already not usable on kernel %s", d.GetName(), dl.GetKernelVersion())
			}
		}
	}

	atRisk := driversNeedingKernelCoverage(installed)
	if len(atRisk) == 0 {
		// Files the operator supplied are also how a fresh air-gapped device is
		// pre-loaded, so stage them even though there is nothing installed to lose.
		if driversDir != "" {
			stageFromDir(ctx, conn, installed, driversDir, "", "", &pf)
		}
		return pf
	}

	// The release's add-on metadata is also the only pre-install statement of
	// which kernel the OTA boots. A single pinned kernel is proof; no metadata or
	// several kernels is ambiguous and must not be reported as safe.
	targetKernel, exts, targetErr := releaseTargetKernel(deviceType, targetVersion, pr)

	// Files the operator supplied win over registry downloads. When the release
	// kernel is known, pass it to the agent so the image is checked against the OTA
	// target rather than merely filed under whatever kernel it declares. When it
	// is unknown, staging still helps an offline operator, but the verdict remains
	// blocking until they explicitly accept that it could not be verified.
	if driversDir != "" {
		stageFromDir(ctx, conn, installed, driversDir, targetKernel, targetErr, &pf)
		return pf
	}

	if targetErr != "" {
		cliLogln("  target kernel could not be determined: %s", targetErr)
		if stageDrivers {
			for _, d := range atRisk {
				pf.unstaged = append(pf.unstaged, unstagedAddon{d.GetName(), targetErr})
			}
		}
		return pf
	}

	byName := map[string][]extensionEntry{}
	for _, e := range exts {
		byName[e.Name] = append(byName[e.Name], e)
	}
	for _, d := range atRisk {
		name := d.GetName()
		switch {
		case targetKernel == dl.GetKernelVersion():
			cliLogln("  %s: unaffected — %s keeps kernel %s", name, targetVersion, targetKernel)
		case !hasDriverForKernel(byName[name], targetKernel):
			cliLogln("  %s: no rebuild published for %s", name, targetVersion)
			if stageDrivers {
				pf.unstaged = append(pf.unstaged, unstagedAddon{name, "no rebuild published for " + targetVersion})
			}
		case stageDrivers:
			if reason := stageRebuild(ctx, conn, name, targetKernel, byName[name]); reason != "" {
				pf.unstaged = append(pf.unstaged, unstagedAddon{name, reason})
			}
		default:
			// --no-drivers: the operator has already accepted this, so it is
			// reported but never blocks.
			cliLogln("  %s: rebuild published for kernel %s — reinstall after the update", name, targetKernel)
		}
	}
	return pf
}

func driversNeedingKernelCoverage(installed []*agentpbv2.InstalledDriver) []*agentpbv2.InstalledDriver {
	atRisk := make([]*agentpbv2.InstalledDriver, 0, len(installed))
	for _, d := range installed {
		if d.GetKernelVersion() != "" || d.GetUnreadable() {
			atRisk = append(atRisk, d)
		}
	}
	return atRisk
}

// releaseTargetKernel resolves the one kernel represented by a release's driver
// metadata. The OS manifest does not yet carry a separate kernel field, so more
// than one pinned kernel is not enough evidence to declare an OTA safe.
func releaseTargetKernel(deviceType, targetVersion string, pr int) (string, []extensionEntry, string) {
	if deviceType == "" || targetVersion == "" {
		return "", nil, "this update resolves no manifest, so its target kernel cannot be verified"
	}
	exts, err := driverExtensionsForFn(deviceType, targetVersion, pr)
	if err != nil {
		return "", nil, fmt.Sprintf("could not read the published add-ons: %v", err)
	}
	set := map[string]bool{}
	for _, e := range exts {
		if e.KernelVersion != "" {
			set[e.KernelVersion] = true
		}
	}
	kernels := make([]string, 0, len(set))
	for kernel := range set {
		kernels = append(kernels, kernel)
	}
	sort.Strings(kernels)
	switch len(kernels) {
	case 0:
		return "", exts, "the target release publishes no kernel metadata"
	case 1:
		return kernels[0], exts, ""
	default:
		return "", exts, fmt.Sprintf("the target release publishes several kernels (%s), so the one this update boots is ambiguous", strings.Join(kernels, ", "))
	}
}

func hasDriverForKernel(entries []extensionEntry, kernel string) bool {
	for _, e := range entries {
		if e.KernelVersion == kernel {
			return true
		}
	}
	return false
}

// stageRebuild puts the published rebuild in place for the kernel the update is
// about to boot into, so the add-on loads on the first boot of the new slot.
// Returns "" on success, or why it could not be staged.
func stageRebuild(ctx context.Context, conn *grpcclient.AgentConnection, name, targetKernel string, entries []extensionEntry) string {
	var matches []extensionEntry
	for _, e := range entries {
		if e.KernelVersion == targetKernel {
			matches = append(matches, e)
		}
	}
	if len(matches) != 1 {
		reason := fmt.Sprintf("the manifest has %d rebuilds for target kernel %s", len(matches), targetKernel)
		cliLogln("  %s: %s", name, reason)
		return reason
	}
	e := matches[0]
	if e.Path == "" {
		reason := "the published rebuild has no artifact in the manifest"
		cliLogln("  %s: %s", name, reason)
		return reason
	}
	sig, err := base64.StdEncoding.DecodeString(e.Signature)
	if err != nil {
		reason := "the published rebuild has an unreadable signature"
		cliLogln("  %s: %s", name, reason)
		return reason
	}
	spec := &agentpbv2.DriverSpec{
		Name:          e.Name,
		Version:       e.Version,
		KernelVersion: e.KernelVersion,
		Sha256:        e.SHA256,
		Signature:     sig,
		ArtifactUrl:   gcsBaseURL + "/" + e.Path,
		ModulesLoad:   e.ModulesLoad,
		StageOnly:     true,
	}
	if err := runDriverStage(ctx, conn, spec); err != nil {
		reason := fmt.Sprintf("staging failed: %v", err)
		cliLogln("  %s: %s", name, reason)
		return reason
	}
	cliLogln("  %s: staged for kernel %s — it will load on the first boot of the new slot", name, e.KernelVersion)
	return ""
}

// runDriverStage drives the install stream to completion for a staged add-on.
// Isolated devices receive the published image over the existing connection.
func runDriverStage(ctx context.Context, conn *grpcclient.AgentConnection, spec *agentpbv2.DriverSpec) error {
	callCtx, cancel := context.WithTimeout(ctx, driverStageTimeout)
	defer cancel()
	spec, path, cleanup, err := prepareDriverUpload(callCtx, conn, spec)
	if err != nil {
		return err
	}
	defer cleanup()
	stream, err := conn.DriverService.InstallDriver(callCtx)
	if err != nil {
		return err
	}
	if err := stream.Send(&agentpbv2.InstallDriverRequest{
		RequestType: &agentpbv2.InstallDriverRequest_Spec{Spec: spec},
	}); err != nil {
		return err
	}
	if path != "" {
		if err := streamDriverChunks(stream, path); err != nil {
			return err
		}
	}
	if err := stream.CloseSend(); err != nil {
		return err
	}
	return awaitDriverApply(stream)
}

// awaitDriverApply drains an apply stream to its verdict.
func awaitDriverApply(stream agentpbv2.WendyDriverService_InstallDriverClient) error {
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("stream ended without a result")
		}
		if err != nil {
			return err
		}
		if f := resp.GetFailed(); f != nil {
			return errors.New(f.GetErrorMessage())
		}
		if resp.GetCompleted() != nil {
			return nil
		}
	}
}

// reportDriverAddonsAfterUpdate reports which add-ons survived the update. It
// dials its own connection: the outcome is already decided, so failing to
// connect here must cost nothing.
func reportDriverAddonsAfterUpdate(ctx context.Context, prior *grpcclient.AgentConnection) {
	callCtx, cancel := context.WithTimeout(ctx, driverListTimeout)
	defer cancel()
	conn, err := dialOSUpdateAgent(callCtx, prior)
	if err != nil {
		return
	}
	defer conn.Close()

	// Unlike the pre-flight, silence is right here: the update is already applied
	// and the agent may still be starting after the reboot.
	dl, err := installedDriverAddons(callCtx, conn)
	if err != nil || len(dl.GetInstalled()) == 0 {
		return
	}
	var stale []string
	for _, d := range dl.GetInstalled() {
		if driverStale(d, dl.GetKernelVersion()) {
			stale = append(stale, d.GetName())
		}
	}
	if len(stale) == 0 {
		cliLogln("%s still match the running kernel (%s).", countAddons(len(dl.GetInstalled())), dl.GetKernelVersion())
		return
	}
	cliLogln("%s", tui.WarningMessage(fmt.Sprintf("%s no longer built for the running kernel (%s): %s.",
		countAddons(len(stale)), dl.GetKernelVersion(), strings.Join(stale, ", "))))
	cliLogln("Reinstall with %s", tui.Command("wendy device drivers install <name>"))
}

func countAddons(n int) string {
	if n == 1 {
		return "1 driver add-on"
	}
	return fmt.Sprintf("%d driver add-ons", n)
}

// confirmDriverPreflight turns the pre-flight verdict into a decision. The tool
// cannot tell a critical NIC driver from a webcam, so it asks rather than guessing.
func confirmDriverPreflight(pf driverPreflight) error {
	cliLogln("")
	if pf.unreadable != "" {
		cliLogln("%s", tui.WarningMessage("The device's driver add-ons could not be read, so this update may remove one."))
	} else {
		cliLogln("%s", tui.WarningMessage(fmt.Sprintf("%s will not be in place after this update:", countAddons(len(pf.unstaged)))))
		for _, u := range pf.unstaged {
			cliLogln("    %s — %s", u.name, u.reason)
		}
	}

	if !isInteractiveTerminal() {
		return fmt.Errorf("aborted: the device would update without %s (re-run with --no-drivers to update anyway)",
			driverPreflightSubject(pf))
	}
	confirmed, err := tui.Confirm("Continue with the update anyway?")
	if err != nil {
		if errors.Is(err, tui.ErrCancelled) {
			return ErrUserCancelled
		}
		return err
	}
	if !confirmed {
		cliNotice("Cancelled.")
		return ErrUserCancelled
	}
	return nil
}

func driverPreflightSubject(pf driverPreflight) string {
	if pf.unreadable != "" {
		return "a driver add-on it may have (the installed set could not be read)"
	}
	names := make([]string, 0, len(pf.unstaged))
	for _, u := range pf.unstaged {
		names = append(names, u.name)
	}
	return strings.Join(names, ", ")
}

// checkDriversDir rejects a --drivers-dir the update cannot honour, before
// anything transfers. A directory with no images is a wrong path, not a verdict
// about the device, so it fails outright rather than becoming a prompt.
func checkDriversDir(conn *grpcclient.AgentConnection, dir string) error {
	if conn == nil || conn.DriverService == nil {
		return fmt.Errorf("--drivers-dir needs the device's driver service; enrol the device first")
	}
	files, err := driverRawsIn(dir)
	if err != nil {
		return fmt.Errorf("--drivers-dir %s: %w", dir, err)
	}
	if len(files) == 0 {
		return fmt.Errorf("--drivers-dir %s contains no .raw driver add-ons", dir)
	}
	return nil
}

func driverRawsIn(dir string) ([]string, error) {
	return filepath.Glob(filepath.Join(dir, "*.raw"))
}

// stageFromDir stages every add-on image in dir, so an air-gapped update can put
// the next kernel's drivers in place. When targetKernel is known the agent checks
// every pinned image against it. Otherwise images can still be pre-loaded into
// their self-declared buckets, but an installed driver remains unverified and
// blocks the OTA until the operator explicitly accepts the risk.
func stageFromDir(ctx context.Context, conn *grpcclient.AgentConnection, installed []*agentpbv2.InstalledDriver, dir, targetKernel, targetErr string, pf *driverPreflight) {
	files, err := driverRawsIn(dir)
	if err != nil {
		pf.unreadable = err.Error()
		cliLogln("%s", tui.WarningMessage("Could not read "+dir+": "+err.Error()))
		return
	}
	cliLogln("Staging driver add-ons from %s", tui.Path(dir))
	present := make(map[string]bool, len(files))
	staged := make(map[string]bool, len(files))
	for _, path := range files {
		name := strings.TrimSuffix(filepath.Base(path), ".raw")
		present[name] = true
		if err := stageLocalRebuild(ctx, conn, name, path, targetKernel); err != nil {
			reason := fmt.Sprintf("staging failed: %v", err)
			cliLogln("  %s: %s", name, reason)
			pf.unstaged = append(pf.unstaged, unstagedAddon{name, reason})
			continue
		}
		staged[name] = true
		if targetKernel == "" {
			cliLogln("  %s: staged from %s (OTA target kernel not verified)", name, filepath.Base(path))
		} else {
			cliLogln("  %s: staged from %s for target kernel %s", name, filepath.Base(path), targetKernel)
		}
	}
	for _, d := range installed {
		name := d.GetName()
		if !present[name] {
			reason := fmt.Sprintf("no %s.raw in %s", name, dir)
			cliLogln("  %s: %s", name, reason)
			pf.unstaged = append(pf.unstaged, unstagedAddon{name, reason})
			continue
		}
		if !staged[name] || (d.GetKernelVersion() == "" && !d.GetUnreadable()) {
			continue // already failed above, or an unpinned add-on needs no kernel proof
		}
		if targetKernel == "" {
			reason := targetErr
			if reason == "" {
				reason = "the OTA target kernel cannot be verified"
			}
			cliLogln("  %s: staged, but %s", name, reason)
			pf.unstaged = append(pf.unstaged, unstagedAddon{name, reason})
		}
	}
}

// stageLocalRebuild streams one local .raw to the agent for staging. The digest
// is computed here so the agent can verify what it received, exactly as
// `drivers install --file` does.
func stageLocalRebuild(ctx context.Context, conn *grpcclient.AgentConnection, name, path, targetKernel string) error {
	sum, err := sha256File(path)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, driverStageTimeout)
	defer cancel()
	stream, err := conn.DriverService.InstallDriver(callCtx)
	if err != nil {
		return err
	}
	// KernelVersion binds the local image to the OTA target when release metadata
	// made that target knowable. If it is empty the agent still files the image by
	// its self-declared kernel, but the caller keeps the pre-flight blocking.
	if err := stream.Send(&agentpbv2.InstallDriverRequest{
		RequestType: &agentpbv2.InstallDriverRequest_Spec{Spec: &agentpbv2.DriverSpec{
			Name:          name,
			KernelVersion: targetKernel,
			Sha256:        sum,
			StageOnly:     true,
		}},
	}); err != nil {
		return err
	}
	if err := streamDriverChunks(stream, path); err != nil {
		return err
	}
	if err := stream.CloseSend(); err != nil {
		return err
	}
	return awaitDriverApply(stream)
}
