import ArgumentParser
import Foundation
import Imager
import Logging
import Noora
import Subprocess

#if os(macOS)
    import Darwin
#elseif canImport(Glibc)
    import Glibc
#elseif canImport(Musl)
    import Musl
#endif

@inline(__always) private func formatDiskContents(available: Int64, capacity: Int64) -> String {
    let availableText = ByteCountFormatter.string(fromByteCount: available, countStyle: .file)
    let capacityText = ByteCountFormatter.string(fromByteCount: capacity, countStyle: .file)
    return "\(availableText) free / \(capacityText) total"
}

@inline(__always) private func clampProgress(_ value: Double) -> Double {
    guard value.isFinite else { return 0.0 }
    return min(max(value, 0.0), 1.0)
}

private struct SendableProgressUpdater: @unchecked Sendable {
    let call: (Double) -> Void

    init(_ call: @escaping (Double) -> Void) {
        self.call = call
    }

    func update(_ value: Double) {
        // Ensure UI/tui updates happen on the main actor to avoid missed refreshes
        Task { @MainActor in
            self.call(value)
        }
    }
}

// Removed unused text/progress formatting helpers; Noora handles rendering.

private enum DeviceFamily: String, CaseIterable {
    // Prefer showing NVIDIA Jetson first in interactive lists
    case nvidiaJetson = "NVIDIA Jetson"
    case raspberryPi = "Raspberry Pi"
    case other = "Other Devices"
}

@inline(__always) private func inferFamily(for deviceName: String) -> DeviceFamily {
    let lowercased = deviceName.lowercased()
    if lowercased.contains("raspberry") || lowercased.contains("pi") {
        return .raspberryPi
    }
    if lowercased.contains("jetson") || lowercased.contains("nvidia") {
        return .nvidiaJetson
    }
    return .other
}

@inline(__always) private func orderedFamilies(
    from devices: [DeviceInfo]
) -> [(DeviceFamily, [DeviceInfo])] {
    let grouped = Dictionary(grouping: devices) { inferFamily(for: $0.name) }
    return DeviceFamily.allCases.compactMap { family in
        guard let items = grouped[family] else { return nil }
        return (family, sortDevices(items, for: family))
    }
}

@inline(__always) private func sortDevices(
    _ items: [DeviceInfo],
    for family: DeviceFamily
) -> [DeviceInfo] {
    switch family {
    case .nvidiaJetson:
        // Prefer Orin Nano and other Nano boards first, and place AGX variants last.
        return items.sorted { a, b in
            func rank(_ name: String) -> Int {
                let s = name.lowercased()
                if s.contains("orin-nano") { return 0 }
                if s.contains("nano") { return 1 }
                if s.contains("orin") { return 2 }
                if s.contains("agx") { return 3 }
                return 4
            }
            let ra = rank(a.name)
            let rb = rank(b.name)
            if ra != rb { return ra < rb }
            return a.name < b.name
        }
    default:
        return items.sorted { $0.name < $1.name }
    }
}

struct OSCommand: AsyncParsableCommand {

    static let configuration = CommandConfiguration(
        commandName: "os",
        abstract: "Download and install WendyOS",
        subcommands: [
            OSInstallCommand.self
        ],
        groupedSubcommands: [
            CommandGroup(
                name: "Advanced OS utilities",
                subcommands: [
                    ListDrivesCommand.self,
                    ListDevicesCommand.self,
                    WriteCommand.self,
                    CacheCommand.self,
                ]
            )
        ]
    )

