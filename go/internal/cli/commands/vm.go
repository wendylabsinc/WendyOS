package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func newVMCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vm",
		Short: "Run WendyOS in a local ARM64 virtual machine",
		// Hidden because the simulator is meant to be reached through
		// `wendy run`'s Simulator tab or --device sim, not administered by
		// hand. The subcommands stay for debugging a VM that misbehaves.
		Hidden: true,
		Long: "Run a WendyOS ARM64 virtual machine on this computer, so you can " +
			"develop and evaluate WendyOS without a Jetson or Raspberry Pi.\n\n" +
			"Requires QEMU (" + qemuInstallHint() + "); on macOS it is offered " +
			"automatically when missing.",
	}
	cmd.PersistentFlags().BoolVarP(&vmAssumeYes, "yes", "y", false,
		"Accept prompts, including installing QEMU when it is missing")
	cmd.AddCommand(newVMCreateCmd(), newVMStartCmd(), newVMStopCmd(),
		newVMLogsCmd(), newVMListCmd(), newVMRemoveCmd())
	return cmd
}

func newVMCreateCmd() *cobra.Command {
	var image, version string
	var diskGiB, prNumber int
	var nightly bool

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a VM, downloading the WendyOS image if needed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVMCreate(cmd, args[0], image, version, diskGiB, nightly, prNumber)
		},
	}
	cmd.Flags().StringVar(&image, "image", "", "Path to a local .wic disk image (default: download the published one)")
	cmd.Flags().StringVar(&version, "version", "", "WendyOS version to download (default: latest)")
	cmd.Flags().BoolVar(&nightly, "nightly", false, "Use nightly/prerelease builds")
	cmd.Flags().IntVar(&prNumber, "pr", 0, "Create from a pull request's build, so a change can be tried before it merges")
	cmd.Flags().IntVar(&diskGiB, "disk", 16, "Disk size in GiB (the image is grown to this size)")
	return cmd
}

// runVMCreate provisions a VM from a local image when one is named, and from the
// published image otherwise.
func runVMCreate(cmd *cobra.Command, name, image, version string, diskGiB int, nightly bool, pr int) error {
	return createVM(cmd.OutOrStdout(), name, image, version, diskGiB, nightly, pr)
}

// createVM provisions a VM, writing progress to out. Split from the cobra
// command so the simulator picker can provision one without a *cobra.Command.
func createVM(out io.Writer, name, image, version string, diskGiB int, nightly bool, pr int) error {
	if image != "" && (version != "" || nightly || pr > 0) {
		return fmt.Errorf("--image cannot be combined with --version, --nightly or --pr: a local image is already a specific build")
	}
	if pr > 0 && (version != "" || nightly) {
		return fmt.Errorf("--pr cannot be combined with --version or --nightly: a pull request publishes exactly one build")
	}
	store, err := vm.NewStore()
	if err != nil {
		return err
	}
	// Before the download: fetching gigabytes only to reject the name is a long
	// wait for an error knowable up front.
	if err := store.CheckCreatable(name); err != nil {
		return err
	}

	if pr > 0 {
		// Booting an image is running it. A PR build comes from a contributor
		// branch, so this is the one place the CLI runs code that has not been
		// through review -- say so rather than let --pr look like --nightly.
		cliLogln("Warning: PR %d's image is built from an unmerged branch. "+
			"Only boot it if you trust that branch's contents.", pr)
	}
	path, resolvedVersion := image, ""
	if path == "" {
		var cleanup func()
		if path, resolvedVersion, cleanup, err = fetchPublishedVMImage(version, nightly, pr); err != nil {
			return err
		}
		defer cleanup()
	}

	// Published images and cached copies are compressed; sniff rather than trust
	// the extension, exactly as os install does for the same artifacts.
	stream, err := openLocalImageStream(path)
	if err != nil {
		return err
	}
	defer stream.Close()

	meta := vm.Meta{ImageVersion: resolvedVersion, ImageSource: vmImageSource(image, nightly, pr)}
	if err := store.CreateFrom(name, stream, stream.uncompressedSize, int64(diskGiB)<<30, meta); err != nil {
		return err
	}
	fmt.Fprintf(out, "Created VM %q. Start it with 'wendy vm start %s'.\n", name, name)
	return nil
}

