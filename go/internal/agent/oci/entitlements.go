package oci

import (
	"fmt"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/wendylabsinc/wendy/go/internal/agent/board"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

const (
	// nvidiaGroupGID is the GID for the nvidia group (standard across most distros).
	nvidiaGroupGID uint32 = 44
	// audioGroupGID is the standard audio group GID.
	audioGroupGID uint32 = 29
	// videoGroupGID is the standard video group GID.
	videoGroupGID uint32 = 44
	// inputGroupFallbackGID is used when the host input group cannot be resolved.
	// Debian/Ubuntu commonly use 105; WendyOS deliberately uses 19, so normal
	// operation must resolve the host group instead of assuming this value.
	inputGroupFallbackGID uint32 = 105
	// dialoutGroupGID is the standard dialout group GID (owns serial tty nodes
	// like /dev/ttyACM* and /dev/ttyUSB* on Debian/Ubuntu hosts).
	dialoutGroupGID uint32 = 20
	// appSystemAPIGroupGID is reserved by WendyOS for private app System API
	// sockets, allowing non-root workloads to connect without world access.
	appSystemAPIGroupGID uint32 = 2000
	// v4l2Major is the standard Video4Linux character device major.
	v4l2Major int64 = 81
)

// ApplyOptions configures optional behavior for entitlement application.
type ApplyOptions struct {
	// DBusProxySocketDir is the host directory holding the xdg-dbus-proxy
	// filtered socket prepared for this container — the path returned by
	// dbusproxy.Manager.Start. When non-empty, the bluetooth entitlement
	// bind-mounts it at /var/run/dbus so the container sees an org.bluez-only
	// D-Bus. It must be the exact directory the proxy created (which is keyed
	// by the container name, not the bare app ID); reconstructing it from the
	// app ID alone drops the per-service suffix and the mount source won't
	// exist. When empty, the bluetooth entitlement adds no mount and only sets
	// DBUS_SYSTEM_BUS_ADDRESS — mounting the raw host D-Bus socket directly
	// would expose every system service, so it is never done.
	DBusProxySocketDir string
	// SystemAPISocketDir is the app-specific host directory prepared by
	// AppSystemAPISocketManager. It contains only the narrow System API socket.
	SystemAPISocketDir string
	// DataSocketDir is an app-private directory containing only data.sock, the
	// episode-write grant's socket.
	DataSocketDir string
	// HostResolvConfPath is a wendy-managed resolv.conf that points at a live
	// resolver in the host network namespace. When empty, host networking keeps
	// the compatibility fallback of bind-mounting the host resolver file.
	HostResolvConfPath string
}

// ApplyEntitlements modifies an OCI spec in-place based on app config entitlements.
func ApplyEntitlements(spec *Spec, cfg *appconfig.AppConfig, opts ApplyOptions) error {
	if err := appconfig.ValidateAppID(cfg.AppID); err != nil {
		return fmt.Errorf("invalid app ID: %w", err)
	}
	if cfg.ServiceName != "" {
		if err := appconfig.ValidateServiceName(cfg.ServiceName); err != nil {
			return fmt.Errorf("invalid service name: %w", err)
		}
	}

	didSetDeviceCapabilities := false
	for _, ent := range cfg.Entitlements {
		switch ent.Type {
		case appconfig.EntitlementGPU:
			applyGPU(spec)
		case appconfig.EntitlementNetwork:
			applyNetwork(spec, ent, opts.HostResolvConfPath)
		case appconfig.EntitlementAudio:
			applyAudio(spec)
			if !didSetDeviceCapabilities {
				didSetDeviceCapabilities = true
				SetDeviceCapabilities(spec)
			}
		case appconfig.EntitlementVideo, appconfig.EntitlementCamera:
			applyCamera(spec)
			if !didSetDeviceCapabilities {
				didSetDeviceCapabilities = true
				SetDeviceCapabilities(spec)
			}
		case appconfig.EntitlementPersist:
			applyPersist(spec, ent, cfg.AppID)
		case appconfig.EntitlementBluetooth:
			applyBluetooth(spec, opts.DBusProxySocketDir)
		case appconfig.EntitlementUSB:
			applyUSB(spec)
		case appconfig.EntitlementI2C:
			if err := applyI2C(spec, ent); err != nil {
				return err
			}
		case appconfig.EntitlementGPIO:
			applyGPIO(spec, ent)
		case appconfig.EntitlementSPI:
			applySPI(spec)
		case appconfig.EntitlementInput:
			applyInput(spec)
		case appconfig.EntitlementSerial:
			if err := applySerial(spec, ent); err != nil {
				return err
			}
		case appconfig.EntitlementDisplay:
			applyDisplay(spec)
			if !didSetDeviceCapabilities {
				didSetDeviceCapabilities = true
				SetDeviceCapabilities(spec)
			}
		case appconfig.EntitlementNotifications:
			applySystemAPI(spec, opts.SystemAPISocketDir)
		case appconfig.EntitlementEpisodeWrite:
			applyDataSocket(spec, opts.DataSocketDir)
		case appconfig.EntitlementAdmin:
			applyAdmin(spec)
		case appconfig.EntitlementBuild:
			applyBuild(spec)
		}
	}
	return nil
}

func applyDataSocket(spec *Spec, hostDirectory string) {
	if hostDirectory == "" {
		return
	}
	const containerDirectory = "/run/wendy/data"
	socketPath := filepath.Join(hostDirectory, "data.sock")
	fi, err := os.Lstat(socketPath)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return
	}
	spec.Mounts = append(spec.Mounts, Mount{Destination: containerDirectory, Source: hostDirectory, Type: "bind", Options: []string{"rbind", "nosuid", "noexec", "ro"}})
	spec.Process.User.AdditionalGids = appendUnique(spec.Process.User.AdditionalGids, appSystemAPIGroupGID)
	spec.Process.Env = append(spec.Process.Env, "WENDY_DATA_SOCKET="+containerDirectory+"/data.sock")
}

// SetDeviceCapabilities adds standard device capabilities plus the cgroup
// mount/namespace wiring needed for device-aware workloads. The caller is
// responsible for setting CgroupsPath after this call (client.go sets it
// explicitly so it is the sole authority on the cgroup path). Callers are
// also responsible for adding explicit device cgroup allow rules for each
// entitlement they enable; this helper intentionally does not add a generic
// allow-all devices rule.
func SetDeviceCapabilities(spec *Spec) {
	caps := []string{
		"CAP_CHOWN",
		"CAP_DAC_OVERRIDE",
		"CAP_FSETID",
		"CAP_FOWNER",
		"CAP_MKNOD",
		"CAP_NET_RAW",
		"CAP_SETGID",
		"CAP_SETUID",
		"CAP_SETFCAP",
		"CAP_SETPCAP",
		"CAP_NET_BIND_SERVICE",
		"CAP_KILL",
		"CAP_AUDIT_WRITE",
	}

	if spec.Process.Capabilities == nil {
		spec.Process.Capabilities = &LinuxCapabilities{}
	}
	for _, cap := range caps {
		spec.Process.Capabilities.Bounding = appendUnique(spec.Process.Capabilities.Bounding, cap)
		spec.Process.Capabilities.Effective = appendUnique(spec.Process.Capabilities.Effective, cap)
		spec.Process.Capabilities.Inheritable = appendUnique(spec.Process.Capabilities.Inheritable, cap)
		spec.Process.Capabilities.Permitted = appendUnique(spec.Process.Capabilities.Permitted, cap)
	}

	// Add cgroup mount.
	spec.Mounts = append(spec.Mounts, Mount{
		Destination: "/sys/fs/cgroup",
		Type:        "cgroup",
		Source:      "cgroup",
		Options:     []string{"ro", "nosuid", "noexec", "nodev"},
	})

	if spec.Linux.Resources == nil {
		spec.Linux.Resources = &LinuxResources{}
	}

	// Add cgroup namespace.
	spec.Linux.Namespaces = append(spec.Linux.Namespaces, LinuxNamespace{Type: "cgroup"})
}