    struct ListDrivesCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "list-drives",
            abstract: "List available external drives."
        )

        @Flag(name: .long, help: "List all drives, not just external drives")
        var all: Bool = false

        func run() async throws {
            let diskLister = DiskListerFactory.createDiskLister()
            let drives = try await diskLister.list(all: all)

            if JSONMode.isEnabled {
                let jsonString = try JSONEncoder().encode(drives)
                print(String(data: jsonString, encoding: .utf8)!)
            } else if drives.isEmpty {
                print("No external drives found.")
            } else {
                print("\nAvailable drives:")
                Noora().table(
                    headers: [
                        "Disk",
                        "Identifier",
                        "Contents",
                        "Type",
                    ],
                    rows: drives.map { drive in
                        [
                            drive.name,
                            drive.id,
                            formatDiskContents(
                                available: drive.available,
                                capacity: drive.capacity
                            ),
                            drive.isExternal ? "External" : "Internal",
                        ]
                    }
                )
            }
        }
    }

    struct ListDevicesCommand: AsyncParsableCommand {

        static let configuration = CommandConfiguration(
            commandName: "supported-devices",
            abstract: "List supported device images"
        )

        func run() async throws {
            if !JSONMode.isEnabled {
                print("📱 Fetching available device images...")
            }

            let manifestManager = ManifestManagerFactory.createManifestManager()
            let deviceList = try await manifestManager.getAvailableDevices()

            if JSONMode.isEnabled {
                let jsonString = try JSONEncoder().encode(deviceList)
                print(String(data: jsonString, encoding: .utf8)!)
            } else if deviceList.isEmpty {
                print("No devices found in the manifest.")
            } else {
                let noora = Noora()
                print("\nAvailable devices:")
                noora.table(
                    headers: [
                        "Device",
                        "Latest Version",
                        "Latest Nightly",
                        "Stability",
                    ],
                    rows: deviceList.map { device in
                        let stabilityIcon: String
                        switch device.stability {
                        case .stable:
                            stabilityIcon = "✓ Stable"
                        case .experimental:
                            stabilityIcon = "⚠ Experimental"
                        case .deprecated:
                            stabilityIcon = "⚠ Deprecated"
                        }
                        return [
                            device.name,
                            device.latestVersion.isEmpty ? "Not Available" : device.latestVersion,
                            device.latestNightlyVersion ?? "—",
                            stabilityIcon,
                        ]
                    }
                )
                print(
                    "\nUse `wendy disk write-device <device-name> <drive-id>` for scripted usage or add `--interactive` for guided selection."
                )
            }
        }
    }

    struct WriteCommand: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            commandName: "write",
            abstract: "Write an image to a drive."
        )

        @Argument(help: "Path to the image file to write")
        var imagePath: String

        @Argument(help: "Target drive to write to")
        var driveId: String

        func run() async throws {
            // Ensure we have admin privileges cached up-front so downstream sudo calls don't fail silently
            try await ensureAdminPrivileges()
            // Use DiskLister to find the drive
            let diskLister = DiskListerFactory.createDiskLister()
            let drive = try await diskLister.findDrive(byId: driveId)

            // Use DiskWriter to write the image with Noora progress bar
            let diskWriter = DiskWriterFactory.createDiskWriter()
            let noora = Noora()

            print("Press Ctrl+C to cancel\n")

            try await noora.progressBarStep(
                message: "Writing image to \(drive.name) (\(drive.id))",
                successMessage: "Image successfully written to \(drive.name)",
                errorMessage: "Failed to write the image"
            ) { updateProgress in
                let progressUpdater = SendableProgressUpdater(updateProgress)
                let monotonic = Monotonic()
                try await diskWriter.write(imagePath: imagePath, drive: drive) { p in
                    if let percent = p.percentComplete {
                        let fraction = clampProgress(percent / 100.0)
                        Task {
                            let m = await monotonic.next(fraction)
                            progressUpdater.update(m)
                        }
                    }
                    // Avoid printing extra lines to keep Noora progress single-line.
                }
                progressUpdater.update(1.0)
            }
        }
    }

    struct OSInstallCommand: AsyncParsableCommand {

        static let configuration = CommandConfiguration(
            commandName: "install",
            abstract: "Install WendyOS on a device."
        )

        @Argument(help: "Device name (e.g., raspberry-pi-5)")
        var deviceName: String?

        @Argument(help: "Target drive to write to")
        var driveId: String?

        @Flag(name: .long, help: "Skip confirmation before writing")
        var force: Bool = false

        @Flag(name: .long, help: "Force redownload and write the image")
        var redownload: Bool = false

        @Flag(name: .long, help: "Install the latest nightly build instead of stable release")
        var nightly: Bool = false

        func run() async throws {
            let logger = Logger(label: "wendy.imager")
            let manifestManager = ManifestManagerFactory.createManifestManager()
            let diskLister = DiskListerFactory.createDiskLister()
            let noora = Noora()

            #if os(Windows)
                noora.info(
                    "Administrator privileges are required to write raw disks. Please ensure you have administrative rights."
                )
            #endif

            let selectedDeviceName: String
            // Interactive device selection is the default when deviceName is omitted
            if deviceName == nil {
                let allDevices = try await manifestManager.getAvailableDevices()
                guard !allDevices.isEmpty else {
                    noora.error("No devices found in the manifest.")
                    return
                }

                let familyOptions = orderedFamilies(from: allDevices)
                let familyRows = familyOptions.map { option -> [String] in
                    [
                        option.0.rawValue,
                        "\(option.1.count) option\(option.1.count == 1 ? "" : "s")",
                    ]
                }

                let familyIndex = try await noora.selectableTable(
                    headers: [
                        "Device Family",
                        "Available Images",
                    ],
                    rows: familyRows,
                    pageSize: familyRows.count
                )

                let devices = familyOptions[familyIndex].1

                let deviceRows = devices.map { device -> [String] in
                    let version: String
                    if nightly {
                        version = device.latestNightlyVersion ?? "—"
                    } else {
                        version = device.latestVersion.isEmpty ? "—" : device.latestVersion
                    }
                    return [
                        device.name,
                        version,
                    ]
                }

                let deviceIndex = try await noora.selectableTable(
                    headers: [
                        "Device",
                        nightly ? "Latest Nightly" : "Latest Version",
                    ],
                    rows: deviceRows,
                    pageSize: deviceRows.count
                )

                selectedDeviceName = devices[deviceIndex].name
            } else if let deviceName {
                selectedDeviceName = deviceName
            } else {
                // Should be unreachable; covered by interactive default above
                throw ValidationError("Missing device name.")
            }

            let selectedDrive: Drive
            // Interactive drive selection is the default when driveId is omitted
            if driveId == nil {
                var drives = try await diskLister.list(all: true)
                drives.removeAll { $0.id.hasSuffix("disk0") }

                guard !drives.isEmpty else {
                    noora.error("No removable drives detected.")
                    return
                }

                let driveRows = drives.map { drive in
                    [
                        drive.name,
                        drive.id,
                        formatDiskContents(available: drive.available, capacity: drive.capacity),
                        drive.isExternal ? "External" : "Internal",
                    ]
                }

                let driveIndex = try await noora.selectableTable(
                    headers: [
                        "Disk",
                        "Identifier",
                        "Contents",
                        "Type",
                    ],
                    rows: driveRows,
                    pageSize: driveRows.count
                )

                let driveChoice = drives[driveIndex]

                noora.warning(
                    "Writing \(selectedDeviceName) will erase all data on \(driveChoice.name) (\(driveChoice.id))."
                )

                if force {
                    noora.warning("Proceeding due to --force flag.")
                    selectedDrive = driveChoice
                } else {
                    let confirmed = noora.yesOrNoChoicePrompt(
                        question: "Do you want to continue?",
                        defaultAnswer: false
                    )

                    guard confirmed else {
                        noora.info("Operation aborted.")
                        return
                    }

                    selectedDrive = driveChoice
                }
            } else if let driveId {
                selectedDrive = try await diskLister.findDrive(byId: driveId)
            } else {
                // Should be unreachable; covered by interactive default above
                throw ValidationError("Missing drive identifier.")
            }

            if driveId != nil && !force {
                print(
                    "\n⚠️  WARNING: All data on \(selectedDrive.name) (\(selectedDrive.id)) will be erased."
                )
                print("   Type 'yes' to continue or any other key to abort:")

                let response = readLine()?.lowercased()
                guard response == "yes" else {
                    print("Operation aborted.")
                    return
                }
            }

            if nightly {
                noora.info("🔍 Finding latest nightly image for \(selectedDeviceName)...")
            } else {
                noora.info("🔍 Finding latest image for \(selectedDeviceName)...")
            }

            // Get the latest image information for the device
            let (imageUrl, imageSize, latestVersion) = try await manifestManager.getLatestImageInfo(
                for: selectedDeviceName,
                nightly: nightly
            )

            noora.info(
                """
                📥 Found image: \(imageUrl.lastPathComponent)
                   Version: \(latestVersion)
                   Size: \(ByteCountFormatter.string(fromByteCount: Int64(imageSize), countStyle: .file))
                """
            )

            // Download and extract as separate progress bars when not using cache
            let imageDownloader = ImageDownloaderFactory.createImageDownloader()

            var localImagePath: String

            // Check if cached image exists and matches the latest version
            let cachedImagePath = try await imageDownloader.cachedImageIfValid(
                deviceName: selectedDeviceName,
                nightly: nightly
            )
            let isCachedLatest = try imageDownloader.isCachedImageLatest(
                deviceName: selectedDeviceName,
                latestVersion: latestVersion,
                nightly: nightly
            )
            let shouldUseCache = !redownload && cachedImagePath != nil && isCachedLatest

            if shouldUseCache, let cachedPath = cachedImagePath {
                localImagePath = cachedPath
                noora.info(
                    "Using cached image for \(selectedDeviceName) (version: \(latestVersion))"
                )
            } else {
                if !redownload && cachedImagePath != nil && !isCachedLatest {
                    noora.info("Newer version available, downloading updated image...")
                }
                // 1) Download archive
                let (zipPath, _): (String, String) = try await noora.progressBarStep(
                    message: "Downloading image for \(selectedDeviceName)",
                    successMessage: "Download complete",
                    errorMessage: "Failed to download image"
                ) { updateProgress in
                    let progressUpdater = SendableProgressUpdater(updateProgress)
                    let monotonic = Monotonic()
                    let result = try await imageDownloader.downloadArchiveOnly(
                        from: imageUrl,
                        deviceName: selectedDeviceName,
                        expectedSize: imageSize,
                        redownload: redownload,
                        version: latestVersion,
                        nightly: nightly
                    ) { progress in
                        let totalUnits = max(1, progress.totalUnitCount)
                        let fraction = clampProgress(
                            Double(progress.completedUnitCount) / Double(totalUnits)
                        )
                        Task {
                            let m = await monotonic.next(fraction)
                            progressUpdater.update(m)
                        }
                    }
                    progressUpdater.update(1.0)
                    return result
                }

                // 2) Extract archive
                localImagePath = try await noora.progressBarStep(
                    message: "Extracting image",
                    successMessage: "Image ready",
                    errorMessage: "Failed to extract image"
                ) { updateProgress in
                    let progressUpdater = SendableProgressUpdater(updateProgress)
                    let monotonic = Monotonic()
                    let result = try await imageDownloader.extractArchiveOnly(
                        deviceName: selectedDeviceName,
                        zipPath: zipPath,
                        version: latestVersion,
                        nightly: nightly
                    ) { p in
                        let total = max(1, p.totalUnitCount)
                        let fraction = clampProgress(Double(p.completedUnitCount) / Double(total))
                        Task {
                            let m = await monotonic.next(fraction)
                            progressUpdater.update(m)
                        }
                    }
                    progressUpdater.update(1.0)
                    return result
                }
            }

            logger.debug("✅ Image ready at: \(localImagePath)")
            noora.info(
                """
                💾 Writing image to \(selectedDrive.name) (\(selectedDrive.id))...
                   Press Ctrl+C to cancel
                """
            )

            // Ensure we have admin privileges cached up-front so downstream sudo calls don't fail silently
            try await ensureAdminPrivileges()

            // Use DiskWriter to write the image with Noora progress bar
            let diskWriter = DiskWriterFactory.createDiskWriter()

            try await noora.progressBarStep(
                message: "Writing image to \(selectedDrive.name) (\(selectedDrive.id))",
                successMessage: "Image successfully written to \(selectedDrive.name)",
                errorMessage: "Failed to write the image"
            ) { updateProgress in
                let progressUpdater = SendableProgressUpdater(updateProgress)
                let monotonic = Monotonic()
                try await diskWriter.write(imagePath: localImagePath, drive: selectedDrive) { p in
                    if let percent = p.percentComplete {
                        let fraction = clampProgress(percent / 100.0)
                        Task {
                            let m = await monotonic.next(fraction)
                            progressUpdater.update(m)
                        }
                    }
                    // Avoid extra prints to keep the progress bar on a single line.
                }
                progressUpdater.update(1.0)
            }

            noora.success("🎉 Device \(selectedDeviceName) successfully imaged!")
        }
    }
}

