package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	g1bundle "github.com/wendylabsinc/wendy/go/simulator/g1"
	go2bundle "github.com/wendylabsinc/wendy/go/simulator/go2"
)

const (
	go2RuntimeAppID = "sh.wendy.simulator.go2"
	g1RuntimeAppID  = "sh.wendy.simulator.g1"
	go2PolicyBundle = "go2-moe-cts-164k-0.6715-v1"
	g1PolicyBundle  = "g1-29dof-velocity-v0-4960b847-v1"
)

type robotRuntime struct {
	name, appID, agentFeature, policyBundle, envPrefix string
	sourceDigest                                       func() string
	materialize                                        func(string) (string, error)
}

func robotRuntimeForKind(kind string) (robotRuntime, error) {
	switch kind {
	case vm.RobotKindGo2:
		return robotRuntime{
			name: "Unitree Go2", appID: go2RuntimeAppID, agentFeature: "go2-virtual-robot",
			policyBundle: go2PolicyBundle, envPrefix: "GO2_",
			sourceDigest: go2bundle.SourceDigest, materialize: go2bundle.Materialize,
		}, nil
	case vm.RobotKindG1:
		return robotRuntime{
			name: "Unitree G1", appID: g1RuntimeAppID, agentFeature: "g1-virtual-robot",
			policyBundle: g1PolicyBundle, envPrefix: "G1_",
			sourceDigest: g1bundle.SourceDigest, materialize: g1bundle.Materialize,
		}, nil
	default:
		return robotRuntime{}, fmt.Errorf("unsupported robot kind %q", kind)
	}
}

func (r robotRuntime) environment(name string, profile vm.RobotProfile) []string {
	env := []string{r.envPrefix + "VM_NAME=" + name, r.envPrefix + "SOURCE_DIGEST=" + profile.SourceDigest,
		r.envPrefix + "WORLD=" + profile.World, r.envPrefix + "SEED=" + strconv.FormatUint(uint64(profile.Seed), 10),
		r.envPrefix + "VISUAL_DETAIL=" + profile.VisualDetail}
	if profile.Kind == vm.RobotKindGo2 {
		env = append(env, "GO2_AUTO_APP_CONTROL=1")
	}
	return env
}

func (r robotRuntime) validateSource(name string, profile vm.RobotProfile) error {
	if profile.SourceDigest != r.sourceDigest() {
		return &robotSourceMismatchError{name: name, runtimeName: r.name}
	}
	return nil
}

type robotSourceMismatchError struct {
	name, runtimeName string
}

func (e *robotSourceMismatchError) Error() string {
	return fmt.Sprintf("VM %q pins a different %s runtime source; use 'wendy vm robot update %s' to apply this CLI's runtime", e.name, e.runtimeName, e.name)
}

// Tests provide a temporary store without redirecting the user's home or
// inspecting any real VM. Every production call uses the standard VM store.
var robotVMStore = vm.NewStore
var robotSandboxPort = (*vm.Store).EnsureRobotSandboxPort

var pickSimulatorProfileFn = func() (string, error) {
	return pickFromItems("Choose a simulator", []tui.PickerItem{
		{Name: "Generic WendyOS", Description: "An ordinary VM for application development", Value: "generic"},
		{Name: "Unitree Go2", Description: "A walking virtual robot with ROS 2 and a MuJoCo sandbox", Value: "go2"},
		{Name: "Unitree G1", Description: "A humanoid virtual robot with ROS 2 and a MuJoCo sandbox", Value: "g1"},
	})
}

func validateSimulatorProfile(kind string) error {
	if kind != "generic" && kind != vm.RobotKindGo2 && kind != vm.RobotKindG1 {
		return fmt.Errorf("unsupported simulator profile %q; choose generic, go2 or g1", kind)
	}
	return nil
}