// applyGPU adds NVIDIA GPU device access. This provides a minimal fallback
// with device nodes and environment variables. For full GPU support with
// correct library mounts, the caller should apply the NVIDIA CDI spec
// (generated by nvidia-ctk at boot) via the cdi package.
func applyGPU(spec *Spec) {
	// AMD ROCm hosts expose the KFD compute device; NVIDIA hosts never do.
	// Branch early so an AMD box doesn't fall through to the NVIDIA logic below,
	// whose no-live-nodes fallback would mknod bogus major-195 "nvidia*" nodes
	// and allow the wrong major. The two vendors' /dev layouts don't overlap, so
	// this is a clean either/or.
	if _, err := os.Stat(kfdDevicePath); err == nil {
		applyAMDGPU(spec)
		return
	}

	// Add the nvidia group GID for device access.
	spec.Process.User.AdditionalGids = appendUnique(spec.Process.User.AdditionalGids, nvidiaGroupGID)
	// Jetson's integrated GPU also requires the host render group. Resolve the
	// live GID instead of assuming a distro-specific value (104 on the G1's
	// JetPack 6 image). Group membership alone exposes no additional node: the
	// exact GPU devices and cgroup rules below remain the access boundary.
	boardInfo := boardDetect()
	if boardInfo.IsJetson() {
		if gid, ok := lookupRenderGID(); ok {
			spec.Process.User.AdditionalGids = appendUnique(spec.Process.User.AdditionalGids, gid)
		}
	}

	// Derive the device list from the live host nodes: nvidia-uvm's major is
	// dynamically allocated on modern drivers (487 on Thor/JetPack 7, not the
	// classic 195), and the node set varies per board (Thor adds /dev/nvidia1
	// and /dev/nvidia-caps/*). A hardcoded list would mknod wrong-major nodes
	// AND the cgroup rule would block the right ones — guaranteed CUDA failure
	// exactly when this fallback is needed (WDY-1804).
	nodes := discoverNvidiaDeviceNodes()

	if len(nodes) == 0 {
		// No /dev/nvidia* on the host (e.g. driver module not yet loaded).
		// Keep the historical static list so runc still creates the classic
		// major-195 nodes in the container. Whole-major rule: 195 is the
		// statically reserved NVIDIA major, and the real nodes' minors are
		// unknowable here by definition.
		staticPaths := []string{
			"/dev/nvidia0",
			"/dev/nvidiactl",
			"/dev/nvidia-uvm",
			"/dev/nvidia-uvm-tools",
			"/dev/nvidia-modeset",
		}
		for _, devPath := range staticPaths {
			spec.Linux.Devices = append(spec.Linux.Devices, LinuxDevice{
				Path:  devPath,
				Type:  "c",
				Major: 195, // NVIDIA major number.
				Minor: 0,
			})
		}
		major := int64(195)
		spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, LinuxDeviceCgroup{
			Allow:  true,
			Type:   "c",
			Major:  &major,
			Access: "rw",
		})
	} else {
		// One allow rule per discovered major:minor pair — never whole-major.
		// Dynamic majors (nvidia-uvm's 487 on Thor) come from a shared kernel
		// pool, so a whole-major rule could cover unrelated drivers that
		// happen to land on the same number on another boot or kernel. The
		// exact pairs are known here because these very nodes are mknod'd
		// into the container below (no hotplug re-minting on this path).
		// Access is "rw", not "rwm": the container only opens the nodes, so
		// the mknod bit is withheld as least privilege.
		// Note on TOCTOU scope: spec.Linux.Devices entries are mknod'd by the
		// runtime inside the container's own /dev tmpfs from the major:minor
		// recorded here — the host path is never re-opened by the runtime on
		// this (root) path, so a post-scan swap of the host node cannot
		// change what the container receives.
		addExactDeviceNodes(spec, nodes)
	}

	// JetPack 6 requires access to the DRM render node for CUDA initialization
	// on the integrated GPU. Grant renderD* only; card* nodes and compositor
	// sockets remain exclusive to the separate display entitlement.
	if boardInfo.IsJetson() {
		addExactDeviceNodes(spec, discoverRenderDeviceNodes())
	}

	// Add environment variables for NVIDIA.
	spec.Process.Env = append(spec.Process.Env,
		"NVIDIA_VISIBLE_DEVICES=all",
		"NVIDIA_DRIVER_CAPABILITIES=all",
	)

	// On Raspberry Pi, also expose the VideoCore mailbox device for board
	// telemetry. The node is absent on Jetson/generic hosts, where this is
	// skipped entirely.
	if boardInfo.IsRaspberryPi() {
		applyVCIO(spec)
	}
}

// kfdDevicePath is the AMD ROCm compute device (kernel fusion driver). Its
// presence is the signal that distinguishes an AMD ROCm host from an NVIDIA
// one — NVIDIA never creates it. Behind a var so tests can point it at a temp
// node (or a non-existent path to force the NVIDIA branch).
var kfdDevicePath = "/dev/kfd"

// applyAMDGPU wires up AMD ROCm compute access. ROCm needs BOTH nodes: /dev/kfd
// (queue/signal submission) and a /dev/dri/renderD* node (the GPU itself) — with
// KFD alone the runtime initializes but every device allocation fails. card*
// nodes are deliberately NOT granted: display output stays behind the display
// entitlement, matching the renderD-only rule the Jetson path uses above.
func applyAMDGPU(spec *Spec) {
	// /dev/kfd and renderD* are group-owned by "render" (and "video" on some
	// distros). Add both; the exact device nodes and cgroup rules below remain
	// the real access boundary, so group membership alone reaches nothing.
	spec.Process.User.AdditionalGids = appendUnique(spec.Process.User.AdditionalGids, videoGroupGID)
	if gid, ok := lookupRenderGID(); ok {
		spec.Process.User.AdditionalGids = appendUnique(spec.Process.User.AdditionalGids, gid)
	}

	// /dev/kfd is a single, stable node (its major is dynamically allocated), so
	// bind it and scope the cgroup rule to its exact major:minor — rw, no mknod —
	// mirroring i2c/serial, never a whole-major grant. A stat race between the
	// gate in applyGPU and here just means no node is added, not a failed start.
	_, _, _ = addScopedCharDevice(spec, kfdDevicePath)

	// The GPU is the DRM render node. Grant renderD* exactly (mknod'd into the
	// container from the live major:minor), the same mechanism as the Jetson iGPU.
	addExactDeviceNodes(spec, discoverRenderDeviceNodes())
}

// nvidiaDeviceGlobs are the NVIDIA device-node patterns the GPU entitlement
// scans to build its fallback device list. /dev/nvidia* covers nvidia0..N,
// nvidiactl, nvidia-uvm, nvidia-uvm-tools and nvidia-modeset; nvidia-caps/*
// covers the capability nodes (major 501 on Thor). Jetson's L4T CSV can lag
// the live nvgpu driver node set, so also discover the tightly allowlisted
// nvhost and integrated-GPU nodes. Behind a var so tests can redirect into a
// tempdir.
var nvidiaDeviceGlobs = []string{
	"/dev/nvidia*",
	"/dev/nvidia-caps/*",
	"/dev/nvhost-*gpu",
	"/dev/nvgpu/igpu*/*",
}

// renderDeviceGlobs matches the DRM render nodes the GPU entitlement grants —
// on Jetson's integrated GPU (CUDA init needs the render node) and on AMD ROCm
// (the render node IS the GPU). Kept separate from the NVIDIA node scan so
// discrete-NVIDIA hosts keep their existing entitlement shape. Behind a var so
// tests can redirect it into a temporary device tree.
var renderDeviceGlobs = []string{"/dev/dri/renderD*"}

// nvidiaDeviceNameRe is the allowlist of node names the GPU entitlement will
// grant from the globs above. Glob results are host filesystem input; an
// unexpected name (say, a planted /dev/nvidia-evil char device) is rejected
// rather than handed to the container.
var nvidiaDeviceNameRe = regexp.MustCompile(`^(nvidia[0-9]+|nvidiactl|nvidia-uvm|nvidia-uvm-tools|nvidia-modeset|nvidia-cap[0-9]+)$`)

// nvhostGPUDeviceNameRe admits only the GPU-specific nvhost nodes exposed by
// NVIDIA's nvgpu driver. In particular, JetPack 6.2 adds the scheduler control
// FIFO that older L4T devices.csv files omit.
var nvhostGPUDeviceNameRe = regexp.MustCompile(`^nvhost-(gpu|(?:as|ctrl|ctxsw|dbg|nvsched|nvsched_ctrl_fifo|power|prof|prof-ctx|prof-dev|sched|tsg)-gpu)$`)

// nvgpuDeviceNameRe is restricted to children of an igpuN directory by
// isAllowedNvidiaDevicePath; matching a generic basename elsewhere is not
// sufficient to grant it to a container.
var nvgpuDeviceNameRe = regexp.MustCompile(`^(as|channel|ctrl|ctxsw|dbg|nvsched|nvsched_ctrl_fifo|power|prof|prof-ctx|prof-dev|sched|tsg)$`)
var nvgpuDirNameRe = regexp.MustCompile(`^igpu[0-9]+$`)
var renderDeviceNameRe = regexp.MustCompile(`^renderD[0-9]+$`)

