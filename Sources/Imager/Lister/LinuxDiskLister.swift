import Foundation
import Logging
import Subprocess

/// Linux implementation of the DiskLister protocol.
public struct LinuxDiskLister: DiskLister {
    let logger = Logger(label: "LinuxDiskLister")
    // MARK: - Codable Structs for lsblk JSON Output

    /// Represents the root JSON structure returned by lsblk -J
    private struct LsblkOutput: Codable {
        let blockdevices: [BlockDevice]
    }

    /// Represents a block device in the lsblk JSON output
    private struct BlockDevice: Codable {
        let name: String
        let model: String?
        let size: Int64?  // Size in bytes as a number
        let hotplug: Bool?  // Boolean indicating if device is hot-pluggable
        let type: String?  // "disk", "part", "loop", "rom", etc.
        let fsavail: String?  // Available filesystem space in bytes (as string or null)
        let fsused: String?  // Used filesystem space in bytes (as string or null)
        let fssize: String?  // Total filesystem size in bytes (as string or null)
        let mountpoint: String?  // Mount point of the filesystem
        let children: [BlockDevice]?

        // Computed properties for convenience
        var isExternal: Bool {
            return hotplug ?? false
        }

        var isDisk: Bool {
            // Only consider actual disks and ROM drives (CD/DVD)
            return type == "disk" || type == "rom"
        }

        var displayName: String {
            if let model = model, !model.isEmpty {
                return model
            }
            // Provide better default names based on device type
            switch type {
            case "rom":
                return "Optical Drive (\(name))"
            case "disk":
                return "Disk (\(name))"
            default:
                return "Storage Device (\(name))"
            }
        }

        var sizeInBytes: Int64 {
            return size ?? 0
        }

        var availableBytes: Int64 {
            // Try to get available space from filesystem info
            if let fsavail = fsavail, let availBytes = Int64(fsavail) {
                return availBytes
            }

            // Check children partitions for available space (sum of all partitions)
            if let children = children {
                let totalAvailable = children.reduce(Int64(0)) { sum, child in
                    if let fsavail = child.fsavail, let availBytes = Int64(fsavail) {
                        return sum + availBytes
                    }
                    return sum
                }
                if totalAvailable > 0 {
                    return totalAvailable
                }
            }

            // If unmounted and no filesystem info, assume entire disk is available
            if mountpoint == nil || mountpoint?.isEmpty == true {
                return sizeInBytes
            }

            // Default to 0 if we can't determine
            return 0
        }
    }

    public init() {}

    /// Lists available drives on Linux.
    /// - Parameter all: If true, lists all drives, not just external drives.
    /// - Returns: An array of Drive objects representing the available drives.
    public func list(all: Bool = false) async throws -> [Drive] {
        do {
            // Use lsblk to get information about all block devices in JSON format
            // Include filesystem information for available space calculation
            let result = try await Subprocess.run(
                Subprocess.Executable.name("lsblk"),
                arguments: [
                    "-J", "-b", "-o",
                    "NAME,SIZE,MODEL,HOTPLUG,TYPE,FSAVAIL,FSUSED,FSSIZE,MOUNTPOINT",
                ],
                output: .string(limit: .max),
                error: .string(limit: .max)
            )

            if result.terminationStatus.isSuccess, let output = result.standardOutput {
                return parseLsblkOutput(output, all: all)
            } else {
                let errorOutput = result.standardError ?? "Unknown error"
                throw DiskListerError.listFailed(error: errorOutput)
            }
        } catch {
            throw error
        }
    }

