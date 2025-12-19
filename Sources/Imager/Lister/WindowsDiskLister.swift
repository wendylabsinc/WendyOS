#if os(Windows)
    import Foundation
    import Logging
    import Subprocess

    /// Windows implementation of the DiskLister protocol using PowerShell.
    public struct WindowsDiskLister: DiskLister {
        let logger = Logger(label: "WindowsDiskLister")

        public init() {}

        /// Lists available drives on Windows.
        /// - Parameter all: If true, lists all drives, not just external/removable drives.
        /// - Returns: An array of Drive objects representing the available drives.
        public func list(all: Bool) async throws -> [Drive] {
            let powerShellScript = all
                ? "Get-Disk | Select-Object Number, BusType, IsSystem, Model, IsReadOnly, FriendlyName, Size | ConvertTo-Json"
                : "Get-Disk | Where-Object { $_.BusType -eq 'USB' -or $_.MediaType -eq 'Removable Media' } | Select-Object Number, BusType, IsSystem, Model, IsReadOnly, FriendlyName, Size | ConvertTo-Json"

            let result = try await Subprocess.run(
                Subprocess.Executable.name("powershell.exe"),
                arguments: ["-NoProfile", "-Command", powerShellScript],
                output: .string(limit: .max),
                error: .string(limit: .max)
            )

            if result.terminationStatus.isSuccess {
                guard let output = result.standardOutput, !output.isEmpty else {
                    return []
                }

                do {
                    return try parsePowerShellOutput(output)
                } catch {
                    return [
                        try parseSingleDiskOutput(output)
                    ]
                }
            } else {
                let stderr = result.standardError ?? "Unknown error"
                throw DiskListerError.listFailed(error: stderr)
            }
        }

        /// Finds a drive by its identifier.
        /// - Parameter id: The identifier of the drive to find (e.g., "0" for Disk0).
        /// - Returns: The Drive object if found.
        /// - Throws: If the drive cannot be found.
        public func findDrive(byId id: String) async throws -> Drive {
            let powerShellScript = "Get-Disk -Number \(id) | Select-Object Number, BusType, IsSystem, Model, IsReadOnly, FriendlyName, Size | ConvertTo-Json"

            let result = try await Subprocess.run(
                Subprocess.Executable.name("powershell.exe"),
                arguments: ["-NoProfile", "-Command", powerShellScript],
                output: .string(limit: .max),
                error: .string(limit: .max)
            )

            if result.terminationStatus.isSuccess {
                guard let output = result.standardOutput, !output.isEmpty else {
                    throw DiskListerError.driveNotFound(id: id, error: "Drive not found")
                }

                return try parseSingleDiskOutput(output)
            } else {
                let stderr = result.standardError ?? "Unknown error"
                throw DiskListerError.driveNotFound(id: id, error: stderr)
            }
        }

        private func parsePowerShellOutput(_ json: String) throws -> [Drive] {
            guard let jsonData = json.data(using: .utf8) else {
                throw DiskListerError.unknownOutput
            }

            let decoder = JSONDecoder()
            let disks = try decoder.decode([DiskInfo].self, from: jsonData)

            return disks.map { disk in
                Drive(
                    id: "Disk\(disk.Number)",
                    name: disk.FriendlyName ?? "Disk \(disk.Number)",
                    available: 0,  // Windows drive available space would require additional queries
                    capacity: disk.Size ?? 0,
                    isExternal: disk.BusType == "USB" || disk.BusType == "SD" || disk.BusType == "IEEE1394"
                )
            }
        }

        struct DiskInfo: Decodable {
            let Number: Int
            let FriendlyName: String?
            let Size: Int64?
            let BusType: String
            let IsSystem: Bool
            let IsReadOnly: Bool
            let Model: String?
        }

        private func parseSingleDiskOutput(_ json: String) throws -> Drive {
            guard let jsonData = json.data(using: .utf8) else {
                throw DiskListerError.unknownOutput
            }

            let decoder = JSONDecoder()
            let disk = try decoder.decode(DiskInfo.self, from: jsonData)

            return Drive(
                id: "Disk\(disk.Number)",
                name: disk.FriendlyName ?? "Disk \(disk.Number)",
                available: 0,
                capacity: disk.Size ?? 0,
                isExternal: true
            )
        }
    }
#endif