type nvidiaDeviceNode struct {
	path         string
	major, minor int64
}

// discoverNvidiaDeviceNodes stats the live NVIDIA nodes matching
// nvidiaDeviceGlobs and returns their real major:minor numbers. Non-device
// matches (e.g. the /dev/nvidia-caps directory itself), stat errors, and
// character devices whose name is not on the NVIDIA allowlist are skipped —
// a host without the driver loaded simply returns nothing.
func discoverNvidiaDeviceNodes() []nvidiaDeviceNode {
	var nodes []nvidiaDeviceNode
	for _, glob := range nvidiaDeviceGlobs {
		matches, err := filepath.Glob(glob)
		if err != nil {
			continue
		}
		for _, p := range matches {
			major, minor, err := statCharDevice(p)
			if err != nil {
				continue
			}
			if !isAllowedNvidiaDevicePath(p) {
				continue
			}
			nodes = append(nodes, nvidiaDeviceNode{path: p, major: major, minor: minor})
		}
	}
	return nodes
}

func discoverRenderDeviceNodes() []nvidiaDeviceNode {
	var nodes []nvidiaDeviceNode
	for _, pattern := range renderDeviceGlobs {
		matches, _ := filepath.Glob(pattern)
		for _, p := range matches {
			if filepath.Base(filepath.Dir(p)) != "dri" || !renderDeviceNameRe.MatchString(filepath.Base(p)) {
				continue
			}
			major, minor, err := statCharDevice(p)
			if err != nil {
				continue
			}
			nodes = append(nodes, nvidiaDeviceNode{path: p, major: major, minor: minor})
		}
	}
	return nodes
}

// addExactDeviceNodes adds runc device definitions and least-privilege cgroup
// rules for live host nodes. Rules are exact major:minor pairs and grant open
// access only (rw, never mknod).
func addExactDeviceNodes(spec *Spec, nodes []nvidiaDeviceNode) {
	seen := map[[2]int64]bool{}
	for _, n := range nodes {
		spec.Linux.Devices = append(spec.Linux.Devices, LinuxDevice{
			Path:  n.path,
			Type:  "c",
			Major: n.major,
			Minor: n.minor,
		})
		key := [2]int64{n.major, n.minor}
		if seen[key] {
			continue
		}
		seen[key] = true
		maj, min := n.major, n.minor
		spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, LinuxDeviceCgroup{
			Allow:  true,
			Type:   "c",
			Major:  &maj,
			Minor:  &min,
			Access: "rw",
		})
	}
}

func isAllowedNvidiaDevicePath(p string) bool {
	base := filepath.Base(p)
	if nvidiaDeviceNameRe.MatchString(base) || nvhostGPUDeviceNameRe.MatchString(base) {
		return true
	}
	return nvgpuDirNameRe.MatchString(filepath.Base(filepath.Dir(p))) && nvgpuDeviceNameRe.MatchString(base)
}

// statCharDevice resolves a host path to its character-device major:minor,
// rejecting anything that is not a real character device node (directories,
// regular files, symlinks). A single Lstat supplies both the type check and
// the device numbers, so the checked inode IS the reported inode — there is
// no follow-up stat for a concurrent symlink swap to redirect (TOCTOU), and
// symlinks are never followed. Behind a var so tests can inject device
// numbers without root/mknod.
var statCharDevice = func(p string) (major, minor int64, err error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return 0, 0, err
	}
	if fi.Mode()&os.ModeCharDevice == 0 {
		return 0, 0, fmt.Errorf("%s is not a character device node", p)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("%s: no raw stat data available", p)
	}
	rdev := uint64(st.Rdev)
	return int64(unix.Major(rdev)), int64(unix.Minor(rdev)), nil
}

// vcioDevicePath is the host VideoCore mailbox device. Behind a var so tests
// can point it at a temp file (a real /dev/vcio only exists on a Raspberry Pi).
var vcioDevicePath = "/dev/vcio"

// applyVCIO exposes the Raspberry Pi VideoCore mailbox device so a container can
// read board telemetry (power/voltage/current/temperature, Pi 5 PMIC ADC)
// through the firmware property interface (e.g. vcgencmd). The node's major is
// dynamically allocated by the firmware/mailbox driver, so it is derived from
// the live node rather than hardcoded. Access is "rw" (no mknod): the container
// only opens the host-created node, it never needs to create one. No-op when the
// node is absent (e.g. vcio not enabled) so a missing bind source cannot stop
// the container from starting.
func applyVCIO(spec *Spec) {
	if _, err := os.Stat(vcioDevicePath); err != nil {
		return
	}
	spec.Mounts = append(spec.Mounts, Mount{
		Destination: "/dev/vcio",
		Source:      vcioDevicePath,
		Type:        "bind",
		Options:     []string{"rbind", "rw", "nosuid", "noexec"},
	})
	// /dev/vcio's major is dynamically allocated, so derive it from the live
	// node. allowMajorsFromGlob dedups and grants the major "rw" (no mknod).
	allowMajorsFromGlob(spec, vcioDevicePath)
}

// applyDisplay grants an app the ability to present to the local display as a
// Wayland client: GPU render-node access via /dev/dri plus, when present, the
// compositor's Wayland socket. It is the ONLY entitlement that exposes
// /dev/dri — apps without it keep the default no-display-GPU sandbox. On Jetson
// the NVIDIA EGL/GLES userspace is injected from the host via CDI; here we only
// ensure the driver advertises graphics+display capabilities.
func applyDisplay(spec *Spec) {
	// /dev/dri/card* is group "video"; renderD* is group "render".
	spec.Process.User.AdditionalGids = appendUnique(spec.Process.User.AdditionalGids, videoGroupGID)
	if gid, ok := lookupRenderGID(); ok {
		spec.Process.User.AdditionalGids = appendUnique(spec.Process.User.AdditionalGids, gid)
	}

	// Allow the DRM major(s) behind /dev/dri (typically 226), discovered at apply
	// time. "rw", no mknod: the host creates the nodes; the bind below surfaces them.
	for _, glob := range driGlobs {
		allowMajorsFromGlob(spec, glob)
	}

	// Bind the host /dev/dri tree so the container can open card*/renderD* nodes.
	// nosuid/noexec but NOT nodev — the whole point is to open device nodes.
	// Skipped when absent so a missing source cannot stop the container starting.
	if _, err := os.Stat("/dev/dri"); err == nil {
		spec.Mounts = append(spec.Mounts, Mount{
			Destination: "/dev/dri",
			Source:      "/dev/dri",
			Type:        "bind",
			Options:     []string{"rbind", "rw", "nosuid", "noexec"},
		})
	}

	// On Jetson the NVIDIA graphics userspace is injected by CDI; ensure the
	// driver capabilities include graphics/display (compute-only otherwise).
	if boardDetect().IsJetson() {
		spec.Process.Env = append(spec.Process.Env,
			"NVIDIA_VISIBLE_DEVICES=all",
			"NVIDIA_DRIVER_CAPABILITIES=all",
		)
	}

	// Bind the compositor's Wayland socket if it exists. Conditional (like the
	// PipeWire socket) so a device with no running compositor still starts the
	// container — the socket simply isn't there yet (no-op-safe for Phase 1).
	const waylandHostSock = "/run/wendyos/wayland-0"
	if fi, err := os.Lstat(waylandHostSock); err == nil && fi.Mode()&os.ModeSocket != 0 {
		spec.Mounts = append(spec.Mounts, Mount{
			Destination: "/run/wendyos/wayland-0",
			Source:      waylandHostSock,
			Type:        "bind",
			Options:     []string{"rbind", "nosuid", "noexec"},
		})
		spec.Process.Env = append(spec.Process.Env,
			"XDG_RUNTIME_DIR=/run/wendyos",
			"WAYLAND_DISPLAY=wayland-0",
		)
	}
}

// applySystemAPI mounts the app's stable, private System API directory. The
// directory is mounted rather than the socket inode so Agent socket recreation
// is transparent to an already-running container.
func applySystemAPI(spec *Spec, hostDirectory string) {
	if hostDirectory == "" {
		return
	}
	const containerDirectory = "/run/wendy/system"
	const containerSocket = containerDirectory + "/system.sock"
	socketPath := filepath.Join(hostDirectory, "system.sock")
	fi, err := os.Lstat(socketPath)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return
	}
	spec.Mounts = append(spec.Mounts, Mount{
		Destination: containerDirectory,
		Source:      hostDirectory,
		Type:        "bind",
		Options:     []string{"rbind", "nosuid", "noexec", "ro"},
	})
	spec.Process.User.AdditionalGids = appendUnique(
		spec.Process.User.AdditionalGids,
		appSystemAPIGroupGID,
	)
	spec.Process.Env = append(spec.Process.Env, "WENDY_SYSTEM_SOCKET="+containerSocket)
}

