import Foundation
import Logging

extension OCI {
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
                "kind": .string(cdiSpec.kind)
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
                "kind": .string(cdiSpec.kind)
            ]
        )
    }

    private mutating func applyContainerEdits(_ edits: CDIContainerEdits, logger: Logger) {
        // 1. Add device nodes
        if let deviceNodes = edits.deviceNodes {
            for node in deviceNodes {
                // Convert CDI device node to OCI device
                let ociDevice = Device(
                    path: node.path,
                    type: node.type ?? "c", // default to character device
                    major: node.major ?? 0,
                    minor: node.minor ?? 0,
                    fileMode: node.fileMode,
                    uid: 0,
                    gid: 0
                )
                self.linux.devices.append(ociDevice)

                logger.trace(
                    "Added device node",
                    metadata: [
                        "path": .string(node.path)
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
                        "hostPath": .string(mount.hostPath)
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
                        "path": .string(cdiHook.path)
                    ]
                )
            }
        }
    }
}
