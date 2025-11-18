import Foundation
import Logging

#if canImport(Darwin)
    import Darwin
#elseif canImport(Glibc)
    import Glibc
#endif

extension OCI {
    /// Get device major/minor numbers from a device file path
    private func getDeviceNumbers(_ path: String) throws -> (major: Int64, minor: Int64) {
        var st = stat()
        guard stat(path, &st) == 0 else {
            throw CDIError.failedToStatDevice(path)
        }

        // Extract major/minor from st_rdev using Linux macros
        // Linux uses: major = (dev >> 8) & 0xfff, minor = (dev & 0xff) | ((dev >> 12) & 0xfff00)
        #if os(Linux)
            // Use Linux device number encoding
            let major = Int64((st.st_rdev >> 8) & 0xfff)
            let minor = Int64((st.st_rdev & 0xff) | ((st.st_rdev >> 12) & 0xfff00))
        #else
            // Use traditional Unix layout for macOS
            let major = Int64((st.st_rdev >> 8) & 0xFF)
            let minor = Int64(st.st_rdev & 0xFF)
        #endif

        return (major, minor)
    }

    /// Apply a CDI device specification to this OCI spec
    /// This adds device nodes, mounts, environment variables, and hooks from the CDI spec
    mutating func applyCDIDevice(_ cdiSpec: CDISpecification, deviceName: String) throws {
        let logger = Logger(label: "OCI+CDI")

        // Find the device in the CDI spec
        guard let device = cdiSpec.devices.first(where: { $0.name == deviceName }) else {
            throw CDIError.deviceNotFound(deviceName)
        }

        logger.debug(
            "Applying CDI device to OCI spec",
            metadata: [
                "device": .string(deviceName),
                "kind": .string(cdiSpec.kind),
            ]
        )

        let edits = device.containerEdits

        // Apply global container edits if present
        if let globalEdits = cdiSpec.containerEdits {
            applyContainerEdits(globalEdits, logger: logger)
        }

        // Apply device-specific container edits
        applyContainerEdits(edits, logger: logger)

        logger.info(
            "Successfully applied CDI device",
            metadata: [
                "device": .string(deviceName),
                "kind": .string(cdiSpec.kind),
            ]
        )
    }

    private mutating func applyContainerEdits(_ edits: CDIContainerEdits, logger: Logger) {
        // 1. Add device nodes
        if let deviceNodes = edits.deviceNodes {
            for node in deviceNodes {
                // If major/minor not specified in CDI spec, look them up from host device
                let (major, minor): (Int, Int)
                if let nodeMajor = node.major, let nodeMinor = node.minor {
                    major = nodeMajor
                    minor = nodeMinor
                } else {
                    // Use hostPath if available, otherwise use path
                    let devicePath = node.hostPath ?? node.path
                    do {
                        let (maj64, min64) = try getDeviceNumbers(devicePath)
                        major = Int(maj64)
                        minor = Int(min64)
                        logger.debug(
                            "Looked up device numbers",
                            metadata: [
                                "path": .string(devicePath),
                                "major": .stringConvertible(major),
                                "minor": .stringConvertible(minor),
                            ]
                        )
                    } catch {
                        logger.warning(
                            "Failed to stat device, using 0:0",
                            metadata: [
                                "path": .string(devicePath),
                                "error": .string(error.localizedDescription),
                            ]
                        )
                        major = 0
                        minor = 0
                    }
                }

                // Convert CDI device node to OCI device
                let ociDevice = Device(
                    path: node.path,
                    type: node.type ?? "c",  // default to character device
                    major: major,
                    minor: minor,
                    fileMode: node.fileMode,
                    uid: 0,
                    gid: 0
                )
                self.linux.devices.append(ociDevice)

                // Add cgroup device allowance for this device
                if self.linux.resources == nil {
                    self.linux.resources = Resources(devices: [])
                }
                if self.linux.resources?.devices == nil {
                    self.linux.resources?.devices = []
                }

                let deviceAllowance = DeviceAllowance(
                    allow: true,
                    type: node.type ?? "c",
                    major: major,
                    minor: minor,
                    access: "rwm"  // read, write, mknod
                )
                self.linux.resources?.devices?.append(deviceAllowance)

                logger.trace(
                    "Added device node and cgroup allowance",
                    metadata: [
                        "path": .string(node.path),
                        "major": .stringConvertible(major),
                        "minor": .stringConvertible(minor),
                    ]
                )
            }
        }

        // 2. Add mounts
        if let mounts = edits.mounts {
            for mount in mounts {
                let ociMount = Mount(
                    destination: mount.containerPath,
                    type: mount.type ?? "bind",
                    source: mount.hostPath,
                    options: mount.options ?? ["rbind", "nosuid", "nodev", "ro"]
                )
                self.mounts.append(ociMount)

                logger.trace(
                    "Added mount",
                    metadata: [
                        "containerPath": .string(mount.containerPath),
                        "hostPath": .string(mount.hostPath),
                    ]
                )
            }
        }

        // 3. Add environment variables
        if let envVars = edits.env {
            self.process.env.append(contentsOf: envVars)

            logger.trace(
                "Added environment variables",
                metadata: [
                    "count": .stringConvertible(envVars.count)
                ]
            )
        }

        // 4. Add hooks
        if let cdiHooks = edits.hooks {
            for cdiHook in cdiHooks {
                let ociHook = Hook(
                    path: cdiHook.path,
                    args: cdiHook.args,
                    env: cdiHook.env,
                    timeout: cdiHook.timeout
                )

                // Initialize hooks struct if needed
                if self.hooks == nil {
                    self.hooks = Hooks()
                }

                // Add hook to the appropriate lifecycle stage
                switch cdiHook.hookName.lowercased() {
                case "prestart":
                    if self.hooks?.prestart == nil {
                        self.hooks?.prestart = []
                    }
                    self.hooks?.prestart?.append(ociHook)
                case "createruntime":
                    if self.hooks?.createRuntime == nil {
                        self.hooks?.createRuntime = []
                    }
                    self.hooks?.createRuntime?.append(ociHook)
                case "createcontainer":
                    if self.hooks?.createContainer == nil {
                        self.hooks?.createContainer = []
                    }
                    self.hooks?.createContainer?.append(ociHook)
                case "startcontainer":
                    if self.hooks?.startContainer == nil {
                        self.hooks?.startContainer = []
                    }
                    self.hooks?.startContainer?.append(ociHook)
                case "poststart":
                    if self.hooks?.poststart == nil {
                        self.hooks?.poststart = []
                    }
                    self.hooks?.poststart?.append(ociHook)
                case "poststop":
                    if self.hooks?.poststop == nil {
                        self.hooks?.poststop = []
                    }
                    self.hooks?.poststop?.append(ociHook)
                default:
                    logger.warning(
                        "Unknown hook name, skipping",
                        metadata: [
                            "hookName": .string(cdiHook.hookName)
                        ]
                    )
                }

                logger.trace(
                    "Added hook",
                    metadata: [
                        "hookName": .string(cdiHook.hookName),
                        "path": .string(cdiHook.path),
                    ]
                )
            }
        }
    }
}