// applyAdmin grants a container access to the wendy-agent's full local control
// socket. Its parent disk-backed directory is mounted so socket recreation and
// host reboot do not strand a long-lived container on a stale inode.
func applyAdmin(spec *Spec) {
	fi, err := os.Lstat(AdminAgentSocketHostPath)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return
	}
	spec.Mounts = append(spec.Mounts, Mount{
		Destination: ctrAgentSocketDir,
		Source:      filepath.Dir(AdminAgentSocketHostPath),
		Type:        "bind",
		Options:     []string{"rbind", "nosuid", "noexec", "ro"},
	})
	spec.Process.Env = append(spec.Process.Env, "WENDY_AGENT_SOCKET="+ctrAgentSocketPath)
}

// applyNetwork configures the network namespace.
//
// DEPRECATION (specs/2026-07-05-network-bridge-default-design.md): an omitted
// mode maps to "host" below, which is the implicit default that design
// deprecates in favor of an eventual "bridge" default. That mapping is
// intentionally unchanged here — only a WARN log at container create (in the
// containerd package, see hasImplicitHostNetworkMode) flags it. This function
// only gains the new "bridge" mode itself.
func applyNetwork(spec *Spec, ent appconfig.Entitlement, hostResolvConfPath string) {
	mode := ent.Mode
	if mode == "" {
		mode = "host"
	}

	if mode == "host" || mode == "host-admin" {
		// Remove the network namespace to use host networking.
		var namespaces []LinuxNamespace
		for _, ns := range spec.Linux.Namespaces {
			if ns.Type != "network" {
				namespaces = append(namespaces, ns)
			}
		}
		spec.Linux.Namespaces = namespaces

		// When using host networking, sysfs cannot be mounted as a new
		// filesystem because the host network namespace already has it
		// mounted. Replace the sysfs mount with a bind mount from the host.
		for i, m := range spec.Mounts {
			if m.Destination == "/sys" && m.Type == "sysfs" {
				spec.Mounts[i] = Mount{
					Destination: "/sys",
					Type:        "bind",
					Source:      "/sys",
					Options:     []string{"rbind", "nosuid", "noexec", "nodev", "ro"},
				}
				break
			}
		}

		// SECURITY (WDY-1094): CAP_NET_ADMIN lets a container reconfigure host
		// network interfaces, routes, and netfilter — a privilege that plain
		// host networking (visibility: bind ports, see interfaces) does not
		// require. Grant it only for the explicit "host-admin" opt-in, so the
		// reconfiguration capability is separate from, and auditable apart from,
		// the visibility entitlement. (CAP_NET_RAW for ping etc. comes from the
		// baseline capability set, not from here, so it is unaffected.)
		if mode == "host-admin" {
			spec.Process.Capabilities.Bounding = appendUnique(spec.Process.Capabilities.Bounding, "CAP_NET_ADMIN")
			spec.Process.Capabilities.Effective = appendUnique(spec.Process.Capabilities.Effective, "CAP_NET_ADMIN")
			spec.Process.Capabilities.Permitted = appendUnique(spec.Process.Capabilities.Permitted, "CAP_NET_ADMIN")
		}

		// Host-network containers share the host's loopback interface. When a
		// live systemd-resolved stub was detected, HostResolvConfPath points them
		// at 127.0.0.53:53 so DHCP/VPN/upstream changes are followed by the daemon
		// instead of being frozen into a bind-mounted resolver-file inode.
		//
		// systemd-resolved is optional on adopted Linux hosts (and on older
		// WendyOS images), so retain the existing host-file fallback when the
		// caller could not prepare that managed file. The fallback is only a
		// compatibility path: atomic replacement of its source is not visible to
		// a running container, which is why the containerd layer warns when used.
		const resolvedConf = "/run/systemd/resolve/resolv.conf"
		alreadyMounted := false
		for _, m := range spec.Mounts {
			if m.Destination == "/etc/resolv.conf" {
				alreadyMounted = true
				break
			}
		}
		if !alreadyMounted {
			source := hostResolvConfPath
			if source == "" {
				if _, err := os.Stat(resolvedConf); err == nil {
					source = resolvedConf
				} else if _, err := os.Stat("/etc/resolv.conf"); err == nil {
					source = "/etc/resolv.conf"
				}
			}
			if source != "" {
				spec.Mounts = append(spec.Mounts, Mount{
					Destination: "/etc/resolv.conf",
					Type:        "bind",
					Source:      source,
					Options:     []string{"rbind", "ro"},
				})
			}
		}
	} else if mode == "none" || mode == "bridge" {
		// Ensure the network namespace is present (container gets its own isolated network).
		// The default spec already has a network namespace, so this is a no-op in most cases,
		// but we add it explicitly in case it was removed previously.
		//
		// "bridge" mirrors "none" here deliberately: this function adds no host
		// mounts (no /sys bind, no host resolv.conf) and no CAP_NET_ADMIN for
		// bridge mode. What makes "bridge" different from "none" — a private IP,
		// NAT egress, and working DNS — is wired up by the containerd layer
		// attaching the container to the per-app CNI bridge (see
		// needsCNIBridgeWiring in the containerd package), not here.
		hasNetworkNS := false
		for _, ns := range spec.Linux.Namespaces {
			if ns.Type == "network" {
				hasNetworkNS = true
				break
			}
		}
		if !hasNetworkNS {
			spec.Linux.Namespaces = append(spec.Linux.Namespaces, LinuxNamespace{Type: "network"})
		}
	}
}

// applyAudio adds audio device access (ALSA/PipeWire).
func applyAudio(spec *Spec) {
	// Add audio group GID.
	spec.Process.User.AdditionalGids = appendUnique(spec.Process.User.AdditionalGids, audioGroupGID)

	// Mount /dev/snd for ALSA access.
	spec.Mounts = append(spec.Mounts, Mount{
		Destination: "/dev/snd",
		Source:      "/dev/snd",
		Type:        "bind",
		Options:     []string{"rbind", "nosuid", "noexec"},
	})

	// Allow all sound devices (major 116). Whole-major is kept: /dev/snd is a
	// directory of nodes (controlC*, pcmC*D*, seq, timer, …) whose minors aren't
	// known at apply time. Access is "rw", not "rwm": the host owns the nodes and
	// the /dev/snd bind above surfaces them, so the mknod bit is withheld.
	major := int64(116)
	spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, LinuxDeviceCgroup{
		Allow:  true,
		Type:   "c",
		Major:  &major,
		Access: "rw",
	})

	// isSocket reports whether path is a Unix domain socket owned by the
	// expected "wendy" session UID. Uses Lstat so symlinks are not followed —
	// runc can't resolve symlink targets through bind mounts, and a symlink
	// swapped in after the glob must not be. The owner check matters because
	// pipewireUserSocketGlob matches every UID under /run/user: without it,
	// any local UID's session socket could be the one this root-run code
	// bind-mounts into a container.
	isSocket := func(path string) bool {
		fi, err := os.Lstat(path)
		if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode()&os.ModeSymlink != 0 {
			return false
		}
		uid, ok := pipewireUserUID()
		if !ok {
			return false
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		return ok && st.Uid == uid
	}

	// Only a user session socket (/run/user/<uid>/pipewire-0) is mounted.
	// WirePlumber runs inside the wendy user's session, and an instance
	// without a session manager has an empty graph — no sinks, no Bluetooth,
	// and no pulse/native beside it. Handing a container that socket gives it
	// something that looks like audio and plays nothing, so no socket is
	// mounted instead and the container is left with /dev/snd and the audio
	// group, which is all such a host can honestly offer.
	var pipewireSocketSource string
	userSockets, _ := filepath.Glob(pipewireUserSocketGlob)
	for _, s := range userSockets {
		if isSocket(s) {
			pipewireSocketSource = s
			break
		}
	}

	if pipewireSocketSource != "" {
		// Mount the individual socket file into the container.
		spec.Mounts = append(spec.Mounts, Mount{
			Destination: "/run/pipewire/pipewire-0",
			Source:      pipewireSocketSource,
			Type:        "bind",
			Options:     []string{"rbind", "nosuid", "noexec"},
		})
		spec.Process.Env = append(spec.Process.Env,
			"PIPEWIRE_RUNTIME_DIR=/run/pipewire",
		)

		// Check for PulseAudio compat socket in the same source directory.
		// PipeWire provides a PulseAudio emulation socket that GStreamer's
		// autoaudiosink needs (pulsesink has the highest rank).
		sourceDir := filepath.Dir(pipewireSocketSource)
		pulseNative := filepath.Join(sourceDir, "pulse", "native")
		if isSocket(pulseNative) {
			spec.Mounts = append(spec.Mounts, Mount{
				Destination: "/run/pipewire/pulse-native",
				Source:      pulseNative,
				Type:        "bind",
				Options:     []string{"rbind", "nosuid", "noexec"},
			})
			spec.Process.Env = append(spec.Process.Env,
				"PULSE_SERVER=unix:/run/pipewire/pulse-native",
			)
		}
	}
}

