// Package oci provides OCI runtime specification generation for containers.
package oci

// Spec represents an OCI runtime specification.
type Spec struct {
	OCIVersion string   `json:"ociVersion"`
	Process    *Process `json:"process"`
	Root       *Root    `json:"root"`
	Hostname   string   `json:"hostname"`
	Mounts     []Mount  `json:"mounts,omitempty"`
	Linux      *Linux   `json:"linux,omitempty"`
	Hooks      *Hooks   `json:"hooks,omitempty"`
	// Annotations is the standard OCI annotations map. The agent uses it to
	// record which host path each pinned device number came from — see
	// pinnedDevicesAnnotation — so a stale number can be re-resolved later.
	// Runtimes pass unknown annotations through untouched.
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Hooks contains OCI lifecycle hooks.
type Hooks struct {
	Prestart        []Hook `json:"prestart,omitempty"`
	CreateRuntime   []Hook `json:"createRuntime,omitempty"`
	CreateContainer []Hook `json:"createContainer,omitempty"`
	StartContainer  []Hook `json:"startContainer,omitempty"`
	Poststart       []Hook `json:"poststart,omitempty"`
	Poststop        []Hook `json:"poststop,omitempty"`
}

// Hook defines a command to run at a particular lifecycle stage.
type Hook struct {
	Path    string   `json:"path"`
	Args    []string `json:"args,omitempty"`
	Env     []string `json:"env,omitempty"`
	Timeout *int     `json:"timeout,omitempty"`
}

// Process defines the container process configuration.
type Process struct {
	Terminal bool     `json:"terminal,omitempty"`
	User     User     `json:"user"`
	Args     []string `json:"args"`
	Env      []string `json:"env,omitempty"`
	Cwd      string   `json:"cwd"`
	// Capabilities restricts the process capabilities.
	Capabilities    *LinuxCapabilities `json:"capabilities,omitempty"`
	Rlimits         []POSIXRlimit      `json:"rlimits,omitempty"`
	NoNewPrivileges bool               `json:"noNewPrivileges,omitempty"`
}

// User specifies the user identity for the container process.
type User struct {
	UID            uint32   `json:"uid"`
	GID            uint32   `json:"gid"`
	AdditionalGids []uint32 `json:"additionalGids,omitempty"`
}

// Root defines the container's root filesystem.
type Root struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly,omitempty"`
}

