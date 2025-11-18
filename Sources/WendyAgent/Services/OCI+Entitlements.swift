import AppConfig
import Foundation
import Logging

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
        let logger = Logger(label: "OCI+Entitlements")
        logger.info(
            "applyEntitlements called",
            metadata: [
                "entitlements_count": .stringConvertible(entitlements.count),
                "entitlements": .string("\(entitlements)"),
            ]
        )
        var didSetDeviceCapabilities = false

        for entitlement in entitlements {
            logger.info(
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
                    self.linux.networkMode = "host"
                case .none:
                    self.linux.networkMode = "none"
                    self.linux.namespaces.append(.init(type: "network"))
                }
            case .bluetooth(let bluetooth):
                switch bluetooth.mode {
                case .bluez:
                    ()  // TODO: Unsupported for now
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
            case .video:
                self.linux.devices.append(
                    .init(
                        path: "/dev/video0",
                        type: "c",
                        major: 81,
                        minor: 17,
                        fileMode: 0o666,
                        uid: 0,
                        gid: 0
                    )
                )

                self.mounts.append(
                    .init(
                        destination: "/dev/video0",
                        type: "bind",
                        source: "/dev/video0",
                        options: ["rbind", "nosuid", "noexec"]
                    )
                )

                self.linux.resources?.devices?.append(
                    DeviceAllowance(allow: true, type: "c", major: 81, minor: 17, access: "rw")
                )

                if !didSetDeviceCapabilities {
                    didSetDeviceCapabilities = true
                    self.setDeviceCapabilities(appName: appName)
                }
            }
        }
    }
}