// statMajor extracts the device major number from the host node at path.
// Exposed as a var so tests can inject majors without creating real device
// nodes (which requires root and mknod).
var statMajor = func(p string) (int64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		return 0, err
	}
	return int64(unix.Major(uint64(st.Rdev))), nil
}

// boardDetect is the package-level board probe, behind a var so tests can
// simulate Jetson/RPi/Generic hosts.
var boardDetect = board.Detect

// udevRuntimeDir is the host udev runtime directory bind-mounted into camera
// containers. libcamera enumerates media/CSI devices through libudev, which
// reads the udevd-maintained database under /run/udev/data; without it the
// in-container enumerator returns nothing (WDY-1342). Behind a var so tests
// can point it at a path that exists on the test host.
var udevRuntimeDir = "/run/udev"

// driGlobs are the DRM/KMS device-node patterns the display entitlement scans
// to discover the render major(s) (typically 226). Behind a var so tests can
// redirect into a tempdir.
var driGlobs = []string{"/dev/dri/*"}

// pipewireUserSocketGlob locates the PipeWire socket the audio entitlement
// mounts. Behind a var so tests can redirect it into a tempdir.
var pipewireUserSocketGlob = "/run/user/*/pipewire-0"

// pipewireUserUID resolves the "wendy" user's UID, the only session whose
// socket applyAudio will bind-mount into a container. Behind a var so tests
// can stub it without requiring a real "wendy" account on the test host. See
// the agent's own audio package for the equivalent used by its PipeWire
// client.
var pipewireUserUID = func() (uint32, bool) {
	u, err := user.Lookup("wendy")
	if err != nil {
		return 0, false
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(uid), true
}

// lookupRenderGID resolves the host "render" group GID, which owns
// /dev/dri/renderD*. Behind a var so tests can stub it. Returns ok=false when
// the host has no render group (then only the video GID is added).
var lookupRenderGID = func() (uint32, bool) {
	g, err := user.LookupGroup("render")
	if err != nil {
		return 0, false
	}
	gid, err := strconv.ParseUint(g.Gid, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(gid), true
}

// lookupInputGID resolves the host group that owns /dev/input. It is behind a
// var so tests can cover WendyOS's GID 19 and the legacy fallback without
// depending on the developer machine's group database.
var lookupInputGID = func() (uint32, bool) {
	g, err := user.LookupGroup("input")
	if err != nil {
		return 0, false
	}
	gid, err := strconv.ParseUint(g.Gid, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(gid), true
}

// AdminAgentSocketHostPath is the host wendy-agent local control socket exposed
// to containers granted the admin entitlement. It is the single source of truth
// for the host socket location — the agent's local listener (main.go) uses it
// too, so the path the agent serves and the path applyAdmin bind-mounts can
// never drift apart.
//
// It lives in its own directory (…/agent-control) so applyAdmin can bind-mount
// that directory rather than the socket file — see applyAdmin for why.
//
// It lives on the disk-backed, /data-persistent /var/lib/wendy tree, NOT on the
// tmpfs /run: the admin bind mount pins the socket directory's inode, and tmpfs
// is wiped (fresh inode) on every boot. A /run path therefore stranded a
// long-lived admin container on the orphaned pre-reboot directory inode after a
// reboot — the socket read as ENOENT inside the container even though the live
// agent was serving one in the new /run directory. A disk-backed directory
// keeps a stable inode across reboots (and, under /var/lib/wendy, across A/B OTA
// too), so the mount never goes stale.
//
// Behind a var so tests can point it at a temp socket.
var AdminAgentSocketHostPath = "/var/lib/wendy/agent-control/agent.sock"

// ctrAgentSocketDir / ctrAgentSocketPath are the in-container destinations for
// the admin socket directory mount and the socket within it (WENDY_AGENT_SOCKET).
const (
	ctrAgentSocketDir  = "/run/wendy/agent"
	ctrAgentSocketPath = "/run/wendy/agent/agent.sock"
)

// applyCamera adds camera/V4L2 device access, plus the additional kernel
// device majors that libcamera (and on Jetson, nvargus/nvhost) require. The
// camera entitlement is intentionally one-size-fits-all: it covers both
// USB UVC cameras and CSI ribbon cameras, so applications that declare the
// entitlement do not need to know which transport their device is on.
func applyCamera(spec *Spec) {
	spec.Process.User.AdditionalGids = appendUnique(spec.Process.User.AdditionalGids, videoGroupGID)

	// Allow video4linux devices (major 81). Access is "rw", not "rwm": the
	// container opens device nodes that the host kernel creates and that the
	// /dev bind mount below surfaces live — it never needs to *create* device
	// nodes itself, so withholding the mknod bit removes a container-escape
	// primitive without affecting camera capture. The major is intentionally
	// left minor-unrestricted: USB webcam hotplug recreates /dev/videoN under a
	// new minor, and pinning a minor discovered at apply time would deny the
	// device after a replug (see the /dev bind-mount rationale below).
	major := v4l2Major
	spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, LinuxDeviceCgroup{
		Allow:  true,
		Type:   "c",
		Major:  &major,
		Access: "rw",
	})

	// Replace the isolated /dev tmpfs with a live bind mount of the host /dev
	// tree. USB webcam hotplug can recreate /dev/videoN with a different node
	// name after unplug/replug, and an OCI device snapshot cannot update inside
	// a running container. Binding host /dev keeps /dev/video* and /dev/v4l
	// current without requiring container restart.
	//
	// nodev is intentionally omitted here (unlike the /run/udev bind below):
	// the camera entitlement's whole purpose is to let the container *open the
	// device nodes* under /dev — applying nodev would make every /dev/video*,
	// /dev/media* etc. unusable as a device. Access is still gated by the
	// per-major cgroup allow rules above (deny-all baseline), so this does not
	// expose arbitrary host devices. Do not add nodev to this mount.
	replaceMount(spec, Mount{
		Destination: "/dev",
		Source:      "/dev",
		Type:        "bind",
		Options:     []string{"rbind", "rw", "nosuid", "noexec"},
	})

	// libcamera and the V4L2 media subsystem rely on additional character
	// device nodes whose majors are dynamic across kernels. Discover them at
	// apply time and add cgroup allow rules so containers can open them.
	for _, glob := range cameraExtraGlobs {
		allowMajorsFromGlob(spec, glob)
	}

	// On Jetson, the nvargus camera stack also needs /dev/nvhost-* and
	// /dev/nvmap. These nodes are absent on non-Jetson hosts, so the globs are
	// a no-op there.
	if boardDetect().IsJetson() {
		for _, glob := range jetsonExtraGlobs {
			allowMajorsFromGlob(spec, glob)
		}
	}

	// Bind the host udev runtime read-only. libcamera enumerates cameras
	// through libudev, which reads the udevd-maintained database under
	// /run/udev/data. The device nodes and cgroup rules above are necessary
	// but not sufficient for CSI/libcamera: with /run/udev absent the udev
	// enumerator returns nothing and `cam -l` is empty, so apps fall back to a
	// synthetic source (WDY-1342). USB/V4L2 capture does not need this, but the
	// camera entitlement is one-size-fits-all so it is added unconditionally.
	// ro/nosuid/noexec/nodev: the container only reads the udev database, never
	// writes it or executes from it. Skipped when the host has no udev runtime
	// (e.g. minimal/non-systemd hosts) so the bind's missing source cannot stop
	// the container from starting.
	if _, err := os.Stat(udevRuntimeDir); err == nil {
		spec.Mounts = append(spec.Mounts, Mount{
			Destination: "/run/udev",
			Source:      udevRuntimeDir,
			Type:        "bind",
			Options:     []string{"rbind", "ro", "nosuid", "noexec", "nodev"},
		})
	}
}

// cameraExtraGlobs is the list of device-node patterns the camera entitlement
// scans on every host. Exposed as a var so tests can redirect into a tempdir.
//
// SECURITY SCOPE (deliberate): each matched node grants its whole device *major*
// (rw, no mknod), not a single minor. This is broader than the camera strictly
// needs — e.g. /dev/dma_heap/* shares its major with heaps used by GPU/display
// codecs — but is intentional and required, not an oversight:
//   - These subsystems are how libcamera/V4L2 capture works: media controllers
//     (/dev/media*), sub-device nodes (/dev/v4l-subdev*) and dma-buf heaps
//     (/dev/dma_heap/*) are all opened by a containerized camera app at runtime.
//   - Minor numbers are NOT stable: USB-webcam replug and dynamic media-graph
//     creation re-mint minors, so a minor pinned at apply time would deny the
//     device after a hotplug. The /dev bind mount (see applyCamera) surfaces the
//     live nodes; a per-major rule is what keeps them reachable.
//   - The cgroup device model keys on major:minor, so "only the cma/system
//     dma-heap" cannot be expressed without enumerating minors that libcamera is
//     free to choose at allocation time — narrowing would be both fragile and
//     capture-breaking.
//
// The entitlement is therefore coarse by design for a single-purpose embedded
// device. Containers still get rw (not rwm), withholding the mknod escape
// primitive. Operators granting `camera` should understand it implies access to
// the host's media/dma-heap majors.
var cameraExtraGlobs = []string{
	"/dev/media*",
	"/dev/v4l-subdev*",
	"/dev/dma_heap/*",
}

// jetsonExtraGlobs adds Jetson-only device patterns. Same major-level rationale
// and tradeoff as cameraExtraGlobs: the nvargus stack needs the nvhost/nvmap
// majors and they cannot be minor-scoped to "camera only" without breaking the
// Argus ISP/encoder path.
var jetsonExtraGlobs = []string{
	"/dev/nvhost-*",
	"/dev/nvmap",
}

// allowMajorsFromGlob stats every path matching glob, extracts the device
// major, and appends a deduplicated cgroup allow rule for that major. Missing
// paths and stat errors are silently skipped so non-existent globs are
// no-ops.
func allowMajorsFromGlob(spec *Spec, glob string) {
	matches, err := filepath.Glob(glob)
	if err != nil {
		return
	}
	seen := existingMajors(spec)
	for _, p := range matches {
		major, err := statMajor(p)
		if err != nil {
			continue
		}
		if seen[major] {
			continue
		}
		seen[major] = true
		m := major
		// "rw", not "rwm": these auxiliary media/dma-heap/v4l-subdev (and Jetson
		// nvhost/nvmap) nodes are opened by the container, never created by it,
		// so the mknod bit is unnecessary and is withheld as least privilege.
		spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, LinuxDeviceCgroup{
			Allow:  true,
			Type:   "c",
			Major:  &m,
			Access: "rw",
		})
	}
}

// existingMajors returns the set of major numbers already covered by an
// allow rule on the spec, so callers can avoid emitting duplicates.
func existingMajors(spec *Spec) map[int64]bool {
	out := map[int64]bool{}
	for _, d := range spec.Linux.Resources.Devices {
		if d.Allow && d.Major != nil {
			out[*d.Major] = true
		}
	}
	return out
}

// applyVideo is a deprecated alias for camera/V4L2 device access.
func applyVideo(spec *Spec) {
	applyCamera(spec)
}

// applyPersist adds a persistent volume bind mount, creating the host
// directory if it does not already exist. Volumes are shared across all
// apps by name — two apps that declare the same volume name will see the
// same host directory.
func applyPersist(spec *Spec, ent appconfig.Entitlement, appID string) {
	// Sanitize the volume name to prevent path traversal.
	name := filepath.Base(ent.Name)
	if name == "." || name == ".." || name == "/" || name == "" {
		return
	}
	// Validate the container destination as a POSIX path: it must be
	// absolute with no dot-dot components. Check the original before cleaning
	// so "a/../b" is rejected even though Clean would resolve it to a valid
	// absolute path.
	if !path.IsAbs(ent.Path) {
		return
	}
	for _, component := range strings.Split(ent.Path, "/") {
		if component == ".." {
			return
		}
	}
	dest := path.Clean(ent.Path)
	hostPath := filepath.Join("/var/lib/wendy/volumes", name)
	if err := os.MkdirAll(hostPath, 0o755); err != nil {
		// Best-effort: the container will fail to start with a clear mount error
		// if the directory truly cannot be created, so we don't abort here.
		_ = err
	}
	spec.Mounts = append(spec.Mounts, Mount{
		Destination: dest,
		Source:      hostPath,
		Type:        "bind",
		Options:     []string{"rbind", "nosuid", "noexec"},
	})
}

// applyBluetooth adds D-Bus socket mounts for Bluetooth access.
// When proxySocketDir is non-empty, it mounts that xdg-dbus-proxy filtered
// socket directory (only org.bluez allowed) at /var/run/dbus. The directory
// is supplied by the caller (the value dbusproxy.Manager.Start returned for
// this container) rather than reconstructed here, so the mount source always
// matches what the proxy actually created. When empty, it adds no mount and
// never falls back to the raw host D-Bus sockets.
func applyBluetooth(spec *Spec, proxySocketDir string) {
	if proxySocketDir != "" {
		// Mount the filtered proxy socket directory.
		spec.Mounts = append(spec.Mounts, Mount{
			Destination: "/var/run/dbus",
			Source:      proxySocketDir,
			Type:        "bind",
			Options:     []string{"rbind", "nosuid", "noexec"},
		})
	}
	// When the proxy is not available, we intentionally skip mounting the
	// raw host D-Bus sockets. Mounting /var/run/dbus or /run/dbus directly
	// exposes every D-Bus service (NetworkManager, systemd, polkit, etc.)
	// giving the container root-level network control. Bluetooth access
	// requires xdg-dbus-proxy to scope D-Bus visibility to org.bluez only.

	spec.Process.Env = append(spec.Process.Env,
		"DBUS_SYSTEM_BUS_ADDRESS=unix:path=/var/run/dbus/system_bus_socket",
	)
}

// applyUSB adds USB device access.
func applyUSB(spec *Spec) {
	// Mount /dev/bus/usb for USB access.
	spec.Mounts = append(spec.Mounts, Mount{
		Destination: "/dev/bus/usb",
		Source:      "/dev/bus/usb",
		Type:        "bind",
		Options:     []string{"rbind", "rw"},
	})

	// Allow USB devices (major 189). Whole-major is kept: /dev/bus/usb is a tree
	// of per-device nodes that hotplug re-mints under new minors, so the minors
	// aren't known at apply time. Access is "rw", not "rwm": the host owns the
	// nodes and the bind above surfaces them, so the mknod bit is withheld.
	major := int64(189)
	spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, LinuxDeviceCgroup{
		Allow:  true,
		Type:   "c",
		Major:  &major,
		Access: "rw",
	})
}