// MARK: - Cache utilities

private enum CachedImageStatus: String, Codable {
    case ready
    case incomplete
    case empty

    var displayValue: String {
        switch self {
        case .ready:
            return "Ready"
        case .incomplete:
            return "Incomplete"
        case .empty:
            return "Empty"
        }
    }
}

private struct CachedImageEntry: Codable {
    let device: String
    let version: String?
    let cachedAt: Date?
    let imagePath: String?
    let sizeBytes: Int64
    let status: CachedImageStatus
}

private func cacheDirectory(fileManager: FileManager = .default) -> URL {
    fileManager.homeDirectoryForCurrentUser.appendingPathComponent(".wendy/cache/images")
}

private func listCachedImages(fileManager: FileManager = .default) throws -> [CachedImageEntry] {
    let root = cacheDirectory(fileManager: fileManager)
    guard fileManager.fileExists(atPath: root.path) else { return [] }

    let contents: [URL]
    do {
        contents = try fileManager.contentsOfDirectory(
            at: root,
            includingPropertiesForKeys: [.isDirectoryKey],
            options: [.skipsHiddenFiles]
        )
    } catch {
        throw ValidationError("Failed to read cache directory: \(error.localizedDescription)")
    }

    var entries: [CachedImageEntry] = []

    for url in contents {
        let values = try? url.resourceValues(forKeys: [.isDirectoryKey])
        guard values?.isDirectory == true else { continue }

        let (version, timestamp) = readCacheMetadata(at: url)
        let imageURL = findImageFile(in: url, fileManager: fileManager)
        let size = directorySize(of: url, fileManager: fileManager)

        let status: CachedImageStatus
        if imageURL != nil {
            status = .ready
        } else if size > 0 {
            status = .incomplete
        } else {
            status = .empty
        }

        entries.append(
            CachedImageEntry(
                device: url.lastPathComponent,
                version: version,
                cachedAt: timestamp,
                imagePath: imageURL?.path,
                sizeBytes: size,
                status: status
            )
        )
    }

    return entries.sorted { $0.device < $1.device }
}

