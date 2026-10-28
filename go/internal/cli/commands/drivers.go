package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// newDriversCmd is the "drivers" command group under `wendy device`, mirroring
// `wendy device apps`. Driver add-ons are prebuilt, signed systemd-sysext images
// applied on top of the immutable /usr; see the agent's WendyDriverService.
func newDriversCmd() *cobra.Command {
	var source driverManifestOptions
	cmd := &cobra.Command{
		Use:     "drivers",
		Aliases: []string{"driver"},
		Short:   "Manage kernel driver add-ons on the target device",
		Long: "Interactively select a driver to install, reinstall, or remove. With --json or\n" +
			"without an interactive terminal, lists installed drivers and rejects --nightly\n" +
			"and --pr. Use list --available to list published drivers, and install or remove\n" +
			"for explicit actions.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := source.validate(cmd); err != nil {
				return err
			}
			if jsonOutput || !isInteractiveTerminal() {
				// The installed list is the same whatever the catalog, so a channel
				// flag here would be silently dropped.
				if source.nightly || source.pr > 0 {
					return usageErrorf("--nightly and --pr need the interactive picker; with --json or without a terminal, use list --available")
				}
				return runDriversList(cmd.Context(), false, source)
			}
			return runDriversInteractive(cmd.Context(), source)
		},
	}
	source.addFlags(cmd)
	cmd.AddCommand(
		newDriversListCmd(),
		newDriversInstallCmd(),
		newDriversRemoveCmd(),
	)
	return cmd
}

var resolveDriversTargetFn = resolveTarget
var driverExtensionsForChannelFn = driverExtensionsForChannel

type driverManifestOptions struct {
	nightly bool
	pr      int
}

func (o *driverManifestOptions) addFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&o.nightly, "nightly", false, "Use drivers from the latest nightly build for this device's kernel")
	cmd.Flags().IntVar(&o.pr, "pr", 0, "Use drivers from a per-PR build manifest")
}

func (o driverManifestOptions) validate(cmd *cobra.Command) error {
	if cmd.Flags().Changed("pr") && o.pr <= 0 {
		return usageErrorf("--pr must be a positive PR number")
	}
	if o.nightly && o.pr > 0 {
		return usageErrorf("--nightly and --pr are mutually exclusive")
	}
	return nil
}

func (o driverManifestOptions) label(osVersion string) string {
	if o.nightly {
		return "latest nightly"
	}
	if o.pr > 0 {
		return fmt.Sprintf("PR %d", o.pr)
	}
	return "OS " + osVersion
}

// A third state: an add-on built for another kernel is not merely unloaded, it
// cannot load at all until it is reinstalled.
var driverIconStale = lipgloss.NewStyle().Foreground(tui.ColorNotice).Render("!")

// driverServiceErr rewrites the transport's Unimplemented into advice that fits.
// WendyDriverService is registered only on the mTLS server, so an unenrolled
// device answers Unimplemented however new its agent is, and the CLI's generic
// "update the agent" hint would send the operator down the wrong path.
func driverServiceErr(err error) error {
	if status.Code(err) != codes.Unimplemented {
		return err
	}
	return errors.New("this device does not serve driver add-ons. They are reachable only on an enrolled device (`wendy device enroll`); an agent predating driver support answers the same way")
}

// driverStale reports whether an installed add-on will not load as it stands:
// its image is unreadable, or it is pinned to a kernel other than the one
// running. An add-on that pins none (udev rules, firmware) is never stale.
func driverStale(d *agentpbv2.InstalledDriver, running string) bool {
	if d.GetUnreadable() {
		return true
	}
	k := d.GetKernelVersion()
	return k != "" && running != "" && k != running
}

// driverInstalledFor reports whether a published add-on is already on the device
// in a form that works here: a copy left by an OTA shares the name, not the kernel.
func driverInstalledFor(installed []*agentpbv2.InstalledDriver, e extensionEntry) bool {
	for _, d := range installed {
		if d.GetName() != e.Name || d.GetUnreadable() {
			continue
		}
		if d.GetKernelVersion() == "" || d.GetKernelVersion() == e.KernelVersion {
			return true
		}
	}
	return false
}

func newDriversListCmd() *cobra.Command {
	var available bool
	var source driverManifestOptions
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List installed driver add-ons (or --available: those published for this device)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := source.validate(cmd); err != nil {
				return err
			}
			if !available && (source.nightly || source.pr > 0) {
				return usageErrorf("--nightly and --pr require --available when listing drivers")
			}
			return runDriversList(cmd.Context(), available, source)
		},
	}
	cmd.Flags().BoolVar(&available, "available", false, "List drivers published in the registry for this device's kernel (from its OS version, or --nightly / --pr)")
	source.addFlags(cmd)
	return cmd
}