func attachSimulatorProfile(name, kind string) error {
	if err := validateSimulatorProfile(kind); err != nil {
		return err
	}
	if kind == "generic" {
		return nil
	}
	if err := vm.ValidName(name); err != nil {
		return err
	}
	runtime, err := robotRuntimeForKind(kind)
	if err != nil {
		return err
	}
	profile, err := vm.NewRobotProfile(kind, runtime.sourceDigest(), runtime.policyBundle)
	if err != nil {
		return err
	}
	store, err := robotVMStore()
	if err != nil {
		return err
	}
	// Configuration requires an existing VM, and must not create lifecycle
	// files for a misspelled name. CreateRobotProfile repeats this check under
	// the lifecycle lock so removal cannot race the profile write.
	if _, exists := store.ReadMeta(name); !exists {
		return fmt.Errorf("VM %q has no readable metadata", name)
	}
	if err := store.CreateRobotProfile(name, profile); err != nil {
		return err
	}
	cliLogln("%s profile created. Run 'wendy vm robot start %s' or connect to vm:%s to build and start its robot runtime.", runtime.name, name, name)
	return nil
}

func simulatorResources(store *vm.Store, name string) (vmStartOptions, error) {
	o := vmStartOptions{cpus: vm.DefaultCPUs, memoryMiB: vm.DefaultMemoryMiB}
	profile, exists, err := store.ReadRobotProfile(name)
	if err != nil {
		return o, fmt.Errorf("reading robot profile for %s: %w", name, err)
	}
	if exists {
		o.cpus, o.memoryMiB = profile.CPUs, profile.MemoryMiB
	}
	return o, nil
}

func validateRobotVMResources(conn *grpcclient.AgentConnection, profile vm.RobotProfile) error {
	name, err := userVMForConnection(conn)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("managed robot requires a verified running VM")
	}
	statuses, err := vmStatusesFn()
	if err != nil {
		return err
	}
	for _, status := range statuses {
		if status.Name != name || !status.Running {
			continue
		}
		if status.State.CPUs >= profile.CPUs && status.State.MemoryMiB >= profile.MemoryMiB {
			return nil
		}
		return fmt.Errorf("VM %q is running with %d CPUs and %d MiB; its %s profile requires at least %d CPUs and %d MiB. "+
			"Run 'wendy vm stop %s', then 'wendy vm robot start %s' to apply the profile's resources",
			name, status.State.CPUs, status.State.MemoryMiB, profile.Kind, profile.CPUs, profile.MemoryMiB, name, name)
	}
	return fmt.Errorf("VM %q stopped while checking robot resources; reconnect to vm:%s", name, name)
}

// Fields are matched against the desired profile before a response is trusted.
// Readiness is live and is never persisted in robot.json.
type robotRuntimeStatus struct {
	VMName         string `json:"vm_name"`
	Simulation     bool   `json:"simulation"`
	ProfileVersion int    `json:"profile_version"`
	RobotKind      string `json:"robot_kind"`
	SourceDigest   string `json:"source_digest"`
	PolicyBundle   string `json:"policy_bundle"`
	World          string `json:"world"`
	ClockMode      string `json:"clock_mode"`
	Seed           uint32 `json:"seed"`
	VisualDetail   string `json:"visual_detail"`
	DDSIsolation   string `json:"dds_isolation"`
	Healthy        bool   `json:"healthy"`
	Ready          bool   `json:"ready"`
	Mode           string `json:"mode"`
	Error          string `json:"error"`
	Epoch          uint64 `json:"epoch"`
}

func (s robotRuntimeStatus) matches(name string, p vm.RobotProfile) error {
	if s.VMName != name || !s.Simulation || s.ProfileVersion != p.Version || s.RobotKind != p.Kind ||
		s.SourceDigest != p.SourceDigest || s.PolicyBundle != p.PolicyBundle || s.World != p.World ||
		s.ClockMode != p.ClockMode || s.Seed != p.Seed || s.VisualDetail != p.VisualDetail ||
		s.DDSIsolation != "udp-rtps-loopback" {
		return fmt.Errorf("sandbox identity does not match this VM's robot profile")
	}
	return nil
}

var robotHTTPClient = &http.Client{
	Timeout: 3 * time.Second,
	// A local sandbox must never redirect a status probe or reset to a different host.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport:     &http.Transport{Proxy: nil},
}

func robotURL(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) }

func readRobotStatus(ctx context.Context, port int) (robotRuntimeStatus, error) {
	var result robotRuntimeStatus
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotURL(port)+"/api/status", nil)
	if err != nil {
		return result, err
	}
	resp, err := robotHTTPClient.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("sandbox status returned HTTP %d", resp.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("decoding sandbox status: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return result, fmt.Errorf("sandbox status must be exactly one JSON object")
	}
	return result, nil
}