// i2cMajor is the standard I2C character-device major (i2c-dev).
const i2cMajor int64 = 89

// applyI2C adds access to a single named I2C bus (e.g. /dev/i2c-1). The bus is a
// single, stable node — not a hotplug or directory device — so it is scoped to
// the node's exact major:minor (via addScopedCharDevice), mirroring serial,
// rather than granted the whole I2C major. Returns an error when the bus is
// absent or malformed so the container fails fast with a clear message.
func applyI2C(spec *Spec, ent appconfig.Entitlement) error {
	// Validate device name is i2c-N before constructing a path from it
	// (defense-in-depth against path traversal; appconfig validates this too).
	if !strings.HasPrefix(ent.Device, "i2c-") {
		return fmt.Errorf("i2c: device must be in i2c-N format, got %q", ent.Device)
	}
	suffix := ent.Device[len("i2c-"):]
	if suffix == "" {
		return fmt.Errorf("i2c: device must be in i2c-N format, got %q", ent.Device)
	}
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return fmt.Errorf("i2c: device must be in i2c-N format, got %q", ent.Device)
		}
	}
	devPath := filepath.Clean(fmt.Sprintf("/dev/%s", ent.Device))

	// Resolve the exact node and scope the cgroup rule to this one bus's
	// major:minor with "rw" (no mknod), never the whole I2C major.
	major, _, err := addScopedCharDevice(spec, devPath)
	if err != nil {
		return fmt.Errorf("i2c bus %s unavailable (need a real, present bus node): %w", devPath, err)
	}
	// Defense-in-depth: the resolved node must actually be on the I2C major, so a
	// node that isn't an i2c bus can't smuggle in access to an unrelated major.
	if major != i2cMajor {
		return fmt.Errorf("i2c bus %s has unexpected major %d (want %d); refusing", devPath, major, i2cMajor)
	}
	return nil
}

