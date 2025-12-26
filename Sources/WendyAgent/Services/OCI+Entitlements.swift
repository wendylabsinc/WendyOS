import AppConfig
import Foundation
import Logging

#if canImport(Darwin)
    import Darwin
#elseif canImport(Glibc)
    import Glibc
#endif

extension OCI {
    mutating func setDeviceCapabilities(appName: String) {
        let deviceCapabilities = [
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
            "CAP_SYS_CHROOT",
            "CAP_KILL",
            "CAP_AUDIT_WRITE",
            "CAP_SYS_PTRACE",
        ]
        self.linux.capabilities.bounding.formUnion(deviceCapabilities)
        self.linux.capabilities.effective.formUnion(deviceCapabilities)
        self.linux.capabilities.inheritable.formUnion(deviceCapabilities)
        self.linux.capabilities.permitted.formUnion(deviceCapabilities)

        self.mounts.append(
            .init(
                destination: "/sys/fs/cgroup",
                type: "cgroup",
                source: "cgroup",
                options: ["ro", "nosuid", "noexec", "nodev"]
            )
        )

        if self.linux.resources == nil {
            self.linux.resources = Resources()
        }

        if self.linux.resources?.devices == nil {
            self.linux.resources?.devices = []
        }

        // Configure cgroup path and mode for device controller delegation
        let path = appName.replacingOccurrences(of: "-", with: "_")
        self.linux.cgroupsPath = "system.slice:edge-agent:\(path)"
        self.linux.namespaces.append(.init(type: "cgroup"))

        // Apply resources to container, these are applies in order
        // Default deny all devices
        self.linux.resources?.devices?.append(
            DeviceAllowance(allow: true, access: "rwm")  // Default deny all
        )
    }