func waitForRobot(ctx context.Context, name string, port int, profile vm.RobotProfile) error {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	var last error
	for {
		state, err := readRobotStatus(ctx, port)
		if err == nil {
			if err = state.matches(name, profile); err != nil {
				return err
			}
			if state.Healthy && state.Ready {
				return nil
			}
			if state.Error != "" {
				return fmt.Errorf("robot runtime failed: %s", state.Error)
			}
			last = fmt.Errorf("robot is %s (ready=%t)", state.Mode, state.Ready)
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for robot readiness: %w (%v)", ctx.Err(), last)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// The provision lock is separate from the VM lifecycle lock: long container
// builds must not stop a user from shutting down QEMU. Every endpoint is
// revalidated against the named VM immediately before deployment.
func lockRobotProvision(ctx context.Context, store *vm.Store, name string) (func(), error) {
	if err := vm.ValidName(name); err != nil {
		return nil, err
	}
	path := filepath.Join(store.Dir(name), "robot-provision.lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("robot provision lock is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, f.Close())
		}
		ok, err := flock.TryLock(f)
		if err != nil {
			return nil, errors.Join(err, f.Close())
		}
		if ok {
			return func() { _ = flock.Unlock(f); _ = f.Close() }, nil
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), f.Close())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

var reconcileSimulatorRobotFn func(context.Context, *grpcclient.AgentConnection) error

func init() { reconcileSimulatorRobotFn = reconcileSimulatorRobot }

type robotAgentMaintenanceKey struct{}
type robotRuntimeNonInteractiveKey struct{}

// Carry the resolver's prompt policy through VM selection and connection.
// NonInteractive suppresses prompts; it does not authorize runtime updates.
func robotRuntimePromptContext(ctx context.Context, nonInteractive bool) context.Context {
	if nonInteractive {
		return context.WithValue(ctx, robotRuntimeNonInteractiveKey{}, true)
	}
	return ctx
}

// Agent repair must remain reachable when a VM's robot cannot start. The two
// agent-update commands use this private context; VM discovery, transport and
// pin verification still happen normally before reconciliation is reached.
func robotAgentMaintenanceContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, robotAgentMaintenanceKey{}, true)
}