// serialDeviceMajors maps a serial tty node-name prefix to its kernel character
// device major. ttyACM = USB CDC-ACM (cdc_acm), ttyUSB = USB-serial bridges
// (FTDI/CH340/CP210x via usbserial). The entitlement is deliberately USB-only:
// on-board UARTs (ttyAMA, ttyS) are excluded because ttyS in particular shares
// its major with a board's system-console UART, adding attack surface for no
// peripheral benefit. Prefixes are validated upstream by appconfig.isValidSerialDevice.
var serialDeviceMajors = map[string]int64{
	"ttyACM": 166,
	"ttyUSB": 188,
}

// serialDeviceMajor returns the cgroup device major for a serial tty node name
// (e.g. "ttyACM0" → 166) and whether the name is a recognized, well-formed node
// (known prefix followed by one or more digits). The full-name validation here
// is defense-in-depth against a malformed device slipping past appconfig
// validation: it guarantees applySerial never bind-mounts a path like
// "ttyACM0/../sda" that filepath.Clean would resolve outside the intended node.
func serialDeviceMajor(device string) (int64, bool) {
	for _, prefix := range appconfig.SerialDevicePrefixes {
		if !strings.HasPrefix(device, prefix) {
			continue
		}
		suffix := device[len(prefix):]
		if suffix == "" {
			return 0, false
		}
		for _, c := range suffix {
			if c < '0' || c > '9' {
				return 0, false
			}
		}
		major, ok := serialDeviceMajors[prefix]
		return major, ok
	}
	return 0, false
}

// statDeviceNode resolves a host device node to its character-device
// major:minor. It rejects a node that does not exist or is a symlink: runc
// cannot resolve a symlink target through a bind mount, and a symlink would let
// the validated node differ from the one ultimately bound. Lstat does not
// follow the link (mirrors the approach in applyAudio). Behind a var so tests
// can inject device numbers without root/mknod.
//
// This is the shared stat/scope/symlink resolver for every entitlement that
// surfaces a single, named device node (serial, i2c). Whole-major entitlements
// (usb, gpio, spi, input, gpu, audio, camera) don't use it because they expose a
// directory or hotplug node set whose minors aren't known at apply time.
var statDeviceNode = func(p string) (major, minor int64, err error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return 0, 0, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return 0, 0, fmt.Errorf("%s is a symlink; want a real device node", p)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		return 0, 0, err
	}
	rdev := uint64(st.Rdev)
	return int64(unix.Major(rdev)), int64(unix.Minor(rdev)), nil
}

// addScopedCharDevice resolves a single host character-device node, bind-mounts
// it, and emits a cgroup allow rule scoped to its exact major:minor. It returns
// the resolved major:minor so callers can validate the node is the device they
// expect. The cgroup rule is "rw" (no mknod): the host owns the node and the
// bind mount surfaces it, so the container only ever opens it — it never needs
// to create one. The mount is nosuid/noexec: a device node is opened for I/O,
// never executed and never a setuid surface. On a stat error (missing node or
// symlink) nothing is appended and the error is returned, so the caller fails
// fast with a clear message instead of a cryptic mount error at container start.
func addScopedCharDevice(spec *Spec, devPath string) (major, minor int64, err error) {
	major, minor, err = statDeviceNode(devPath)
	if err != nil {
		return 0, 0, err
	}
	spec.Mounts = append(spec.Mounts, Mount{
		Destination: devPath,
		Source:      devPath,
		Type:        "bind",
		Options:     []string{"rbind", "rw", "nosuid", "noexec"},
	})
	maj, min := major, minor
	spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, LinuxDeviceCgroup{
		Allow:  true,
		Type:   "c",
		Major:  &maj,
		Minor:  &min,
		Access: "rw",
	})
	return major, minor, nil
}

// applySerial adds access to a single serial tty device (e.g. a USB-attached
// servo bus or sensor on /dev/ttyACM0). Unlike the usb entitlement — which
// exposes raw libusb access via /dev/bus/usb (major 189) — this grants the
// kernel tty node that pyserial/termios open, which is a different device major
// (166 for ttyACM, 188 for ttyUSB). The device field is a bare node name
// validated by appconfig.isValidSerialDevice, so it cannot contain a path
// separator or escape /dev.
func applySerial(spec *Spec, ent appconfig.Entitlement) error {
	wantMajor, ok := serialDeviceMajor(ent.Device)
	if !ok {
		return fmt.Errorf("serial: unrecognized device name %q", ent.Device)
	}
	devPath := filepath.Clean(fmt.Sprintf("/dev/%s", ent.Device))

	// Resolve the exact node and scope the cgroup rule to this one device
	// (major:minor), never the whole kernel major. A whole-major rule would
	// expose every other device sharing that major on the host — every ttyACM*
	// or ttyUSB* adapter (SOC2-CC6, ISO27001-A.8, NIST-AC-3). addScopedCharDevice
	// also fails fast and clearly when the device is not connected, instead of a
	// cryptic mount error at start.
	major, _, err := addScopedCharDevice(spec, devPath)
	if err != nil {
		return fmt.Errorf("serial device %s unavailable (need a real, connected tty node): %w", devPath, err)
	}
	// Defense-in-depth: the resolved node must be the character-device major its
	// name implies, so a node that isn't the expected serial device can't smuggle
	// in access to an unrelated major. (The scoped rule above is discarded with
	// the whole spec when this returns an error, so nothing is granted.)
	if major != wantMajor {
		return fmt.Errorf("serial device %s has unexpected major %d (want %d for %q); refusing", devPath, major, wantMajor, ent.Device)
	}

	// Serial tty nodes are group-owned by dialout; resolve its GID on the host so
	// a non-root process can open the port, falling back to the Debian/Ubuntu
	// default when the group is absent (mirrors applySPI's group lookup). This GID
	// applies process-tree-wide, but the major:minor cgroup rule above is the real
	// access gate — membership alone reaches no device the cgroup rule denies.
	dialoutGID := dialoutGroupGID
	if grp, gerr := user.LookupGroup("dialout"); gerr == nil {
		if gid, perr := strconv.ParseUint(grp.Gid, 10, 32); perr == nil {
			dialoutGID = uint32(gid)
		}
	}
	spec.Process.User.AdditionalGids = appendUnique(spec.Process.User.AdditionalGids, dialoutGID)
	return nil
}

// applyGPIO adds GPIO device access for specified pins.
func applyGPIO(spec *Spec, ent appconfig.Entitlement) {
	// Mount gpiochip devices that exist on the host.
	for i := 0; i < 8; i++ {
		devPath := fmt.Sprintf("/dev/gpiochip%d", i)
		if _, err := os.Stat(devPath); err == nil {
			spec.Mounts = append(spec.Mounts, Mount{
				Destination: devPath,
				Source:      devPath,
				Type:        "bind",
				Options:     []string{"rbind", "rw"},
			})
		}
	}

	// Allow GPIO devices (major 254). Whole-major is kept: a host can expose up to
	// eight /dev/gpiochip* nodes and access is chip-level (pins are validated, not
	// gated per-node), so all chips share the major. Access is "rw", not "rwm":
	// the host owns the nodes and the binds above surface them, so mknod is withheld.
	major := int64(254)
	spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, LinuxDeviceCgroup{
		Allow:  true,
		Type:   "c",
		Major:  &major,
		Access: "rw",
	})

	_ = ent.Pins // Pins are used for documentation/validation; access is chip-level.
}

// applySPI adds SPI device access.
func applySPI(spec *Spec) {
	// Mount SPI devices that exist on the host.
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			devPath := fmt.Sprintf("/dev/spidev%d.%d", i, j)
			if _, err := os.Stat(devPath); err == nil {
				spec.Mounts = append(spec.Mounts, Mount{
					Destination: devPath,
					Source:      devPath,
					Type:        "bind",
					Options:     []string{"rbind", "rw"},
				})
			}
		}
	}

	// Add SPI group GID for device permissions (group name varies by distro).
	if grp, err := user.LookupGroup("spi"); err == nil {
		if gid, err := strconv.ParseUint(grp.Gid, 10, 32); err == nil {
			spec.Process.User.AdditionalGids = appendUnique(spec.Process.User.AdditionalGids, uint32(gid))
		}
	}

	// Allow SPI devices (major 153). Whole-major is kept: a host can expose up to
	// sixteen /dev/spidev*.* nodes (bus.chipselect) and the entitlement grants the
	// SPI subsystem rather than one bus, so the minors aren't fixed at apply time.
	// Access is "rw", not "rwm": the host owns the nodes and the binds above
	// surface them, so the mknod bit is withheld.
	spiMajor := int64(153)
	spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, LinuxDeviceCgroup{
		Allow:  true,
		Type:   "c",
		Major:  &spiMajor,
		Access: "rw",
	})
}