func newVMStartCmd() *cobra.Command {
	var o vmStartOptions

	cmd := &cobra.Command{
		Use:   "start [name]",
		Short: "Start a VM and attach to its console",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveVMName(args, "start")
			if err != nil {
				return err
			}
			return runVMStart(cmd, name, o)
		},
	}
	cmd.Flags().StringVar(&o.netMode, "net", string(vm.NetUser),
		"Networking: 'user' forwards the agent port and needs no setup, but "+
			"'wendy discover' cannot see the VM; 'shared' puts it on a host-visible "+
			"network so discover works (needs socket_vmnet)")
	cmd.Flags().IntVar(&o.hostPort, "port", vm.DefaultAgentPort,
		"Host port to forward to the guest agent's 50051. Only needed when "+
			"something else already holds the default")
	cmd.Flags().IntVar(&o.memoryMiB, "memory", vm.DefaultMemoryMiB, "Guest memory in MiB")
	cmd.Flags().IntVar(&o.cpus, "cpus", vm.DefaultCPUs, "Guest CPU count")
	cmd.Flags().BoolVarP(&o.detach, "detach", "d", false,
		"Run the VM in the background instead of attaching to its console")
	return cmd
}

// vmStartOptions are the knobs both start paths share.
type vmStartOptions struct {
	netMode   string
	hostPort  int
	memoryMiB int
	cpus      int
	detach    bool
}

// resolveVMSpec turns the host's capabilities and the caller's flags into a
// launchable Spec. Shared by the foreground and detached paths so the two can
// never diverge on acceleration, firmware or networking.
func resolveVMSpec(name string, o vmStartOptions) (vm.Spec, *vm.Store, error) {
	if err := vm.ValidName(name); err != nil {
		return vm.Spec{}, nil, err
	}
	netMode, err := vm.ParseNetMode(o.netMode)
	if err != nil {
		return vm.Spec{}, nil, err
	}
	store, err := vm.NewStore()
	if err != nil {
		return vm.Spec{}, nil, err
	}
	if _, err := os.Stat(store.DiskPath(name)); err != nil {
		return vm.Spec{}, nil, fmt.Errorf("no VM named %q; create one with 'wendy vm create %s'", name, name)
	}
	if err := ensureQEMUFn(context.Background()); err != nil {
		return vm.Spec{}, nil, err
	}

	brew := vm.BrewPrefix()
	firmware, err := vm.FindFirmware(runtime.GOOS, brew)
	if err != nil {
		return vm.Spec{}, nil, err
	}

	mac, err := store.MACAddress(name)
	if err != nil {
		return vm.Spec{}, nil, err
	}
	net := vm.NetConfig{Mode: netMode, MAC: mac}
	if net.Mode == vm.NetShared {
		socket, err := ensureSocketVMNet(context.Background(), runtime.GOOS, brew)
		if err != nil {
			return vm.Spec{}, nil, err
		}
		net.SocketPath = socket
	} else {
		// Only user mode forwards a port. Leaving AgentPort zero in shared mode
		// keeps a loopback address that nothing listens on out of the run
		// record, and so out of `vm list`, discovery and the simulator picker.
		if err := vm.CheckHostPort(o.hostPort); err != nil {
			return vm.Spec{}, nil, err
		}
		net.AgentPort = o.hostPort
		// Keep the conventional port when available. Deployment resolves the
		// VM's actual registry forward through QMP and allocates another port
		// when 5000 belongs to another VM or host process.
		if err := vm.CheckHostPort(vm.DeviceRegistryPort); err == nil {
			net.RegistryPort = vm.DeviceRegistryPort
		}
	}

	return vm.Spec{
		Name:         name,
		DiskPath:     store.DiskPath(name),
		FirmwareCode: firmware,
		FirmwareVars: store.VarsPath(name),
		MemoryMiB:    o.memoryMiB,
		CPUs:         o.cpus,
		Accel:        vm.AccelFor(runtime.GOOS, runtime.GOARCH),
		Net:          net,
	}, store, nil
}