func reconcileSimulatorRobot(ctx context.Context, conn *grpcclient.AgentConnection) error {
	if maintenance, _ := ctx.Value(robotAgentMaintenanceKey{}).(bool); maintenance {
		return nil
	}
	err := reconcileRobot(ctx, conn, false)
	var mismatch *robotSourceMismatchError
	nonInteractive, _ := ctx.Value(robotRuntimeNonInteractiveKey{}).(bool)
	if !errors.As(err, &mismatch) || nonInteractive || jsonOutput || !isInteractiveTerminalFn() {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// The failed reconciliation released its provisioning lock before asking.
	// Reuse the explicit update path after approval, including readiness checks.
	question := fmt.Sprintf("%s in simulator %q needs a runtime update. Rebuild it and reset its robot world now?", mismatch.runtimeName, mismatch.name)
	if !confirmFn(question) {
		return ErrUserCancelled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return reconcileRobot(ctx, conn, true)
}

func requireRobotAgentCapability(ctx context.Context, conn *grpcclient.AgentConnection, kind string) error {
	runtime, err := robotRuntimeForKind(kind)
	if err != nil {
		return err
	}
	if conn == nil || conn.AgentService == nil {
		return fmt.Errorf("managed %s runtime requires a verified agent connection", runtime.name)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	info, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return fmt.Errorf("checking virtual robot agent support: %w", err)
	}
	if (info.GetOs() == "linux" || info.GetOs() == "wendyos") && info.GetDeviceType() == "vm-arm64" && slices.Contains(info.GetFeatureset(), runtime.agentFeature) {
		return nil
	}
	return fmt.Errorf("VM %q agent %q lacks %s support. Update it with 'wendy --device vm:%s device update', "+
		"or use 'wendy --device vm:%s device update --binary <path-to-agent-binary>' for a development build. "+
		"The selected release or binary must include virtual robot support", conn.SimulatorName, info.GetVersion(), runtime.agentFeature, conn.SimulatorName, conn.SimulatorName)
}

func reconcileRobot(ctx context.Context, conn *grpcclient.AgentConnection, update bool) error {
	if conn == nil || conn.SimulatorName == "" {
		return nil
	}
	name := conn.SimulatorName
	store, err := robotVMStore()
	if err != nil {
		return err
	}
	if _, exists, err := store.ReadRobotProfile(name); err != nil {
		return err
	} else if !exists {
		return nil
	}
	unlock, err := lockRobotProvision(ctx, store, name)
	if err != nil {
		return err
	}
	defer unlock()
	return reconcileRobotLocked(ctx, conn, store, update)
}

// Caller holds the provisioning lock. Explicit restart can use this when the
// runtime is absent without recursively taking the same lock.
func reconcileRobotLocked(ctx context.Context, conn *grpcclient.AgentConnection, store *vm.Store, update bool) error {
	name := conn.SimulatorName
	if update {
		if err := store.UpdateRobotProfile(name, func(p *vm.RobotProfile) error {
			runtime, err := robotRuntimeForKind(p.Kind)
			if err != nil {
				return err
			}
			p.SourceDigest, p.PolicyBundle, p.RuntimeDigest = runtime.sourceDigest(), runtime.policyBundle, ""
			return nil
		}); err != nil {
			return err
		}
	}
	profile, exists, err := store.ReadRobotProfile(name)
	if err != nil {
		return err
	}
	if !exists {
		return vm.ErrRobotProfileMissing
	}
	runtime, err := robotRuntimeForKind(profile.Kind)
	if err != nil {
		return err
	}
	if err := runtime.validateSource(name, profile); err != nil {
		return err
	}
	if err := validateRobotVMResources(conn, profile); err != nil {
		return err
	}
	if err := requireRobotAgentCapability(ctx, conn, profile.Kind); err != nil {
		return err
	}
	port, err := robotSandboxPort(store, ctx, name)
	if err != nil {
		return err
	}
	current, healthErr := readRobotStatus(ctx, port)
	if healthErr == nil && !update {
		if err := current.matches(name, profile); err != nil {
			return err
		}
		if current.Healthy {
			// Pausing or lying down is user state, not a reason to rebuild or
			// reset the world when a second CLI command connects.
			return nil
		}
		return fmt.Errorf("robot runtime is unhealthy: %s; inspect 'wendy vm robot status %s' or recover with 'wendy vm robot restart %s'", current.Error, name, name)
	}
	app, err := findRobotContainer(ctx, conn, runtime.appID)
	if err != nil {
		return err
	}
	if !update && app != nil && app.GetAppVersion() == robotAppVersion(profile) {
		if app.GetRunningState() != agentpb.AppRunningState_RUNNING {
			stream, err := conn.ContainerService.StartContainer(ctx, &agentpb.StartContainerRequest{AppName: runtime.appID})
			if err != nil {
				return fmt.Errorf("starting managed %s runtime: %w", runtime.name, err)
			}
			if err := awaitStarted(stream); err != nil {
				return err
			}
		}
		return waitForRobot(ctx, name, port, profile)
	}
	cache, err := config.CacheDir()
	if err != nil {
		return err
	}
	source, err := runtime.materialize(filepath.Join(cache, "simulator"))
	if err != nil {
		return err
	}
	cfg, err := appconfig.LoadFromFile(filepath.Join(source, "wendy.json"))
	if err != nil {
		return err
	}
	if cfg.AppID != runtime.appID {
		return fmt.Errorf("embedded %s manifest has an unexpected app ID", runtime.name)
	}
	cfg.Version = robotAppVersion(profile)
	cfg.Readiness, cfg.Hooks = nil, nil // This runtime uses its own mapped HTTP identity/readiness check.
	if _, err := userVMForConnection(conn); err != nil {
		return err
	}
	cliLogln("Building and provisioning %s in %s (the first build downloads pinned assets and ROS dependencies)...", runtime.name, name)
	err = runWithAgent(ctx, conn, source, cfg, runOptions{
		managedRobot: true, buildType: "docker", builder: "docker", dockerfile: "Dockerfile",
		detach: true, yes: true, restartUnlessStopped: true,
		env: runtime.environment(name, profile),
	})
	if err != nil {
		return fmt.Errorf("provisioning %s runtime: %w", runtime.name, err)
	}
	if err := waitForRobot(ctx, name, port, profile); err != nil {
		return err
	}
	cliLogln("%s ready. Sandbox: %s", runtime.name, robotURL(port))
	if err := browserOpen(robotURL(port)); err != nil {
		cliLogln("Open %s in your browser to view the sandbox (auto-open failed: %v).", robotURL(port), err)
	}
	return nil
}

func restartRobot(ctx context.Context, conn *grpcclient.AgentConnection) error {
	if conn == nil || conn.SimulatorName == "" {
		return fmt.Errorf("managed robot restart requires a named simulator")
	}
	name := conn.SimulatorName
	store, err := robotVMStore()
	if err != nil {
		return err
	}
	unlock, err := lockRobotProvision(ctx, store, name)
	if err != nil {
		return err
	}
	defer unlock()
	profile, exists, err := store.ReadRobotProfile(name)
	if err != nil {
		return err
	}
	if !exists {
		return vm.ErrRobotProfileMissing
	}
	runtime, err := robotRuntimeForKind(profile.Kind)
	if err != nil {
		return err
	}
	if err := runtime.validateSource(name, profile); err != nil {
		return err
	}
	if err := validateRobotVMResources(conn, profile); err != nil {
		return err
	}
	if err := requireRobotAgentCapability(ctx, conn, profile.Kind); err != nil {
		return err
	}
	app, err := findRobotContainer(ctx, conn, runtime.appID)
	if err != nil {
		return err
	}
	if app == nil {
		return reconcileRobotLocked(ctx, conn, store, false)
	}
	if app.GetAppVersion() != robotAppVersion(profile) {
		return fmt.Errorf("managed robot app version %q does not match the pinned profile; use 'wendy vm robot update %s'", app.GetAppVersion(), name)
	}
	port, err := robotSandboxPort(store, ctx, name)
	if err != nil {
		return err
	}
	// A failed worker may leave HTTP responding. It still has to identify this
	// exact managed runtime before restart can stop it. An unavailable endpoint
	// is recoverable once the agent verifies the app ID and pinned version.
	if state, err := readRobotStatus(ctx, port); err == nil {
		if err := state.matches(name, profile); err != nil {
			return err
		}
	}
	if _, err := userVMForConnection(conn); err != nil {
		return err
	}
	if _, err := conn.ContainerService.StopContainer(ctx, &agentpb.StopContainerRequest{AppName: runtime.appID}); err != nil {
		return fmt.Errorf("stopping managed %s runtime for restart: %w", runtime.name, err)
	}
	if _, err := userVMForConnection(conn); err != nil {
		return err
	}
	stream, err := conn.ContainerService.StartContainer(ctx, &agentpb.StartContainerRequest{
		AppName: runtime.appID, RestartPolicy: &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED},
	})
	if err != nil {
		return fmt.Errorf("restarting managed %s runtime: %w", runtime.name, err)
	}
	if err := awaitStarted(stream); err != nil {
		return err
	}
	if err := waitForRobot(ctx, name, port, profile); err != nil {
		return err
	}
	cliLogln("Restarted %s in %s. Sandbox: %s", runtime.name, name, robotURL(port))
	return nil
}

func robotAppVersion(p vm.RobotProfile) string {
	return "0.1.0-" + strings.TrimPrefix(p.SourceDigest, "sha256:")[:12]
}

func findRobotContainer(ctx context.Context, conn *grpcclient.AgentConnection, appID string) (*agentpb.AppContainer, error) {
	stream, err := conn.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		return nil, err
	}
	var found *agentpb.AppContainer
	for {
		response, err := stream.Recv()
		if err == io.EOF {
			return found, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading managed robot container: %w", err)
		}
		if c := response.GetContainer(); c != nil && c.GetAppName() == appID {
			found = c
		}
	}
}

