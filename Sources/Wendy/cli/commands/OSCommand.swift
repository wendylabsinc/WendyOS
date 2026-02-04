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
            OSInstallCommand.self,
            CacheCommand.self,
        ],
        groupedSubcommands: [
            CommandGroup(
                name: "Advanced OS utilities",
                subcommands: [
                    ListDrivesCommand.self,
                    ListDevicesCommand.self,
                    WriteCommand.self,
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

                let timestampFormatter = DateFormatter()
                timestampFormatter.locale = Locale(identifier: "en_US_POSIX")
                timestampFormatter.timeZone = TimeZone.current
                timestampFormatter.dateFormat = "MMM d yyyy h:mma zzz"

                let deviceRows = devices.map { device -> [String] in
                    let version: String
                    let uploadedAt: String
                    let downloadedAt: String
                    let path: String
                    if nightly {
                        version = device.latestNightlyVersion ?? "—"
                        uploadedAt =
                            device.latestNightlyReleaseDate.map {
                                timestampFormatter.string(from: $0)
                            } ?? "—"
                        downloadedAt =
                            cachedImageDownloadDate(
                                deviceName: device.name,
                                nightly: true
                            ).map {
                                timestampFormatter.string(from: $0)
                            } ?? "—"
                        path = device.latestNightlyPath ?? "—"
                    } else {
                        version = device.latestVersion.isEmpty ? "—" : device.latestVersion
                        uploadedAt =
                            device.latestVersionReleaseDate.map {
                                timestampFormatter.string(from: $0)
                            } ?? "—"
                        downloadedAt =
                            cachedImageDownloadDate(
                                deviceName: device.name,
                                nightly: false
                            ).map {
                                timestampFormatter.string(from: $0)
                            } ?? "—"
                        path = device.latestVersionPath ?? "—"
                    }
                    return [
                        device.name,
                        version,
                        uploadedAt,
                        downloadedAt,
                        path,
                    ]
                }

                let deviceIndex = try await noora.selectableTable(
                    headers: [
                        "Device",
                        nightly ? "Latest Nightly" : "Latest Version",
                        "Uploaded At",
                        "Downloaded At",
                        "Path",
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
            let (imageUrl, imageSize, latestVersion, releaseDate) =
                try await manifestManager.getLatestImageInfo(
                    for: selectedDeviceName,
                    nightly: nightly
                )

            let timestampFormatter = DateFormatter()
            timestampFormatter.locale = Locale(identifier: "en_US_POSIX")
            timestampFormatter.timeZone = TimeZone.current
            timestampFormatter.dateFormat = "MMM d yyyy h:mma zzz"

            let downloadedAt = cachedImageDownloadDate(
                deviceName: selectedDeviceName,
                nightly: nightly
            )

            noora.info("📥 Found image for \(selectedDeviceName)")
            noora.table(
                headers: [
                    "Image",
                    "Version",
                    "Size",
                    "Uploaded At",
                    "Downloaded At",
                ],
                rows: [
                    [
                        imageUrl.lastPathComponent,
                        latestVersion,
                        ByteCountFormatter.string(
                            fromByteCount: Int64(imageSize),
                            countStyle: .file
                        ),
                        releaseDate.map { timestampFormatter.string(from: $0) } ?? "—",
                        downloadedAt.map { timestampFormatter.string(from: $0) } ?? "—",
                    ]
                ]
            )

            // Download archive and stream directly to disk (no extraction step)
            let imageDownloader = ImageDownloaderFactory.createImageDownloader()

            var zipPath: String

            // Check if cached zip exists and matches the latest version
            let cachedZipPath = try imageDownloader.cachedZipIfValid(
                deviceName: selectedDeviceName,
                nightly: nightly
            )
            let isCachedLatest = try imageDownloader.isCachedImageLatest(
                deviceName: selectedDeviceName,
                latestVersion: latestVersion,
                nightly: nightly
            )
            let shouldUseCache = !redownload && cachedZipPath != nil && isCachedLatest

            if shouldUseCache, let cachedPath = cachedZipPath {
                zipPath = cachedPath
                noora.info(
                    "Using cached image for \(selectedDeviceName) (version: \(latestVersion))"
                )
            } else {
                if !redownload && cachedZipPath != nil && !isCachedLatest {
                    noora.info("Newer version available, downloading updated image...")
                }
                // Download archive to cache
                zipPath = try await noora.progressBarStep(
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
            }

            logger.debug("✅ Archive ready at: \(zipPath)")
            noora.info(
                """
                💾 Writing image to \(selectedDrive.name) (\(selectedDrive.id))...
                   Streaming decompression directly to disk (no extraction step)
                   Press Ctrl+C to cancel
                """
            )

            // Ensure we have admin privileges cached up-front so downstream sudo calls don't fail silently
            try await ensureAdminPrivileges()

            // Use DiskWriter to stream the zip directly to disk
            let diskWriter = DiskWriterFactory.createDiskWriter()

            try await noora.progressBarStep(
                message: "Writing image to \(selectedDrive.name) (\(selectedDrive.id))",
                successMessage: "Image successfully written to \(selectedDrive.name)",
                errorMessage: "Failed to write the image"
            ) { updateProgress in
                let progressUpdater = SendableProgressUpdater(updateProgress)
                let monotonic = Monotonic()
                try await diskWriter.writeFromZip(zipPath: zipPath, drive: selectedDrive) { p in
                    if let percent = p.percentComplete {
                        let fraction = clampProgress(percent / 100.0)
                        Task {
                            let m = await monotonic.next(fraction)
                            progressUpdater.update(m)
                        }
                    }
                }
                progressUpdater.update(1.0)
            }

            noora.success("🎉 Device \(selectedDeviceName) successfully imaged!")
        }
    }
}

// MARK: - Helpers

/// Ensure the user has active sudo credentials before attempting privileged disk operations.
/// This warms up sudo so subsequent calls (diskutil/dd) won't fail due to missing TTY prompts.
private func ensureAdminPrivileges() async throws {
    #if os(macOS) || os(Linux)
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
