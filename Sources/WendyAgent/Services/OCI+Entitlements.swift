import AppConfig
import Foundation
import Logging

struct OCIDependencies: Sendable {
    var directoriesToCreate = [URL]()
}

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

    struct AvailableDevices: Sendable {
        let devices: [Device]

        static func detect() throws -> AvailableDevices {
            let fm = FileManager.default
            let devContents = try fm.contentsOfDirectory(atPath: "/dev")
            return AvailableDevices(
                devices: devContents.compactMap { device -> Device? in
                    let devicePath = "/dev/\(device)"

                    // Get device info using stat to find major/minor numbers
                    var statInfo = stat()
                    guard stat(devicePath, &statInfo) == 0 else { return nil }

                    // Extract major/minor numbers from st_rdev
                    // On Linux: major = (rdev >> 8) & 0xfff, minor = (rdev & 0xff) | ((rdev >> 12) & ~0xff)
                    let rdev = UInt64(statInfo.st_rdev)
                    let deviceMajor = Int((rdev >> 8) & 0xfff)
                    let deviceMinor = Int((rdev & 0xff) | ((rdev >> 12) & ~0xff))

                    return Device(
                        path: devicePath,
                        type: "c",
                        major: deviceMajor,
                        minor: deviceMinor,
                        fileMode: 0o666,
                        uid: 0,
                        gid: 0
                    )
                }
            )
        }
    }

    @discardableResult
    mutating func applyEntitlements(
        entitlements: [Entitlement],
        appName: String,
        availableDevices: AvailableDevices,
        persistenceBasePath: URL
    ) -> OCIDependencies {
        let logger = Logger(label: #file)
        logger.debug(
            "applyEntitlements called",
            metadata: [
                "entitlements_count": .stringConvertible(entitlements.count),
                "entitlements": .string("\(entitlements)"),
            ]
        )
        var didSetDeviceCapabilities = false
        var dependencies = OCIDependencies()

        for entitlement in entitlements {
            logger.trace(
                "Processing entitlement",
                metadata: ["entitlement": .string("\(entitlement)")]
            )
            switch entitlement {
            case .persist(let persistenceEntitlements):
                let source = persistenceBasePath.appending(component: persistenceEntitlements.name)
                dependencies.directoriesToCreate.append(source)

                self.mounts.append(
                    .init(
                        destination: persistenceEntitlements.path,
                        type: "bind",
                        source: source.path,
                        options: ["rbind", "nosuid", "noexec"]
                    )
                )
                logger.debug(
                    "Added persist mount",
                    metadata: [
                        "mount": .string("\(persistenceEntitlements.path)")
                    ]
                )
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
                logger.info("Audio entitlement detected - adding audio group")
                // Add audio group (gid 29) for access to ALSA devices
                // Audio devices on Linux are owned by group 'audio' (gid 29)
                if !self.process.user.additionalGids.contains(29) {
                    self.process.user.additionalGids.append(29)
                    logger.debug(
                        "Added audio group to additionalGids",
                        metadata: [
                            "additionalGids": .stringConvertible(self.process.user.additionalGids)
                        ]
                    )
                }

                // Bind mount the entire /dev/snd directory
                self.mounts.append(
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

                self.linux.resources?.devices?.append(
                    DeviceAllowance(allow: true, type: "c", major: 116, access: "rw")
                )

                if !didSetDeviceCapabilities {
                    didSetDeviceCapabilities = true
                    self.setDeviceCapabilities(appName: appName)
                }
            case .video(let video):
                // Find all video devices in /dev
                let videoDevices = availableDevices.devices.filter { device in
                    guard device.path.hasPrefix("/dev/video") else {
                        return false
                    }

                    switch video.mode {
                    case .all:
                        return true
                    case .allowlist:
                        return video.allowlist.contains { allowed in
                            let allowedName = allowed.replacingOccurrences(of: "/dev/", with: "")
                            let deviceName = device.path.replacingOccurrences(of: "/dev/", with: "")
                            return deviceName == allowedName
                        }
                    }
                }

                for device in videoDevices {
                    self.linux.devices.append(device)
                    self.mounts.append(
                        .init(
                            destination: device.path,
                            type: "bind",
                            source: device.path,
                            options: ["rbind", "nosuid", "noexec"]
                        )
                    )
                }

                if !videoDevices.isEmpty {
                    // Allow all video4linux devices (major 81)
                    self.linux.resources?.devices?.append(
                        DeviceAllowance(allow: true, type: "c", major: 81, access: "rw")
                    )

                    if !didSetDeviceCapabilities {
                        didSetDeviceCapabilities = true
                        self.setDeviceCapabilities(appName: appName)
                    }
                } else {
                    logger.warning(
                        "Video entitlement requested but no /dev/video* devices found"
                    )
                }
            }
        }

        return dependencies
    }
}