// Mount defines a filesystem mount point.
type Mount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"`
	Source      string   `json:"source,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// Linux contains Linux-specific configuration.
type Linux struct {
	Resources     *LinuxResources  `json:"resources,omitempty"`
	Namespaces    []LinuxNamespace `json:"namespaces,omitempty"`
	Devices       []LinuxDevice    `json:"devices,omitempty"`
	CgroupsPath   string           `json:"cgroupsPath,omitempty"`
	MaskedPaths   []string         `json:"maskedPaths,omitempty"`
	ReadonlyPaths []string         `json:"readonlyPaths,omitempty"`
	Seccomp       *LinuxSeccomp    `json:"seccomp,omitempty"`
}

// LinuxSeccomp defines a seccomp filter for the container.
type LinuxSeccomp struct {
	DefaultAction LinuxSeccompAction `json:"defaultAction"`
	Syscalls      []LinuxSyscall     `json:"syscalls,omitempty"`
}

// LinuxSeccompAction is the action taken when a seccomp rule matches.
type LinuxSeccompAction string

const (
	ActAllow LinuxSeccompAction = "SCMP_ACT_ALLOW"
	ActErrno LinuxSeccompAction = "SCMP_ACT_ERRNO"
)

// LinuxSyscall restricts a set of syscalls.
type LinuxSyscall struct {
	Names    []string           `json:"names"`
	Action   LinuxSeccompAction `json:"action"`
	ErrnoRet *uint              `json:"errnoRet,omitempty"`
	Args     []LinuxSeccompArg  `json:"args,omitempty"`
}

// LinuxSeccompArg restricts a syscall based on argument values.
type LinuxSeccompArg struct {
	Index    uint                 `json:"index"`
	Value    uint64               `json:"value"`
	ValueTwo uint64               `json:"valueTwo,omitempty"`
	Op       LinuxSeccompOperator `json:"op"`
}

// LinuxSeccompOperator is the comparison operator used in seccomp argument matching.
type LinuxSeccompOperator string

const (
	OpMaskedEqual LinuxSeccompOperator = "SCMP_CMP_MASKED_EQ"
)

// LinuxResources has container resource constraints.
type LinuxResources struct {
	Devices []LinuxDeviceCgroup `json:"devices,omitempty"`
	Memory  *LinuxMemory        `json:"memory,omitempty"`
	CPU     *LinuxCPU           `json:"cpu,omitempty"`
	Pids    *LinuxPids          `json:"pids,omitempty"`
}

// LinuxPids contains the cgroup pids-controller constraint.
type LinuxPids struct {
	Limit int64 `json:"limit"`
}

// LinuxDeviceCgroup represents a device access rule.
type LinuxDeviceCgroup struct {
	Allow  bool   `json:"allow"`
	Type   string `json:"type,omitempty"`
	Major  *int64 `json:"major,omitempty"`
	Minor  *int64 `json:"minor,omitempty"`
	Access string `json:"access,omitempty"`
}

// LinuxMemory contains memory resource constraints.
type LinuxMemory struct {
	Limit *int64 `json:"limit,omitempty"`
}

// LinuxCPU contains CPU resource constraints.
type LinuxCPU struct {
	Shares *uint64 `json:"shares,omitempty"`
	Quota  *int64  `json:"quota,omitempty"`
	Period *uint64 `json:"period,omitempty"`
	Cpus   string  `json:"cpus,omitempty"`
}

// LinuxNamespace defines a Linux namespace.
type LinuxNamespace struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
}

// LinuxDevice represents a device that should be available in the container.
type LinuxDevice struct {
	Path     string  `json:"path"`
	Type     string  `json:"type"`
	Major    int64   `json:"major"`
	Minor    int64   `json:"minor"`
	FileMode *uint32 `json:"fileMode,omitempty"`
	UID      *uint32 `json:"uid,omitempty"`
	GID      *uint32 `json:"gid,omitempty"`
}

// LinuxCapabilities specifies the capabilities for the container process.
type LinuxCapabilities struct {
	Bounding    []string `json:"bounding,omitempty"`
	Effective   []string `json:"effective,omitempty"`
	Inheritable []string `json:"inheritable,omitempty"`
	Permitted   []string `json:"permitted,omitempty"`
	Ambient     []string `json:"ambient,omitempty"`
}

// POSIXRlimit defines a POSIX resource limit.
type POSIXRlimit struct {
	Type string `json:"type"`
	Hard uint64 `json:"hard"`
	Soft uint64 `json:"soft"`
}

// DedupeDevices removes duplicate device-node entries (same Path) from
// spec.Linux.Devices, keeping the first occurrence. runc mknod()s each device
// entry, so a duplicate path makes container creation fail with EEXIST. Several
// independent provisioners can add the same node — e.g. the NVIDIA CDI spec (or
// the L4T CSV fallback) and the gpu entitlement both add /dev/nvidiactl — so the
// finalized spec is deduped once before it is handed to runc. The cgroup allow
// rules are intentionally left untouched: duplicate allow rules are harmless
// (purely additive) and a whole-major rule is not redundant with a major:minor
// one.
func DedupeDevices(spec *Spec) {
	if spec.Linux == nil || len(spec.Linux.Devices) < 2 {
		return
	}
	seen := make(map[string]bool, len(spec.Linux.Devices))
	deduped := spec.Linux.Devices[:0]
	for _, d := range spec.Linux.Devices {
		if seen[d.Path] {
			continue
		}
		seen[d.Path] = true
		deduped = append(deduped, d)
	}
	spec.Linux.Devices = deduped
}

func DefaultSpec(rootfsPath string, args []string) *Spec {
	return &Spec{
		OCIVersion: "1.0.2",
		Process: &Process{
			User: User{UID: 0, GID: 0},
			Args: args,
			Env: []string{
				"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
				"TERM=xterm",
			},
			Cwd:             "/",
			Capabilities:    defaultCapabilities(),
			NoNewPrivileges: true,
		},
		Root: &Root{
			Path:     rootfsPath,
			Readonly: false,
		},
		Hostname: "wendy",
		Mounts:   defaultMounts(),
		Linux: &Linux{
			Namespaces: []LinuxNamespace{
				{Type: "pid"},
				{Type: "ipc"},
				{Type: "uts"},
				{Type: "mount"},
				{Type: "network"},
			},
			Resources: &LinuxResources{
				Devices: []LinuxDeviceCgroup{
					{Allow: false, Access: "rwm"},
				},
			},
			MaskedPaths: []string{
				"/proc/acpi",
				"/proc/kcore",
				"/proc/keys",
				"/proc/latency_stats",
				"/proc/timer_list",
				"/proc/timer_stats",
				"/proc/sched_debug",
				"/sys/firmware",
			},
			ReadonlyPaths: []string{
				"/proc/asound",
				"/proc/bus",
				"/proc/fs",
				"/proc/irq",
				"/proc/sys",
				"/proc/sysrq-trigger",
			},
			Seccomp: defaultSeccomp(),
		},
	}
}

func defaultCapabilities() *LinuxCapabilities {
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
	return &LinuxCapabilities{
		Bounding:    caps,
		Effective:   caps,
		Inheritable: caps,
		Permitted:   caps,
		Ambient:     caps,
	}
}

func defaultSeccomp() *LinuxSeccomp {
	eperm := uint(1)
	cloneNewuser := uint64(0x10000000) // CLONE_NEWUSER
	return &LinuxSeccomp{
		DefaultAction: ActAllow,
		Syscalls: []LinuxSyscall{
			{
				Names:    []string{"ptrace", "unshare"},
				Action:   ActErrno,
				ErrnoRet: &eperm,
			},
			{
				// SECURITY (WDY-1012): deny kernel-attack-surface syscalls that
				// a normal application container never needs — kernel module
				// loading and kexec. These are pure host-escape primitives;
				// blocking them here is defense-in-depth on top of the capability
				// gating that already withholds CAP_SYS_MODULE / CAP_SYS_BOOT.
				// (create_module is long-removed from Linux but is listed so the
				// filter denies it on any kernel that still exposes it.)
				Names: []string{
					"init_module",
					"finit_module",
					"delete_module",
					"create_module",
					"kexec_load",
					"kexec_file_load",
				},
				Action:   ActErrno,
				ErrnoRet: &eperm,
			},
			{
				Names:  []string{"clone"},
				Action: ActErrno,
				Args: []LinuxSeccompArg{
					{
						Index:    0,
						Value:    cloneNewuser,
						ValueTwo: cloneNewuser,
						Op:       OpMaskedEqual,
					},
				},
				ErrnoRet: &eperm,
			},
		},
	}
}

// DropToMinimalCapabilities strips the process capability set to empty. The ROS 2
// CLI sidecar only execs `ros2` and needs none of the default caps
// (CAP_NET_RAW/MKNOD/SETUID/...), so a network-joined helper should not carry
// them (WDY-1704; least privilege, SOC2-CC6/NIST-AC-6).
func DropToMinimalCapabilities(spec *Spec) {
	empty := []string{}
	spec.Process.Capabilities = &LinuxCapabilities{
		Bounding:    empty,
		Effective:   empty,
		Inheritable: empty,
		Permitted:   empty,
		Ambient:     empty,
	}
}

// InjectHostsMount adds a bind-mount that overlays /etc/hosts with the file at
// hostsPath. Use this for isolated-mode containers so service names resolve.
func InjectHostsMount(spec *Spec, hostsPath string) {
	spec.Mounts = append(spec.Mounts, Mount{
		Destination: "/etc/hosts",
		Type:        "bind",
		Source:      hostsPath,
		Options:     []string{"rbind", "ro"},
	})
}

// InjectResolvMount adds a read-only bind-mount that overlays
// /etc/resolv.conf with the file at resolvPath. Use this for meshed
// containers so hostnames resolve via the mesh DNS server on their bridge
// gateway.
func InjectResolvMount(spec *Spec, resolvPath string) {
	spec.Mounts = append(spec.Mounts, Mount{
		Destination: "/etc/resolv.conf",
		Type:        "bind",
		Source:      resolvPath,
		Options:     []string{"rbind", "ro"},
	})
}

func defaultMounts() []Mount {
	return []Mount{
		{
			Destination: "/proc",
			Type:        "proc",
			Source:      "proc",
			Options:     []string{"nosuid", "noexec", "nodev"},
		},
		{
			Destination: "/dev",
			Type:        "tmpfs",
			Source:      "tmpfs",
			Options:     []string{"nosuid", "strictatime", "mode=755", "size=65536k"},
		},
		{
			Destination: "/dev/pts",
			Type:        "devpts",
			Source:      "devpts",
			Options:     []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"},
		},
		{
			Destination: "/dev/shm",
			Type:        "tmpfs",
			Source:      "shm",
			Options:     []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"},
		},
		{
			Destination: "/dev/mqueue",
			Type:        "mqueue",
			Source:      "mqueue",
			Options:     []string{"nosuid", "noexec", "nodev"},
		},
		{
			Destination: "/sys",
			Type:        "sysfs",
			Source:      "sysfs",
			Options:     []string{"nosuid", "noexec", "nodev", "ro"},
		},
		{
			Destination: "/sys/fs/cgroup",
			Type:        "cgroup",
			Source:      "cgroup",
			Options:     []string{"nosuid", "noexec", "nodev", "relatime", "ro"},
		},
	}
}