// robotEndpoint is read-only: stopped VMs, lost forwards and foreign listeners
// are reported rather than booted, repaired or accepted from robot.json.
func robotEndpoint(ctx context.Context, name string) (*vm.Store, vm.RobotProfile, int, error) {
	store, err := robotVMStore()
	if err != nil {
		return nil, vm.RobotProfile{}, 0, err
	}
	profile, exists, err := store.ReadRobotProfile(name)
	if err != nil {
		return store, profile, 0, err
	}
	if !exists {
		return store, profile, 0, vm.ErrRobotProfileMissing
	}
	port, err := store.TCPPortMapping(ctx, name, vm.RobotSandboxGuestPort)
	if err == nil && port == 0 {
		err = fmt.Errorf("robot sandbox has no live forward; connect to vm:%s to provision it", name)
	}
	return store, profile, port, err
}

func newVMRobotCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "robot", Short: "Inspect and manage a simulator's virtual robot"}
	configure := &cobra.Command{
		Use:   "configure <name>",
		Short: "Attach a Unitree robot profile to an existing VM",
		Long: "Attach a Unitree Go2 or G1 profile to an existing VM, pinning this CLI's robot runtime. " +
			"Existing robot profiles are never replaced.\n\n" +
			"This records the profile without booting or restarting the VM. " +
			"Run 'wendy vm robot start <name>' to build and start the robot runtime.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error { return runVMRobot(c, "configure", args[0]) },
	}
	configure.Flags().String("profile", vm.RobotKindGo2, "Robot profile: go2 or g1")
	cmd.AddCommand(configure)
	for _, action := range []string{"status", "open", "reset", "start", "restart", "update"} {
		cmd.AddCommand(&cobra.Command{
			Use: action + " <name>", Short: map[string]string{
				"status": "Read live robot identity, health and state", "open": "Open the running robot's sandbox",
				"reset": "Reset the robot world and revoke command ownership", "start": "Start and reconcile the managed robot",
				"restart": "Restart the managed robot to recover from a runtime failure",
				"update":  "Update the profile to this CLI's embedded robot runtime",
			}[action], Args: cobra.ExactArgs(1),
			RunE: func(c *cobra.Command, args []string) error { return runVMRobot(c, action, args[0]) },
		})
	}
	return cmd
}