// applyInput adds Linux input access (game controllers, scanners, keyboards,
// etc.).
func applyInput(spec *Spec) {
	// Add the host's input group for /dev/input permissions. Group IDs are an
	// image policy detail rather than a portable ABI: WendyOS uses 19 while
	// Debian/Ubuntu commonly use 105. The supplemental group is what lets a
	// non-root OCI process open the recursively mounted event nodes.
	inputGID := inputGroupFallbackGID
	if gid, ok := lookupInputGID(); ok {
		inputGID = gid
	}
	spec.Process.User.AdditionalGids = appendUnique(spec.Process.User.AdditionalGids, inputGID)

	// Mount /dev/input for HID event devices.
	spec.Mounts = append(spec.Mounts, Mount{
		Destination: "/dev/input",
		Source:      "/dev/input",
		Type:        "bind",
		Options:     []string{"rbind", "nosuid", "noexec"},
	})

	// Allow input devices (major 13). Whole-major is kept: /dev/input is a
	// directory of event/mouse/js nodes that hotplug re-mints under new minors, so
	// the minors aren't known at apply time. Access is "rw", not "rwm": the host
	// owns the nodes and the bind above surfaces them, so the mknod bit is withheld.
	major := int64(13)
	spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, LinuxDeviceCgroup{
		Allow:  true,
		Type:   "c",
		Major:  &major,
		Access: "rw",
	})
}

// appendUnique appends a value to a slice only if it is not already present.
func appendUnique[T comparable](slice []T, val T) []T {
	for _, v := range slice {
		if v == val {
			return slice
		}
	}
	return append(slice, val)
}

func replaceMount(spec *Spec, mount Mount) {
	for i, existing := range spec.Mounts {
		if existing.Destination == mount.Destination {
			spec.Mounts[i] = mount
			return
		}
	}
	spec.Mounts = append(spec.Mounts, mount)
}

// applyBuild relaxes the hardened container profile just enough to run a nested
// container image builder (BuildKit) in-container: it adds CAP_SYS_ADMIN (mount /
// pivot_root / namespace creation for BuildKit's runc executor) and un-denies the
// unshare + clone(CLONE_NEWUSER) syscalls the default seccomp profile blocks. The
// module-load and kexec denials are deliberately KEPT (relaxSeccompForBuild), so
// the grant is scoped to what BuildKit needs. This is privileged-equivalent — see
// the security note in entitlements.md.
func applyBuild(spec *Spec) {
	if spec.Process.Capabilities == nil {
		spec.Process.Capabilities = &LinuxCapabilities{}
	}
	// BuildKit's runc executor needs a full privileged capability set, not just
	// CAP_SYS_ADMIN: nested-container setup, overlay/bind mounts, and the
	// cgroup-v2 device controller (eBPF, needs CAP_BPF/CAP_PERFMON on kernel
	// >=5.8) all require more. Granting the whole set matches what `--privileged`
	// gives and what the entitlement already documents (privileged-equivalent);
	// CAP_SYS_ADMIN alone is already escape-capable, so the rest add no new blast
	// radius. The kernel-module/kexec SYSCALLS stay denied by seccomp regardless
	// (relaxSeccompForBuild keeps those rules), so CAP_SYS_MODULE/CAP_SYS_BOOT are
	// inert here — defense in depth.
	for _, cap := range allLinuxCapabilities {
		spec.Process.Capabilities.Bounding = appendUnique(spec.Process.Capabilities.Bounding, cap)
		spec.Process.Capabilities.Effective = appendUnique(spec.Process.Capabilities.Effective, cap)
		spec.Process.Capabilities.Permitted = appendUnique(spec.Process.Capabilities.Permitted, cap)
		spec.Process.Capabilities.Inheritable = appendUnique(spec.Process.Capabilities.Inheritable, cap)
	}
	relaxSeccompForBuild(spec)
	relaxCgroupsForBuild(spec)
}

// allLinuxCapabilities is the full capability set a `--privileged` container
// receives. The build entitlement grants it because BuildKit's nested runc
// needs far more than CAP_SYS_ADMIN (notably CAP_BPF/CAP_PERFMON for the
// cgroup-v2 device controller, CAP_NET_ADMIN, CAP_SYS_RESOURCE, etc.).
var allLinuxCapabilities = []string{
	"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_DAC_READ_SEARCH", "CAP_FOWNER",
	"CAP_FSETID", "CAP_KILL", "CAP_SETGID", "CAP_SETUID", "CAP_SETPCAP",
	"CAP_LINUX_IMMUTABLE", "CAP_NET_BIND_SERVICE", "CAP_NET_BROADCAST",
	"CAP_NET_ADMIN", "CAP_NET_RAW", "CAP_IPC_LOCK", "CAP_IPC_OWNER",
	"CAP_SYS_MODULE", "CAP_SYS_RAWIO", "CAP_SYS_CHROOT", "CAP_SYS_PTRACE",
	"CAP_SYS_PACCT", "CAP_SYS_ADMIN", "CAP_SYS_BOOT", "CAP_SYS_NICE",
	"CAP_SYS_RESOURCE", "CAP_SYS_TIME", "CAP_SYS_TTY_CONFIG", "CAP_MKNOD",
	"CAP_LEASE", "CAP_AUDIT_WRITE", "CAP_AUDIT_CONTROL", "CAP_SETFCAP",
	"CAP_MAC_OVERRIDE", "CAP_MAC_ADMIN", "CAP_SYSLOG", "CAP_WAKE_ALARM",
	"CAP_BLOCK_SUSPEND", "CAP_AUDIT_READ", "CAP_PERFMON", "CAP_BPF",
	"CAP_CHECKPOINT_RESTORE",
}

// relaxCgroupsForBuild lets BuildKit's runc executor create the per-build-step
// cgroup it needs. The hardened default mounts /sys/fs/cgroup read-only, so a
// build step dies with "mkdir /sys/fs/cgroup/<name>: read-only file system".
// Give the container its own cgroup namespace (so the writable mount is the
// container's scoped cgroup subtree, not the host root) and drop "ro" from the
// cgroup mount(s).
func relaxCgroupsForBuild(spec *Spec) {
	if spec.Linux == nil {
		spec.Linux = &Linux{}
	}
	hasCgroupNS := false
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == "cgroup" {
			hasCgroupNS = true
			break
		}
	}
	if !hasCgroupNS {
		spec.Linux.Namespaces = append(spec.Linux.Namespaces, LinuxNamespace{Type: "cgroup"})
	}
	for i := range spec.Mounts {
		if spec.Mounts[i].Destination != "/sys/fs/cgroup" {
			continue
		}
		kept := spec.Mounts[i].Options[:0]
		for _, o := range spec.Mounts[i].Options {
			if o == "ro" {
				continue // a mount with neither ro nor rw defaults to read-write
			}
			kept = append(kept, o)
		}
		spec.Mounts[i].Options = kept
	}
}

// relaxSeccompForBuild removes only the seccomp deny rules that block the
// namespace syscalls a nested builder needs: it drops "unshare" from any deny
// rule (the default profile denies ["ptrace","unshare"] together, so ptrace stays
// denied) and removes the dedicated clone(CLONE_NEWUSER) deny rule. Every other
// rule — notably the kernel-module and kexec denials — is left untouched.
func relaxSeccompForBuild(spec *Spec) {
	if spec.Linux == nil || spec.Linux.Seccomp == nil {
		return
	}
	const cloneNewuser = uint64(0x10000000) // CLONE_NEWUSER
	kept := spec.Linux.Seccomp.Syscalls[:0]
	for _, rule := range spec.Linux.Seccomp.Syscalls {
		// Drop the dedicated clone(CLONE_NEWUSER) deny rule.
		if len(rule.Names) == 1 && rule.Names[0] == "clone" {
			isNewuserRule := false
			for _, a := range rule.Args {
				if a.Value == cloneNewuser {
					isNewuserRule = true
					break
				}
			}
			if isNewuserRule {
				continue
			}
		}
		// Remove "unshare" from this rule's names, keeping the rest (e.g. ptrace).
		names := make([]string, 0, len(rule.Names))
		for _, n := range rule.Names {
			if n == "unshare" {
				continue
			}
			names = append(names, n)
		}
		rule.Names = names
		if len(rule.Names) == 0 {
			continue // the rule only covered unshare
		}
		kept = append(kept, rule)
	}
	spec.Linux.Seccomp.Syscalls = kept
}