private func readCacheMetadata(at url: URL) -> (String?, Date?) {
    let metadataURL = url.appendingPathComponent("version.json")

    guard let data = try? Data(contentsOf: metadataURL) else {
        return (nil, nil)
    }

    struct CacheVersionMetadata: Codable {
        let version: String
        let timestamp: Date
    }

    let decoder = JSONDecoder()
    decoder.dateDecodingStrategy = .iso8601
    guard let metadata = try? decoder.decode(CacheVersionMetadata.self, from: data) else {
        return (nil, nil)
    }

    return (metadata.version, metadata.timestamp)
}

private func directorySize(of url: URL, fileManager: FileManager = .default) -> Int64 {
    guard
        let enumerator = fileManager.enumerator(
            at: url,
            includingPropertiesForKeys: [
                .isRegularFileKey, .totalFileAllocatedSizeKey, .fileSizeKey,
            ],
            options: [.skipsHiddenFiles]
        )
    else { return 0 }

    var total: Int64 = 0
    for case let fileURL as URL in enumerator {
        guard
            let values = try? fileURL.resourceValues(
                forKeys: [.isRegularFileKey, .totalFileAllocatedSizeKey, .fileSizeKey]
            ),
            values.isRegularFile == true
        else { continue }

        if let allocated = values.totalFileAllocatedSize {
            total += Int64(allocated)
        } else if let size = values.fileSize {
            total += Int64(size)
        }
    }

    return total
}