func runVMRobot(cmd *cobra.Command, action, name string) error {
	if err := vm.ValidName(name); err != nil {
		return err
	}
	if action == "configure" {
		kind := vm.RobotKindGo2
		if cmd.Flags().Lookup("profile") != nil {
			var err error
			kind, err = cmd.Flags().GetString("profile")
			if err != nil {
				return err
			}
		}
		if _, err := robotRuntimeForKind(kind); err != nil {
			return err
		}
		return attachSimulatorProfile(name, kind)
	}
	ctx := cmd.Context()
	if action == "start" || action == "restart" || action == "update" {
		store, err := robotVMStore()
		if err != nil {
			return err
		}
		if profile, exists, err := store.ReadRobotProfile(name); err != nil {
			return err
		} else if !exists {
			return vm.ErrRobotProfileMissing
		} else if action != "update" {
			runtime, err := robotRuntimeForKind(profile.Kind)
			if err != nil {
				return err
			}
			if err := runtime.validateSource(name, profile); err != nil {
				return err
			}
		}
	}
	if action == "update" || action == "restart" {
		addr, started, err := ensureSimulatorRunning(ctx, name)
		if err != nil {
			return err
		}
		conn, err := awaitSimulator(ctx, name, addr, started)
		if err != nil {
			return err
		}
		defer conn.Close()
		if action == "restart" {
			return restartRobot(ctx, conn)
		}
		return reconcileRobot(ctx, conn, true)
	}
	if action == "start" {
		picked, err := connectSimulatorChoice(ctx, &simulatorChoice{Name: name}, true)
		if err != nil {
			return err
		}
		picked.Agent.Close()
		return nil
	}
	_, profile, port, err := robotEndpoint(ctx, name)
	if err != nil {
		return err
	}
	runtime, err := robotRuntimeForKind(profile.Kind)
	if err != nil {
		return err
	}
	state, err := readRobotStatus(ctx, port)
	if err != nil {
		return err
	}
	if err := state.matches(name, profile); err != nil {
		return err
	}
	switch action {
	case "status":
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				robotRuntimeStatus
				SandboxURL string `json:"sandbox_url"`
			}{robotRuntimeStatus: state, SandboxURL: robotURL(port)})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s: %s, %s, healthy=%t, ready=%t, epoch=%d\nSandbox: %s\n", name, runtime.name, state.Mode, state.Healthy, state.Ready, state.Epoch, robotURL(port))
	case "open":
		return browserOpen(robotURL(port))
	case "reset":
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, robotURL(port)+"/api/reset", strings.NewReader("{}"))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := robotHTTPClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("robot reset returned HTTP %d", resp.StatusCode)
		}
		if err := waitForRobot(ctx, name, port, profile); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Reset %s; previous command owners are revoked.\n", name)
	}
	return nil
}