    /// Finds a drive by its identifier.
    /// - Parameter id: The identifier of the drive to find.
    /// - Returns: A Drive object representing the found drive.
    /// - Throws: An error if the drive is not found.
    public func findDrive(byId id: String) async throws -> Drive {
        // Validate input to prevent injection or invalid paths
        guard !id.isEmpty else {
            throw DiskListerError.driveNotFound(id: id, error: "Disk ID must not be empty")
        }

        // Normalize the id to remove /dev/ prefix if present
        let deviceId = id.hasPrefix("/dev/") ? String(id.dropFirst(5)) : id

        // Further validate the device ID - it should be alphanumeric with possible numbers
        let validPattern = "^[a-zA-Z]+[a-zA-Z0-9]*$"
        guard deviceId.range(of: validPattern, options: .regularExpression) != nil else {
            throw DiskListerError.driveNotFound(id: id, error: "Invalid drive ID format")
        }

        let devicePath = "/dev/\(deviceId)"

        // First, check if the device exists
        if !FileManager.default.fileExists(atPath: devicePath) {
            throw DiskListerError.driveNotFound(
                id: id,
                error: "Device does not exist at \(devicePath)"
            )
        }

        do {
            let result = try await Subprocess.run(
                Subprocess.Executable.name("lsblk"),
                arguments: [
                    "-J", "-b", "-o",
                    "NAME,SIZE,MODEL,HOTPLUG,TYPE,FSAVAIL,FSUSED,FSSIZE,MOUNTPOINT", devicePath,
                ],
                output: .string(limit: .max),
                error: .string(limit: .max)
            )

            if result.terminationStatus.isSuccess, let output = result.standardOutput {
                let drives = parseLsblkOutput(output, all: true)

                if let drive = drives.first {
                    // Ensure the drive ID is correctly formatted
                    if !drive.id.hasPrefix("/dev/") {
                        // Fix the drive ID if needed
                        var correctedDrive = drive
                        correctedDrive.id = devicePath
                        return correctedDrive
                    }
                    return drive
                } else {
                    throw DiskListerError.driveNotFound(
                        id: id,
                        error: "Drive not found or is not a valid disk"
                    )
                }
            } else {
                let errorOutput = result.standardError ?? "Unknown error"
                throw DiskListerError.listFailed(error: errorOutput)
            }
        } catch {
            throw error
        }
    }

    // MARK: - Private Methods

    /// Parses the JSON output from lsblk.
    /// - Parameters:
    ///   - output: The JSON output from lsblk.
    ///   - all: If true, includes all drives, not just external ones.
    /// - Returns: An array of Drive objects.
    private func parseLsblkOutput(_ output: String, all: Bool) -> [Drive] {
        var drives: [Drive] = []

        // Try to parse the JSON output from lsblk -J
        guard let jsonData = output.data(using: .utf8) else {
            logger.error("Could not convert lsblk output to data")
            return []
        }

        do {
            // Use JSONDecoder to parse the JSON output
            let decoder = JSONDecoder()
            let lsblkOutput = try decoder.decode(LsblkOutput.self, from: jsonData)

            for device in lsblkOutput.blockdevices {
                // Only include actual disk devices (not partitions, loops, etc.)
                if device.isDisk {
                    // Only include external devices unless all is true
                    if all || device.isExternal {
                        let id = "/dev/\(device.name)"

                        let drive = Drive(
                            id: id,
                            name: device.displayName,
                            available: device.availableBytes,
                            capacity: device.sizeInBytes,
                            isExternal: device.isExternal
                        )

                        drives.append(drive)
                    }
                }
            }
        } catch let decodingError as DecodingError {
            switch decodingError {
            case .keyNotFound(let key, let context):
                logger.error(
                    "JSON parsing error - Missing key: \(key.stringValue)",
                    metadata: ["context": "\(context.debugDescription)"]
                )
            case .typeMismatch(let type, let context):
                logger.error(
                    "JSON parsing error - Type mismatch for type: \(type)",
                    metadata: ["context": "\(context.debugDescription)"]
                )
            case .valueNotFound(let type, let context):
                logger.error(
                    "JSON parsing error - Value not found for type: \(type)",
                    metadata: ["context": "\(context.debugDescription)"]
                )
            case .dataCorrupted(let context):
                logger.error(
                    "JSON parsing error - Data corrupted",
                    metadata: ["context": "\(context.debugDescription)"]
                )
            @unknown default:
                logger.error("JSON parsing error - Unknown decoding error: \(decodingError)")
            }
            return []
        } catch {
            logger.error("lsblk JSON parsing failed with error: \(error)")
            return []
        }

        return drives
    }

    // Helper function to find lsblk executable
    private func findLsblkPath() -> String? {
        // Common paths
        let possiblePaths = [
            "/usr/bin/lsblk",
            "/bin/lsblk",
            "/sbin/lsblk",
            "/usr/sbin/lsblk",
        ]

        for path in possiblePaths {
            if FileManager.default.fileExists(atPath: path) {
                return path
            }
        }

        // Try using 'which' to find lsblk if it's not one of the common paths
        do {
            let process = Process()
            process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
            process.arguments = ["which", "lsblk"]

            let pipe = Pipe()
            process.standardOutput = pipe
            process.standardError = Pipe()

            try process.run()
            process.waitUntilExit()

            if process.terminationStatus == 0 {
                let data = pipe.fileHandleForReading.readDataToEndOfFile()
                if let path = String(data: data, encoding: .utf8)?.trimmingCharacters(
                    in: .whitespacesAndNewlines
                ),
                    !path.isEmpty
                {
                    return path
                }
            }
        } catch {
            logger.error("Unable to find lsblk path: ", metadata: ["error": "\(error)"])
        }

        return nil
    }
}