private func findImageFile(in url: URL, fileManager: FileManager = .default) -> URL? {
    guard
        let enumerator = fileManager.enumerator(
            at: url,
            includingPropertiesForKeys: [.isRegularFileKey],
            options: [.skipsHiddenFiles]
        )
    else { return nil }

    for case let fileURL as URL in enumerator {
        guard
            let values = try? fileURL.resourceValues(forKeys: [.isRegularFileKey]),
            values.isRegularFile == true
        else { continue }

        if fileURL.pathExtension.lowercased() == "img" {
            return fileURL
        }
    }

    return nil
}

// MARK: - Helpers

/// Ensure the user has active admin credentials before attempting privileged disk operations.
/// This warms up sudo on macOS/Linux or validates admin rights on Windows.
private func ensureAdminPrivileges() async throws {
    #if os(Windows)
        // Check if running as administrator
        let result = try await Subprocess.run(
            Subprocess.Executable.name("powershell.exe"),
            arguments: [
                "-NoProfile", "-Command",
                "[Security.Principal.WindowsIdentity]::GetCurrent().Owner",
            ],
            output: .string(limit: .max),
            error: .discarded
        )

        guard result.terminationStatus.isSuccess else {
            throw ValidationError(
                "Failed to check administrator status. Please run as administrator."
            )
        }

        // Inform the user about privilege requirements
        Noora().info(
            "Administrator privileges are required to write raw disks. Continuing..."
        )
    #elseif os(macOS) || os(Linux)
        // If already root, nothing to do
        if getuid() == 0 { return }

        // Inform the user and validate sudo timestamp (may prompt for password in the terminal)
        Noora().info(
            "Administrator privileges are required to write raw disks. You may be prompted for your password."
        )
        do {
            let result = try await Subprocess.run(
                Subprocess.Executable.name("sudo"),
                arguments: ["-v"],
                output: .discarded,
                error: .discarded
            )
            guard result.terminationStatus.isSuccess else {
                throw ValidationError(
                    "Failed to acquire sudo privileges. Try: sudo wendy … or ensure your user can use sudo."
                )
            }
        } catch {
            throw ValidationError(
                "Unable to prompt for admin privileges. Try re-running the command with sudo."
            )
        }
    #endif
}