// Resolve a declared ROS app onto the selected robot's bus in memory. The
// source manifest remains unchanged. Host discovery is mapped to the isolated
// guest bus; explicit conflicting domain, middleware or environment choices fail
// before any image build or deployment.
func prepareRobotAppConfig(conn *grpcclient.AgentConnection, cfg *appconfig.AppConfig, overrides []string) (*appconfig.AppConfig, error) {
	if conn == nil || conn.SimulatorName == "" {
		return cfg, nil
	}
	store, err := robotVMStore()
	if err != nil {
		return nil, err
	}
	_, exists, err := store.ReadRobotProfile(conn.SimulatorName)
	if err != nil {
		return nil, err
	}
	if !exists {
		return cfg, nil
	}
	if cfg.AppID == go2RuntimeAppID || cfg.AppID == g1RuntimeAppID {
		return nil, fmt.Errorf("%s is reserved for the managed robot; use 'wendy vm robot update %s'", cfg.AppID, conn.SimulatorName)
	}
	return normalizeRobotROSConfig(cfg, overrides)
}

func normalizeRobotROSConfig(cfg *appconfig.AppConfig, overrides []string) (*appconfig.AppConfig, error) {
	result := *cfg
	configure := func(scope string, ros *appconfig.ROS2Config, entitlements []appconfig.Entitlement, env []string) (*appconfig.FrameworksConfig, []appconfig.Entitlement, error) {
		if ros == nil {
			return nil, entitlements, nil
		}
		if ros.DomainID != nil && *ros.DomainID != 0 {
			return nil, nil, fmt.Errorf("%s: managed robot requires ROS domain 0", scope)
		}
		if ros.ResolvedDistro() != "humble" {
			return nil, nil, fmt.Errorf("%s: managed robot requires ROS 2 Humble", scope)
		}
		if ros.ResolvedRMW() != "rmw_cyclonedds_cpp" {
			return nil, nil, fmt.Errorf("%s: managed robot requires CycloneDDS", scope)
		}
		if ros.ResolvedDiscoveryScope() == "" {
			return nil, nil, fmt.Errorf("%s: invalid ROS discovery scope", scope)
		}
		for _, entry := range env {
			key, value, _ := strings.Cut(entry, "=")
			switch key {
			case "ROS_DOMAIN_ID":
				if value != "0" {
					return nil, nil, fmt.Errorf("%s: ROS_DOMAIN_ID conflicts with managed robot domain 0", scope)
				}
			case "RMW_IMPLEMENTATION":
				if value != "rmw_cyclonedds_cpp" {
					return nil, nil, fmt.Errorf("%s: RMW_IMPLEMENTATION conflicts with managed robot CycloneDDS", scope)
				}
			case "ROS_LOCALHOST_ONLY":
				if value != "1" {
					return nil, nil, fmt.Errorf("%s: ROS_LOCALHOST_ONLY must be 1 for managed robot applications", scope)
				}
			case "ROS_AUTOMATIC_DISCOVERY_RANGE":
				if value != "LOCALHOST" {
					return nil, nil, fmt.Errorf("%s: ROS_AUTOMATIC_DISCOVERY_RANGE must be LOCALHOST for managed robot applications", scope)
				}
			case "CYCLONEDDS_URI", "ROS_DISCOVERY_SERVER", "FASTRTPS_DEFAULT_PROFILES_FILE", "FASTDDS_DEFAULT_PROFILES_FILE", "ROS_STATIC_PEERS":
				if value != "" {
					return nil, nil, fmt.Errorf("%s: %s overrides managed robot loopback discovery", scope, key)
				}
			}
		}
		network := false
		entitlements = append([]appconfig.Entitlement(nil), entitlements...)
		for _, ent := range entitlements {
			if ent.Type == appconfig.EntitlementNetwork {
				network = true
				if ent.Mode != "host" {
					return nil, nil, fmt.Errorf("%s: managed robot ROS applications require network mode host inside the VM", scope)
				}
			}
		}
		if !network {
			entitlements = append(entitlements, appconfig.Entitlement{Type: appconfig.EntitlementNetwork, Mode: "host"})
		}
		domain := 0
		return &appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{DomainID: &domain, Distro: "humble", RMW: "rmw_cyclonedds_cpp", DiscoveryScope: "app"}}, entitlements, nil
	}
	var err error
	if cfg.GetROS2Config() != nil {
		// Services resolve their own environment below. A non-ROS service's
		// environment must not invalidate an inherited ROS framework elsewhere.
		result.Frameworks, result.Entitlements, err = configure("frameworks.ros2", cfg.GetROS2Config(), cfg.Entitlements, mergeEnvEntries(expandServiceEnv(cfg, nil), overrides))
		if err != nil {
			return nil, err
		}
	}
	if len(cfg.Services) != 0 {
		result.Services = make(map[string]*appconfig.ServiceConfig, len(cfg.Services))
		for name, service := range cfg.Services {
			if service == nil {
				return nil, fmt.Errorf("services.%s has no service configuration", name)
			}
			copy := *service
			ros := cfg.ResolveROS2ConfigForService(name)
			if ros != nil {
				// Match the existing service entitlement inheritance rule.
				entitlements := service.Entitlements
				if len(entitlements) == 0 {
					entitlements = cfg.Entitlements
				}
				copy.Frameworks, copy.Entitlements, err = configure("services."+name+".frameworks.ros2", ros, entitlements, mergeEnvEntries(expandServiceEnv(cfg, service), overrides))
				if err != nil {
					return nil, err
				}
			}
			result.Services[name] = &copy
		}
	}
	return &result, nil
}