func runDriversList(ctx context.Context, available bool, source driverManifestOptions) error {
	target, err := resolveDriversTargetFn(ctx)
	if err != nil {
		return err
	}
	defer target.Close()
	if target.Agent == nil {
		return fmt.Errorf("selected device does not support driver add-ons")
	}
	if available {
		return renderAvailableDrivers(ctx, target, source)
	}
	resp, err := target.Agent.DriverService.ListDrivers(ctx, &agentpbv2.ListDriversRequest{})
	if err != nil {
		return fmt.Errorf("listing drivers: %w", driverServiceErr(err))
	}
	return renderDriversList(resp)
}

// renderAvailableDrivers lists the driver add-ons published for the device's OS
// version, filtered to its running kernel and marking those already installed.
func renderAvailableDrivers(ctx context.Context, target *SelectedDevice, source driverManifestOptions) error {
	deviceType, dl, err := deviceDriverCoords(ctx, target)
	if err != nil {
		return err
	}
	osVersion, kernel := dl.GetBaseVersion(), dl.GetKernelVersion()
	exts, err := driverExtensionsForChannelFn(ctx, deviceType, osVersion, source.nightly, source.pr)
	if err != nil {
		return err
	}

	// Only add-ons built for this device's running kernel are installable here.
	type avail struct {
		e     extensionEntry
		local bool
	}
	var rows []avail
	for _, e := range exts {
		if !kernelMatches(e, kernel) {
			continue
		}
		rows = append(rows, avail{e, driverInstalledFor(dl.GetInstalled(), e)})
	}

	if jsonOutput {
		type jr struct {
			Name          string   `json:"name"`
			Version       string   `json:"version,omitempty"`
			KernelVersion string   `json:"kernelVersion"`
			Modules       []string `json:"modulesLoad,omitempty"`
			Installed     bool     `json:"installed"`
		}
		out := struct {
			DeviceType string `json:"deviceType"`
			OSVersion  string `json:"osVersion"`
			Kernel     string `json:"kernel"`
			Source     string `json:"source"`
			Available  []jr   `json:"available"`
		}{DeviceType: deviceType, OSVersion: osVersion, Kernel: kernel, Source: source.label(osVersion)}
		out.Available = make([]jr, 0, len(rows))
		for _, r := range rows {
			out.Available = append(out.Available, jr{r.e.Name, r.e.Version, r.e.KernelVersion, r.e.ModulesLoad, r.local})
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	cliLogln("Available for %s · %s · kernel %s", deviceType, source.label(osVersion), kernel)
	if len(rows) == 0 {
		cliLogln("No drivers published in this catalog for the running kernel.")
		return nil
	}
	// A filled dot marks an add-on already installed on the device.
	headers := []string{"", "Name", "Version", "Modules"}
	var trows [][]string
	for _, r := range rows {
		icon := stateIconStopped
		if r.local {
			icon = stateIconRunning
		}
		trows = append(trows, []string{icon, r.e.Name, r.e.Version, strings.Join(r.e.ModulesLoad, ", ")})
	}
	fmt.Print(tui.RenderTable(headers, trows))
	return nil
}

func renderDriversList(resp *agentpbv2.ListDriversResponse) error {
	if jsonOutput {
		type jsonDriver struct {
			Name          string   `json:"name"`
			KernelVersion string   `json:"kernelVersion,omitempty"`
			Modules       []string `json:"modulesLoad,omitempty"`
			Loaded        bool     `json:"loaded"`
			Unreadable    bool     `json:"unreadable"`
			// Reported rather than derived, so a fleet script can alert on one field.
			Stale bool `json:"stale"`
		}
		out := struct {
			BaseVersion   string       `json:"baseVersion"`
			KernelVersion string       `json:"kernelVersion"`
			Installed     []jsonDriver `json:"installed"`
			LoadedModules []string     `json:"loadedModules,omitempty"`
		}{
			BaseVersion:   resp.GetBaseVersion(),
			KernelVersion: resp.GetKernelVersion(),
			LoadedModules: resp.GetLoadedModules(),
		}
		out.Installed = make([]jsonDriver, 0, len(resp.GetInstalled()))
		for _, d := range resp.GetInstalled() {
			out.Installed = append(out.Installed, jsonDriver{
				Name:          d.GetName(),
				KernelVersion: d.GetKernelVersion(),
				Modules:       d.GetModulesLoad(),
				Loaded:        d.GetLoaded(),
				Unreadable:    d.GetUnreadable(),
				Stale:         driverStale(d, resp.GetKernelVersion()),
			})
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	cliLogln("OS %s · kernel %s", resp.GetBaseVersion(), resp.GetKernelVersion())
	if len(resp.GetInstalled()) == 0 {
		cliLogln("No driver add-ons installed.")
		return nil
	}
	// A filled dot marks a driver whose modules are loaded, an empty dot one that
	// is merged but not loaded, and "!" one built for another kernel. No Version
	// column: the image carries no version field.
	headers := []string{"", "Name", "Kernel", "Modules"}
	var rows [][]string
	stale := 0
	for _, d := range resp.GetInstalled() {
		icon := stateIconStopped
		switch {
		case driverStale(d, resp.GetKernelVersion()):
			// Before Loaded: a stale add-on can still have modules resident from
			// before the kernel changed, which is the misleading case.
			icon = driverIconStale
			stale++
		case d.GetLoaded():
			icon = stateIconRunning
		}
		kernel := d.GetKernelVersion()
		if d.GetUnreadable() {
			kernel = "unreadable"
		}
		rows = append(rows, []string{icon, d.GetName(), kernel, strings.Join(d.GetModulesLoad(), ", ")})
	}
	fmt.Print(tui.RenderTable(headers, rows))
	if stale > 0 {
		subject := "1 add-on is"
		if stale > 1 {
			subject = fmt.Sprintf("%d add-ons are", stale)
		}
		cliLogln("")
		// The cause is per-row (a kernel that does not match, or "unreadable"), so
		// the summary states the consequence they share.
		cliLogln("%s", tui.WarningMessage(subject+" not usable on this kernel and will not load until reinstalled."))
		cliLogln("Reinstall with %s", tui.Command("wendy device drivers install <name>"))
	}
	return nil
}

func newDriversRemoveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a driver add-on from the device",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]

			if !force {
				confirmed, err := confirmDriverRemovalFn(fmt.Sprintf("Remove driver %s?", name))
				if err != nil {
					if errors.Is(err, tui.ErrCancelled) {
						return ErrUserCancelled
					}
					return err
				}
				if !confirmed {
					cliNotice("Cancelled.")
					return nil
				}
			}

			target, err := resolveDriversTargetFn(ctx)
			if err != nil {
				return err
			}
			defer target.Close()
			if target.Agent == nil {
				return fmt.Errorf("selected device does not support driver add-ons")
			}
			return removeDriver(ctx, target.Agent.DriverService, name)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Skip the confirmation prompt")
	return cmd
}

var confirmDriverRemovalFn = tui.Confirm

func removeDriver(ctx context.Context, client agentpbv2.WendyDriverServiceClient, name string) error {
	stream, err := client.RemoveDriver(ctx, &agentpbv2.RemoveDriverRequest{Name: name})
	if err != nil {
		return fmt.Errorf("removing driver: %w", driverServiceErr(err))
	}
	return consumeDriverApply(stream, name, "removed")
}

// driverApplyRecvStream is satisfied by both the InstallDriver (bidi) and
// RemoveDriver (server-streaming) client streams.
type driverApplyRecvStream interface {
	Recv() (*agentpbv2.DriverApplyResponse, error)
}

// consumeDriverApply prints streamed apply progress and returns the outcome.
// verb names what the stream is completing — the same apply RPC backs both
// install and remove, so the caller says which one the user asked for.
func consumeDriverApply(stream driverApplyRecvStream, name, verb string) error {
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("driver %s: stream ended without a result", name)
		}
		if err != nil {
			return driverServiceErr(err)
		}
		switch {
		case resp.GetProgress() != nil:
			p := resp.GetProgress()
			cliLogln("  %s… %d%%", p.GetPhase(), p.GetPercent())
		case resp.GetCompleted() != nil:
			c := resp.GetCompleted()
			cliSuccess("Driver %s %s.", c.GetName(), verb)
			if c.GetRebootRequired() {
				cliLogln("  A reboot is required to finish.")
			}
			return nil
		case resp.GetFailed() != nil:
			return fmt.Errorf("driver %s: %s", name, resp.GetFailed().GetErrorMessage())
		}
	}
}