// runVMStart boots a VM, either attached to this terminal or in the background.
func runVMStart(cmd *cobra.Command, name string, o vmStartOptions) error {
	spec, store, err := resolveVMSpec(name, o)
	if err != nil {
		// A running VM still holds its forwarded port, so the port check fires
		// first and blames the port for what is really "already running". Asked
		// only once the start has failed anyway: a liveness probe on the happy
		// path would turn a transient reading into a start that silently does
		// nothing.
		if s, sErr := vm.NewStore(); sErr == nil {
			if st, sErr := s.Status(name); sErr == nil && st.Running {
				return fmt.Errorf("VM %q is already running; reach it with 'wendy vm logs %s' or stop it first", name, name)
			}
		}
		return err
	}
	out := cmd.OutOrStdout()

	if o.detach {
		st, err := store.StartDetached(spec)
		if err != nil {
			if errors.Is(err, vm.ErrAlreadyRunning) {
				fmt.Fprintf(out, "VM %q is already running.\n", name)
				return nil
			}
			return err
		}
		// StartDetached returns as soon as QEMU is spawned. One that rejects its
		// arguments or firmware is gone milliseconds later, and reporting
		// success then sends the user off to debug a VM that never existed.
		if err := confirmVMSurvivedStartup(store, name); err != nil {
			return err
		}
		fmt.Fprintf(out, "Started %s in the background (pid %d).\n", name, st.PID)
		vmPrintReachability(out, spec.Net, o.hostPort)
		fmt.Fprintf(out, "Console: 'wendy vm logs %s'. Stop it with 'wendy vm stop %s'.\n", name, name)
		return nil
	}

	fmt.Fprintf(out, "Starting %s (%s acceleration, %s networking).\n", name, spec.Accel, spec.Net.Mode)
	vmPrintReachability(out, spec.Net, o.hostPort)
	fmt.Fprintln(out, "Press Ctrl-A then X to power it off.")

	// Hand the terminal to QEMU: the guest console is the point of `vm start`.
	err = store.RunForeground(cmd.Context(), spec, os.Stdin, out, cmd.ErrOrStderr())
	if errors.Is(err, vm.ErrAlreadyRunning) {
		return fmt.Errorf("VM %q is already running; reach it with 'wendy vm logs %s' or stop it first", name, name)
	}
	return err
}

// vmStartupSettleTime is how long a detached start waits before believing the
// emulator is up. QEMU rejects a bad spec and exits well inside this.
const vmStartupSettleTime = 400 * time.Millisecond

// confirmVMSurvivedStartup reports an emulator that exited immediately after
// launch, quoting the console so the reason is in the error rather than in a
// log the user has not thought to read yet.
func confirmVMSurvivedStartup(store *vm.Store, name string) error {
	time.Sleep(vmStartupSettleTime)
	st, err := store.Status(name)
	if err != nil || st.Running {
		return nil
	}
	return fmt.Errorf("VM %q exited immediately after starting%s", name, vmConsoleTail(name))
}

// vmConsoleTail returns the last few console lines, to append to a boot
// failure. Best-effort: a missing log just means there is nothing to add.
func vmConsoleTail(name string) string {
	store, err := vm.NewStore()
	if err != nil {
		return ""
	}
	data, err := readFileTail(store.LogPath(name), consoleTailBytes)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > vmConsoleTailLines {
		lines = lines[len(lines)-vmConsoleTailLines:]
	}
	for i, l := range lines {
		lines[i] = sanitizeConsoleLine(l)
	}
	return fmt.Sprintf("\nlast console output (%s):\n  %s",
		store.LogPath(name), strings.Join(lines, "\n  "))
}