type simulatorRobotInfo struct {
	Kind  string
	State string
	Hint  string
}

// Picker refresh only observes files, QMP listeners and HTTP health. It never
// boots a VM, repairs a port forward, provisions a runtime, or resets a world.
func readSimulatorRobots(ctx context.Context, statuses []vm.Status) map[string]simulatorRobotInfo {
	result := make(map[string]simulatorRobotInfo)
	store, err := robotVMStore()
	if err != nil {
		return result
	}
	type observation struct {
		name string
		info simulatorRobotInfo
	}
	replies := make(chan observation, len(statuses))
	count := 0
	for _, status := range statuses {
		profile, exists, err := store.ReadRobotProfile(status.Name)
		if err != nil {
			result[status.Name] = simulatorRobotInfo{Kind: "Invalid", State: "profile error", Hint: err.Error()}
			continue
		}
		if !exists {
			continue
		}
		runtime, err := robotRuntimeForKind(profile.Kind)
		if err != nil {
			result[status.Name] = simulatorRobotInfo{Kind: "Invalid", State: "profile error", Hint: err.Error()}
			continue
		}
		info := simulatorRobotInfo{Kind: runtime.name, State: "stopped", Hint: "Connect to start the robot and open its sandbox with 'wendy vm robot open " + status.Name + "'."}
		if !status.Running {
			result[status.Name] = info
			continue
		}
		count++
		go func() {
			probeCtx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			port, err := store.TCPPortMapping(probeCtx, status.Name, vm.RobotSandboxGuestPort)
			info.State = "not ready"
			if err == nil && port != 0 {
				state, err := readRobotStatus(probeCtx, port)
				if err == nil {
					err = state.matches(status.Name, profile)
				}
				if err == nil {
					info.State = state.Mode
					if state.Ready {
						info.State = "ready"
					}
					if !state.Healthy {
						info.State = "unhealthy"
					}
					info.Hint = "Sandbox: " + robotURL(port) + "; 'wendy vm robot open " + status.Name + "' opens it; 'wendy vm robot reset " + status.Name + "' resets the world."
				} else {
					info.Hint = err.Error()
				}
			} else if err != nil {
				info.Hint = err.Error()
			}
			replies <- observation{status.Name, info}
		}()
	}
	for range count {
		item := <-replies
		result[item.name] = item.info
	}
	return result
}