    mutating func applyEntitlements(
        entitlements: [Entitlement],
        appName: String
    ) {
        let logger = Logger(label: #file)
        logger.debug(
            "applyEntitlements called",
            metadata: [
                "entitlements_count": .stringConvertible(entitlements.count),
                "entitlements": .string("\(entitlements)"),
            ]
        )
        var didSetDeviceCapabilities = false

        for entitlement in entitlements {
            logger.trace(
                "Processing entitlement",
                metadata: ["entitlement": .string("\(entitlement)")]
            )
            switch entitlement {
            case .gpu(_):
                logger.info("GPU entitlement detected - adding video group")
                // Add video group (gid 44) for access to GPU devices
                // GPU devices on Jetson are owned by group 'video' (gid 44)
                if !self.process.user.additionalGids.contains(44) {
                    self.process.user.additionalGids.append(44)
                    logger.debug(
                        "Added video group to additionalGids",
                        metadata: [
                            "additionalGids": .stringConvertible(self.process.user.additionalGids)
                        ]
                    )
                }
            case .network(let entitlement):
                switch entitlement.mode {
                case .host:
                    // Remove any network namespace to ensure host networking
                    self.linux.namespaces.removeAll(where: { $0.type == "network" })

                    // Mount systemd-resolved's actual resolv.conf for DNS resolution
                    // We use /run/systemd/resolve/resolv.conf instead of /etc/resolv.conf
                    // because /etc/resolv.conf often points to 127.0.0.53 (systemd-resolved stub)
                    // which doesn't work in containers. The /run path contains the real upstream DNS servers.
                    // See: https://github.com/moby/moby/blob/master/libnetwork/resolvconf/resolvconf.go#L16
                    if !self.mounts.contains(where: { $0.destination == "/etc/resolv.conf" }) {
                        self.mounts.append(
                            .init(
                                destination: "/etc/resolv.conf",
                                type: "bind",
                                source: "/run/systemd/resolve/resolv.conf",
                                options: ["rbind", "ro"]
                            )
                        )
                        logger.debug(
                            "Added /run/systemd/resolve/resolv.conf bind mount for host networking DNS"
                        )
                    }

                // Note: We do NOT mount /etc/hosts because:
                // 1. Containerd manages the container's own /etc/hosts file with container-specific entries
                // 2. The container needs its own IP/hostname mappings, not the host's
                // 3. Mounting host's /etc/hosts would leak host-internal names and break container identity
                // If custom host entries are needed, they should be added via the OCI spec's /etc/hosts
                // generation or via container runtime mechanisms, not by mounting the host's file.

                case .none:
                    self.linux.namespaces.append(.init(type: "network"))
                }
            case .bluetooth(let bluetooth):
                switch bluetooth.mode {
                case .bluez:
                    // Mount D-Bus for BlueZ daemon communication
                    self.mounts.append(
                        .init(
                            destination: "/run/dbus",
                            type: "bind",
                            source: "/run/dbus",
                            options: ["rbind", "nosuid", "noexec"]
                        )
                    )

                    // Also mount /var/run/dbus as some systems use this path
                    self.mounts.append(
                        .init(
                            destination: "/var/run/dbus",
                            type: "bind",
                            source: "/var/run/dbus",
                            options: ["rbind", "nosuid", "noexec"]
                        )
                    )
                case .kernel:
                    for entitlement in entitlements {
                        if case .network(let networkEntitlements) = entitlement,
                            networkEntitlements.mode == .none
                        {
                            // TODO: Throw error
                        }
                    }

                    // These already exist
                    //                    self.linux.namespaces.append(.init(type: "pid"))
                    //                    self.linux.namespaces.append(.init(type: "ipc"))
                    //                    self.linux.namespaces.append(.init(type: "uts"))

                    let deviceCapabilities = [
                        "CAP_NET_ADMIN",
                        "CAP_NET_RAW",
                    ]
                    self.linux.capabilities.bounding.formUnion(deviceCapabilities)
                    self.linux.capabilities.effective.formUnion(deviceCapabilities)
                    self.linux.capabilities.inheritable.formUnion(deviceCapabilities)
                    self.linux.capabilities.permitted.formUnion(deviceCapabilities)

                    self.linux.seccomp = .init(
                        defaultAction: "SCMP_ACT_ERRNO",
                        architectures: [
                            "SCMP_ARCH_X86_64", "SCMP_ARCH_AARCH64", "SCMP_ARCH_X86",
                            "SCMP_ARCH_ARM",
                        ],
                        syscalls: [
                            Syscall(
                                names: ["socket"],
                                action: "SCMP_ACT_ALLOW",
                                args: [
                                    .init(
                                        index: 0,
                                        value: 31,  // AF_BLUETOOTH
                                        valueTwo: nil,
                                        op: .EQ
                                    )
                                ]
                            ),
                            Syscall(
                                names: ["socket"],
                                action: "SCMP_ACT_ALLOW",
                                args: [
                                    .init(
                                        index: 0,
                                        value: 16,  // AF_NETLINK
                                        valueTwo: nil,
                                        op: .EQ
                                    )
                                ]
                            ),
                            Syscall(
                                names: [
                                    "bind", "connect", "getsockopt", "setsockopt", "ioctl",
                                    "sendmsg", "recvmsg", "sendto", "recvfrom",
                                ],
                                action: "SCMP_ACT_ALLOW"
                            ),
                            Syscall(
                                names: [
                                    "poll", "ppoll", "epoll_create1", "epoll_ctl", "epoll_wait",
                                ],
                                action: "SCMP_ACT_ALLOW"
                            ),
                            Syscall(
                                names: [
                                    "read", "write", "close", "futex", "nanosleep", "clock_gettime",
                                    "getrandom", "eventfd2", "timerfd_create", "timerfd_settime",
                                    "signalfd4", "mmap", "mprotect", "munmap",
                                ],
                                action: "SCMP_ACT_ALLOW"
                            ),
                        ]
                    )
                }
            case .audio:
                if !didSetDeviceCapabilities {
                    didSetDeviceCapabilities = true
                    self.setDeviceCapabilities(appName: appName)
                }

                // Bind mount the entire /dev/snd directory
                appendMountIfMissing(
                    .init(
                        destination: "/dev/snd",
                        type: "bind",
                        source: "/dev/snd",
                        options: ["rbind", "nosuid", "noexec"]
                    )
                )

                // Add device allowance for ALSA sound devices (major 116)
                if self.linux.resources == nil {
                    self.linux.resources = Resources()
                }
                if self.linux.resources?.devices == nil {
                    self.linux.resources?.devices = []
                }

                // ALSA sound devices are major 116 on Linux.
                self.linux.resources?.devices?.append(
                    DeviceAllowance(allow: true, type: "c", major: 116, access: "rw")
                )

                // Prefer PipeWire/Pulse runtime sockets when present so apps can use host audio services.
                if let runtime = resolveAudioRuntime(logger: logger) {
                    appendMountIfMissing(
                        .init(
                            destination: runtime.path,
                            type: "bind",
                            source: runtime.path,
                            options: ["rbind", "nosuid", "noexec"]
                        )
                    )

                    // Point clients at the host audio runtime directory.
                    setEnv("XDG_RUNTIME_DIR", value: runtime.path)

                    if runtime.hasPipewire {
                        // PipeWire default socket name.
                        setEnv("PIPEWIRE_REMOTE", value: "pipewire-0")
                    }

                    if runtime.hasPulse {
                        // PulseAudio compatible socket for libpulse clients.
                        setEnv(
                            "PULSE_SERVER",
                            value: "unix:\(runtime.path)/pulse/native"
                        )
                    }
                }
            case .video(let videoEntitlement):
                if !didSetDeviceCapabilities {
                    didSetDeviceCapabilities = true
                    self.setDeviceCapabilities(appName: appName)
                }

                // Select V4L2 device paths based on the entitlement mode.
                let devicePaths: [String]
                switch videoEntitlement.mode {
                case .all:
                    devicePaths = listV4L2DevicePaths(logger: logger)
                case .whitelist:
                    devicePaths = videoEntitlement.devices.map(normalizeDevicePath)
                }

                let uniqueDevicePaths = Array(Set(devicePaths)).sorted()

                // Grant major 81 for V4L2 when in all mode to avoid per-node cgroup churn.
                if videoEntitlement.mode == .all {
                    if self.linux.resources == nil {
                        self.linux.resources = Resources()
                    }
                    if self.linux.resources?.devices == nil {
                        self.linux.resources?.devices = []
                    }

                    self.linux.resources?.devices?.append(
                        DeviceAllowance(allow: true, type: "c", major: 81, access: "rwm")
                    )
                }

                for devicePath in uniqueDevicePaths {
                    guard FileManager.default.fileExists(atPath: devicePath) else {
                        logger.warning(
                            "Video device not found, skipping",
                            metadata: ["path": .string(devicePath)]
                        )
                        continue
                    }

                    appendMountIfMissing(
                        .init(
                            destination: devicePath,
                            type: "bind",
                            source: devicePath,
                            options: ["rbind", "nosuid", "noexec"]
                        )
                    )

                    guard let deviceNumbers = deviceNumbers(for: devicePath) else {
                        logger.warning(
                            "Failed to read video device numbers",
                            metadata: ["path": .string(devicePath)]
                        )
                        continue
                    }

                    self.linux.devices.append(
                        .init(
                            path: devicePath,
                            type: "c",
                            major: deviceNumbers.major,
                            minor: deviceNumbers.minor,
                            fileMode: 0o666,
                            uid: 0,
                            gid: 0
                        )
                    )

                    if videoEntitlement.mode == .whitelist {
                        if self.linux.resources == nil {
                            self.linux.resources = Resources()
                        }
                        if self.linux.resources?.devices == nil {
                            self.linux.resources?.devices = []
                        }

                        self.linux.resources?.devices?.append(
                            DeviceAllowance(
                                allow: true,
                                type: "c",
                                major: deviceNumbers.major,
                                minor: deviceNumbers.minor,
                                access: "rw"
                            )
                        )
                    }
                }
            }
        }
    }

    private struct AudioRuntime {
        let path: String
        let hasPipewire: Bool
        let hasPulse: Bool
    }

    // Locate a host runtime dir with PipeWire/Pulse sockets for client discovery.
    private func resolveAudioRuntime(logger: Logger) -> AudioRuntime? {
        let candidates = [
            "/run/pipewire",
            "/run/user/1000",
        ]

        for candidate in candidates {
            let pipewireSocket = "\(candidate)/pipewire-0"
            let pulseSocket = "\(candidate)/pulse/native"
            let hasPipewire = FileManager.default.fileExists(atPath: pipewireSocket)
            let hasPulse = FileManager.default.fileExists(atPath: pulseSocket)

            if hasPipewire || hasPulse {
                return AudioRuntime(
                    path: candidate,
                    hasPipewire: hasPipewire,
                    hasPulse: hasPulse
                )
            }
        }

        logger.debug("No PipeWire or PulseAudio runtime socket found")
        return nil
    }

    // Enumerate V4L2-related device nodes without binding all of /dev.
    private func listV4L2DevicePaths(logger: Logger) -> [String] {
        do {
            let entries = try FileManager.default.contentsOfDirectory(atPath: "/dev")
            return entries.filter { entry in
                entry.hasPrefix("video")
                    || entry.hasPrefix("media")
                    || entry.hasPrefix("v4l-subdev")
            }.map { "/dev/\($0)" }.sorted()
        } catch {
            logger.warning(
                "Failed to enumerate V4L2 devices",
                metadata: ["error": .string(error.localizedDescription)]
            )
            return []
        }
    }

    private func normalizeDevicePath(_ path: String) -> String {
        guard !path.hasPrefix("/") else { return path }
        return "/dev/\(path)"
    }

    private mutating func appendMountIfMissing(_ mount: Mount) {
        guard !self.mounts.contains(where: { $0.destination == mount.destination }) else {
            return
        }
        self.mounts.append(mount)
    }

    private mutating func setEnv(_ key: String, value: String) {
        let prefix = "\(key)="
        if let index = self.process.env.firstIndex(where: { $0.hasPrefix(prefix) }) {
            self.process.env[index] = "\(prefix)\(value)"
        } else {
            self.process.env.append("\(prefix)\(value)")
        }
    }

    // Resolve major/minor from a device node for OCI device specs and cgroup rules.
    private func deviceNumbers(for path: String) -> (major: Int, minor: Int)? {
        var st = stat()
        guard stat(path, &st) == 0 else {
            return nil
        }

        #if os(Linux)
            let major = Int((st.st_rdev >> 8) & 0xfff)
            let minor = Int((st.st_rdev & 0xff) | ((st.st_rdev >> 12) & 0xfff00))
        #else
            let major = Int((st.st_rdev >> 8) & 0xFF)
            let minor = Int(st.st_rdev & 0xFF)
        #endif

        return (major: major, minor: minor)
    }
}