// readFileTail returns at most n bytes from the end of path.
func readFileTail(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > n {
		if _, err := f.Seek(-n, io.SeekEnd); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(io.LimitReader(f, n))
}

// sanitizeConsoleLine strips control characters before guest output reaches the
// terminal. The console is whatever the guest chose to print, and an escape
// sequence in it would otherwise be executed by the user's terminal rather than
// shown -- repositioning the cursor or rewriting what is already on screen.
func sanitizeConsoleLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		// C0 and C1 both: 0x9b and 0x9d are the 8-bit CSI and OSC introducers,
		// so stripping only the 7-bit ESC form leaves the same door open.
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, s)
}

// consoleTailBytes bounds the read. A console log grows for the life of the VM,
// and only the last few lines are ever shown.
const consoleTailBytes = 8 << 10

const vmConsoleTailLines = 15

// vmPrintReachability says how to reach the guest, which differs by net mode.
func vmPrintReachability(out io.Writer, net vm.NetConfig, hostPort int) {
	if net.SupportsDiscovery() {
		fmt.Fprintln(out, "Once it boots, find it with 'wendy discover'.")
		return
	}
	fmt.Fprintf(out, "Once it boots, reach it with 'wendy --device 127.0.0.1:%d device info', "+
		"or pick it from the Simulator tab of 'wendy discover'.\n", hostPort)
	fmt.Fprintln(out, "It is not on your network, though: nothing else can reach a user-mode VM. "+
		"Use --net shared for that.")
}

// ensureQEMUFn resolves the emulator, prompting to install it where that is
// possible. A package var so both start paths share one message and tests can
// describe a host without QEMU.
var ensureQEMUFn = func(ctx context.Context) error {
	return ensureQEMUForHostOS(ctx, runtime.GOOS)
}

// qemuLookPathFn resolves the emulator binary; indirected so tests can describe
// a host that does not have it.
var qemuLookPathFn = exec.LookPath

func newVMListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List local VMs",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := vm.NewStore()
			if err != nil {
				return err
			}
			statuses, err := store.Statuses()
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(vmListJSON(statuses))
			}
			if len(statuses) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No VMs. Create one with 'wendy vm create <name>'.")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSTATE\tADDRESS\tVERSION")
			for _, st := range statuses {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
					st.Name, vmStateLabel(st), firstNonEmpty(vmAddress(st), "-"),
					firstNonEmpty(st.Meta.ImageVersion, "-"))
			}
			return w.Flush()
		},
	}
}

// vmStateLabel distinguishes a VM that is booting from one that is up: the run
// lock is taken before the emulator has a pid, and a reader must not mistake
// that window for a stale record.
func vmStateLabel(st vm.Status) string {
	switch {
	case !st.Running:
		return "stopped"
	case st.State.PID == 0:
		return "starting"
	default:
		return "running"
	}
}

// vmAddress is where the guest agent answers, which only user-mode VMs forward.
// vmAddress returns the forwarded agent address, or empty when the VM has
// none. Empty rather than a display dash: both callers that treat it as data
// had to recognise and undo the dash first.
func vmAddress(st vm.Status) string {
	if !st.Running || st.State.AgentPort == 0 {
		return ""
	}
	return fmt.Sprintf("127.0.0.1:%d", st.State.AgentPort)
}

type vmListEntry struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Address string `json:"address,omitempty"`
	Version string `json:"version,omitempty"`
	Source  string `json:"source,omitempty"`
	PID     int    `json:"pid,omitempty"`
}

func vmListJSON(statuses []vm.Status) []vmListEntry {
	out := make([]vmListEntry, 0, len(statuses))
	for _, st := range statuses {
		e := vmListEntry{
			Name:    st.Name,
			State:   vmStateLabel(st),
			Version: st.Meta.ImageVersion,
			Source:  st.Meta.ImageSource,
			PID:     st.State.PID,
		}
		if addr := vmAddress(st); addr != "" {
			e.Address = addr
		}
		out = append(out, e)
	}
	return out
}

func newVMStopCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "stop [name]",
		Short: "Stop a VM running in the background",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveVMName(args, "stop")
			if err != nil {
				return err
			}
			return runVMStop(cmd, name, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Kill the VM instead of asking it to power off")
	return cmd
}

// Allow systemd's normal service-stop timeout and filesystem flushing. Timing
// out leaves the guest running; power cuts require an explicit --force.
const vmStopGrace = 120 * time.Second

func runVMStop(cmd *cobra.Command, name string, force bool) error {
	store, err := vm.NewStore()
	if err != nil {
		return err
	}
	st, err := store.Status(name)
	if err != nil {
		return err
	}
	if !st.Exists {
		return fmt.Errorf("no VM named %q", name)
	}
	if !st.Running {
		fmt.Fprintf(cmd.OutOrStdout(), "VM %q is not running.\n", name)
		return nil
	}
	// Store.Stop makes the "still starting" refusal itself, so that `vm rm
	// --force` gets it too rather than only this command.
	if err := store.StopContext(cmd.Context(), name, force, vmStopGrace); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Stopped VM %q.\n", name)
	return nil
}

func newVMLogsCmd() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs [name]",
		Short: "Show a backgrounded VM's console output",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveVMName(args, "show logs for")
			if err != nil {
				return err
			}
			store, err := vm.NewStore()
			if err != nil {
				return err
			}
			if err := vm.ValidName(name); err != nil {
				return err
			}
			return streamFile(cmd.Context(), cmd.OutOrStdout(), store.LogPath(name), follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Keep printing as the guest writes")
	return cmd
}

// streamFile copies path to out, optionally waiting for more. Written against
// the file rather than tailing a pipe because a detached VM's console is a
// plain file that outlives any particular CLI invocation.
func streamFile(ctx context.Context, out io.Writer, path string, follow bool) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no console log yet; start the VM with 'wendy vm start -d'")
		}
		return err
	}
	// Closes whichever handle is current: a rotation below replaces f, and a
	// plain `defer f.Close()` would bind the original -- double-closing it and
	// leaking the replacement.
	defer func() { f.Close() }()

	for {
		if _, err := io.Copy(out, f); err != nil {
			return err
		}
		if !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(300 * time.Millisecond):
		}
		// A restart renames console.log to console.log.prev and starts a new
		// one. Following the inode we opened would tail the file nobody writes
		// to any more, which looks exactly like a VM that stopped logging.
		if next, err := reopenIfReplaced(f, path); err == nil && next != nil {
			f.Close()
			f = next
		}
	}
}

// reopenIfReplaced returns a handle on path when it is no longer the file f
// refers to, or nil when it is unchanged or momentarily absent.
func reopenIfReplaced(f *os.File, path string) (*os.File, error) {
	open, err := f.Stat()
	if err != nil {
		return nil, err
	}
	onDisk, err := os.Stat(path)
	if err != nil || os.SameFile(open, onDisk) {
		return nil, err
	}
	return os.Open(path)
}

func newVMRemoveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "rm [name]",
		Short:   "Delete a VM and its disk",
		Args:    cobra.MaximumNArgs(1),
		Aliases: []string{"remove"},
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveVMName(args, "remove")
			if err != nil {
				return err
			}
			store, err := vm.NewStore()
			if err != nil {
				return err
			}
			st, err := store.Status(name)
			if err != nil {
				return err
			}
			// Store.Remove refuses a running VM on its own; stopping first is
			// what --force means, and turns its lock error into a clear one.
			if st.Running {
				if !force {
					return fmt.Errorf("VM %q is running; stop it first or pass --force", name)
				}
				if err := store.Stop(name, true, vmStopGrace); err != nil {
					return err
				}
			}
			if err := store.Remove(name); err != nil {
				if errors.Is(err, vm.ErrAlreadyRunning) {
					return fmt.Errorf("VM %q started while it was being removed; stop it and retry", name)
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted VM %q.\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Stop the VM first if it is running")
	return cmd
}

// resolveVMName keeps explicitly named invocations scriptable and opens the
// picker only when a person omitted the name in an interactive terminal.
func resolveVMName(args []string, action string) (string, error) {
	if len(args) == 1 {
		return args[0], nil
	}
	if jsonOutput || !isInteractiveTerminalFn() {
		return "", fmt.Errorf("no VM name specified; run in an interactive terminal or pass the VM name as an argument")
	}
	return pickVMNameFn(action)
}

// pickVMNameFn is a seam for command tests: a real picker owns the process TTY
// and cannot be driven through cobra's in-memory input and output buffers.
var pickVMNameFn = pickVMName

func pickVMName(action string) (string, error) {
	statuses, err := vmStatusesFn()
	if err != nil {
		return "", err
	}
	if len(statuses) == 0 {
		return "", fmt.Errorf("no VMs found; create one with 'wendy vm create <name>'")
	}

	items := make([]tui.PickerItem, 0, len(statuses))
	for _, st := range statuses {
		items = append(items, tui.PickerItem{
			Name:      st.Name,
			Type:      vmStateLabel(st),
			Address:   vmAddress(st),
			OSVersion: st.Meta.ImageVersion,
			Value:     st.Name,
		})
	}

	return pickFromItemsWithColumns("Select a VM to "+action, items, simulatorPickerColumns())
}

// The VM's manifest key is its WENDYOS_BOARD_ID, the same string the image
// bakes into /etc/wendyos/device-type. Every board is keyed that way, so this
// is not a name to choose freely.
const (
	vmDeviceKey  = "vm-arm64"
	vmStorageKey = "disk"
)

// fetchPublishedVMImage resolves the published VM image, downloading it if the
// cache does not already hold it. It returns the local path and the version tag
// it resolved to, which the caller records so a stopped VM still knows what it
// is running.
func fetchPublishedVMImage(version string, nightly bool, pr int) (string, string, func(), error) {
	fetchMain := fetchMainManifest
	if pr > 0 {
		fetchMain = func() (*mainManifest, error) { return fetchPRMainManifest(pr) }
	}
	mm, err := fetchMain()
	if err != nil {
		return "", "", nil, err
	}
	dev, ok := mm.Devices[vmDeviceKey]
	if !ok {
		if pr > 0 {
			return "", "", nil, fmt.Errorf("PR %d published no %s image; check the build finished for that device", pr, vmDeviceKey)
		}
		return "", "", nil, fmt.Errorf("no published %s image yet; build one and pass --image", vmDeviceKey)
	}

	// PR entries are always published as nightlies, so their tag lives in
	// LatestNightly.
	ver := version
	if pr > 0 {
		ver = prDeviceVersion(dev)
	} else if ver == "" {
		ver = dev.Latest
		if nightly && dev.LatestNightly != "" {
			ver = dev.LatestNightly
		}
	}
	if ver == "" {
		return "", "", nil, fmt.Errorf("no %s release published yet; retry with --nightly, or build one and pass --image", vmDeviceKey)
	}

	dm, err := fetchDeviceManifest(dev.ManifestPath)
	if err != nil {
		return "", "", nil, err
	}
	info, err := getImageInfo(dm, ver, vmStorageKey)
	if err != nil {
		return "", "", nil, err
	}

	path, cleanup, err := resolveVMImage(info)
	return path, ver, cleanup, err
}

// vmImageSource names where a VM's image came from, for the record kept beside
// the disk. Only the channel, not the URL: a PR or nightly build is replaced in
// place, so a stored URL would outlive the bytes it named.
func vmImageSource(image string, nightly bool, pr int) string {
	switch {
	case image != "":
		return "local"
	case pr > 0:
		return fmt.Sprintf("pr/%d", pr)
	case nightly:
		return "nightly"
	default:
		return "release"
	}
}
