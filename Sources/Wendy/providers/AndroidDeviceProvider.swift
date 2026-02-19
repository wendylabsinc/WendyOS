import CLIOutput
import Foundation
import Logging
import Subprocess
import WendyShared

#if os(macOS)
    import System
#else
    import SystemPackage
#endif

/// Build context carried between the build and run phases for Android
struct AndroidBuildContext: Sendable {
    let apkPath: String
    let serialNumber: String
}

/// Device provider for Android devices via ADB.
/// Uses `adb devices -l` for discovery, `swift package bundle-apk` for
/// building, and `adb install` for deployment.
struct AndroidDeviceProvider: DeviceProvider, Sendable {
    let key = "android"
    let displayName = "Android (ADB)"

    private let logger = Logger(label: "sh.wendy.provider.android")

    // MARK: - Availability

    func isAvailable() async -> Bool {
        do {
            let result = try await Subprocess.run(
                .name("adb"),
                arguments: ["version"],
                output: .discarded,
                error: .discarded
            )
            return result.terminationStatus.isSuccess
        } catch {
            return false
        }
    }

    // MARK: - Requirements

    func checkRequirements(shouldAutoAccept: Bool) async throws {
        // Verify adb is available
        guard await isAvailable() else {
            cliOutput.error(
                """
                ADB (Android Debug Bridge) is not installed.

                Install it via Android SDK or standalone platform-tools:
                  macOS:   brew install android-platform-tools
                  Linux:   sudo apt install android-tools-adb

                For more information: https://developer.android.com/tools/adb
                """
            )
            throw CLIError.serviceNotInstalled(name: "adb")
        }
    }

    // MARK: - Discovery

    func discoverDevices() async throws -> [ExternalDevice] {
        let result = try await Subprocess.run(
            .name("adb"),
            arguments: ["devices", "-l"],
            output: .string(limit: 100_000),
            error: .discarded
        )

        guard result.terminationStatus.isSuccess, let output = result.standardOutput else {
            return []
        }

        // Parse `adb devices -l` output. Each device line looks like:
        // HVA12345               device usb:1-1 product:redfin model:Pixel_5 transport_id:3
        var devices = [ExternalDevice]()
        for line in output.split(separator: "\n") {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            // Skip header and empty lines
            if trimmed.isEmpty || trimmed.hasPrefix("List of") {
                continue
            }

            let parts = trimmed.split(separator: " ", maxSplits: 1)
            guard parts.count >= 2 else { continue }

            let serial = String(parts[0])
            let rest = String(parts[1]).trimmingCharacters(in: .whitespaces)

            // Only include devices in "device" state (not "offline", "unauthorized", etc.)
            guard rest.hasPrefix("device") else { continue }

            // Parse key:value pairs from the rest of the line
            var properties = [String: String]()
            for token in rest.split(separator: " ") {
                let kv = token.split(separator: ":", maxSplits: 1)
                if kv.count == 2 {
                    properties[String(kv[0])] = String(kv[1])
                }
            }

            let model =
                properties["model"]?.replacingOccurrences(of: "_", with: " ")
                ?? serial
            let product = properties["product"] ?? "unknown"

            devices.append(
                ExternalDevice(
                    id: "adb:\(serial)",
                    displayName: model,
                    providerKey: key,
                    connectionInfo: [
                        "serial": serial,
                        "product": product,
                    ],
                    os: "Android",
                    cpuArchitecture: await getDeviceArch(serial: serial)
                )
            )
        }

        return devices
    }

    /// Query the CPU architecture of a connected device
    private func getDeviceArch(serial: String) async -> String? {
        do {
            let result = try await Subprocess.run(
                .name("adb"),
                arguments: ["-s", serial, "shell", "uname", "-m"],
                output: .string(limit: 1000),
                error: .discarded
            )
            guard result.terminationStatus.isSuccess, let output = result.standardOutput else {
                return nil
            }
            return output.trimmingCharacters(in: .whitespacesAndNewlines)
        } catch {
            return nil
        }
    }

    // MARK: - Build

    func canBuild(projectPath: URL) async -> Bool {
        FileManager.default.fileExists(
            atPath: projectPath.appendingPathComponent("Package.swift").path
        )
    }

    func build(
        for device: ExternalDevice,
        projectPath: URL,
        executable: String,
        debug: Bool
    ) async throws -> ProviderBuiltApp {
        guard let serial = device.connectionInfo["serial"] else {
            throw CLIError.invalidArgument(
                name: "device",
                value: device.id,
                reason: "Missing serial number in connection info"
            )
        }

        // Build APK using swift-package bundle-apk plugin
        let apkFilename = "\(executable).apk"
        let result = try await Subprocess.run(
            .name("swiftly"),
            arguments: Arguments([
                "run",
                "+main-snapshot",
                "swift",
                "package",
                "--force-resolved-versions",
                "--disable-sandbox",
                "--allow-writing-to-package-directory",
                "bundle-apk",
                "--product", executable,
                apkFilename,
            ]),
            workingDirectory: FilePath(projectPath.path),
            output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false),
            error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
        )

        guard result.terminationStatus.isSuccess else {
            throw CLIError.commandFailed(
                command: "swift package bundle-apk",
                exitCode: Int32(result.terminationStatus.exitCode),
                output: "APK bundling for Android failed"
            )
        }

        // Locate the built APK
        let apkPath =
            projectPath
            .appendingPathComponent(".build-apk")
            .appendingPathComponent(apkFilename)
            .path

        guard FileManager.default.fileExists(atPath: apkPath) else {
            throw CLIError.fileNotFound(path: apkPath)
        }

        let context = AndroidBuildContext(
            apkPath: apkPath,
            serialNumber: serial
        )

        cliOutput.success("Built \(executable) for Android")

        return ProviderBuiltApp(
            provider: self,
            device: device,
            appName: executable,
            context: context
        )
    }

    // MARK: - Run

    func run(
        _ builtApp: ProviderBuiltApp,
        detach: Bool,
        output: AsyncStream<ProviderRunOutput>.Continuation
    ) async throws {
        guard let ctx = builtApp.context as? AndroidBuildContext else {
            throw CLIError.invalidArgument(
                name: "context",
                value: "unknown",
                reason: "Invalid build context for Android provider"
            )
        }

        // Install the APK on the device
        cliOutput.info("Installing \(builtApp.appName) on device...")
        let installResult = try await Subprocess.run(
            .name("adb"),
            arguments: ["-s", ctx.serialNumber, "install", "-r", ctx.apkPath],
            output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false),
            error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
        )

        guard installResult.terminationStatus.isSuccess else {
            throw CLIError.commandFailed(
                command: "adb install",
                exitCode: Int32(installResult.terminationStatus.exitCode),
                output: "Failed to install APK on device"
            )
        }

        // Extract package name and launchable activity from the APK
        let (packageName, launchableActivity) = try await extractLaunchInfo(from: ctx.apkPath)

        // Launch the app on the device
        cliOutput.info("Launching \(builtApp.appName)...")
        let launchResult = try await Subprocess.run(
            .name("adb"),
            arguments: [
                "-s", ctx.serialNumber, "shell",
                "am", "start",
                "-n", "\(packageName)/\(launchableActivity)",
            ],
            output: .fileDescriptor(.standardOutput, closeAfterSpawningProcess: false),
            error: .fileDescriptor(.standardError, closeAfterSpawningProcess: false)
        )

        guard launchResult.terminationStatus.isSuccess else {
            throw CLIError.commandFailed(
                command: "adb shell am start",
                exitCode: Int32(launchResult.terminationStatus.exitCode),
                output: "Failed to launch app on device"
            )
        }

        output.yield(.started)
        output.finish()
    }

    // MARK: - Stop

    func stop(_ builtApp: ProviderBuiltApp) async throws {
        guard let ctx = builtApp.context as? AndroidBuildContext else { return }

        // Force-stop the app on device
        _ = try await Subprocess.run(
            .name("adb"),
            arguments: [
                "-s", ctx.serialNumber, "shell",
                "am", "force-stop", builtApp.appName,
            ],
            output: .discarded,
            error: .discarded
        )
    }

    /// Locate the aapt2 binary from the Android SDK.
    private func findAapt2() -> String? {
        let fm = FileManager.default

        // Check ANDROID_HOME or ANDROID_SDK_ROOT
        let sdkRoot =
            ProcessInfo.processInfo.environment["ANDROID_HOME"]
            ?? ProcessInfo.processInfo.environment["ANDROID_SDK_ROOT"]

        if let sdkRoot {
            let buildToolsDir = URL(fileURLWithPath: sdkRoot)
                .appendingPathComponent("build-tools")

            // Pick the latest build-tools version directory
            if let versions = try? fm.contentsOfDirectory(atPath: buildToolsDir.path)
                .sorted()
                .reversed()
            {
                for version in versions {
                    let aapt2 =
                        buildToolsDir
                        .appendingPathComponent(version)
                        .appendingPathComponent("aapt2")
                        .path
                    if fm.isExecutableFile(atPath: aapt2) {
                        return aapt2
                    }
                }
            }
        }

        // Try common SDK locations
        let home = fm.homeDirectoryForCurrentUser.path
        let commonPaths = [
            "\(home)/Library/Android/sdk",
            "\(home)/Android/Sdk",
            "/usr/local/lib/android/sdk",
        ]
        for base in commonPaths {
            let buildToolsDir = URL(fileURLWithPath: base)
                .appendingPathComponent("build-tools")
            if let versions = try? fm.contentsOfDirectory(atPath: buildToolsDir.path)
                .sorted()
                .reversed()
            {
                for version in versions {
                    let aapt2 =
                        buildToolsDir
                        .appendingPathComponent(version)
                        .appendingPathComponent("aapt2")
                        .path
                    if fm.isExecutableFile(atPath: aapt2) {
                        return aapt2
                    }
                }
            }
        }

        return nil
    }

    /// Extract the package name and launchable activity from an APK using aapt2.
    private func extractLaunchInfo(
        from apkPath: String
    ) async throws -> (packageName: String, activity: String) {
        guard let aapt2Path = findAapt2() else {
            throw CLIError.serviceNotInstalled(
                name: "aapt2 (Android build-tools). Set ANDROID_HOME to your SDK path"
            )
        }

        let result = try await Subprocess.run(
            .path(FilePath(aapt2Path)),
            arguments: ["dump", "badging", apkPath],
            output: .string(limit: 100_000),
            error: .discarded
        )

        guard result.terminationStatus.isSuccess, let output = result.standardOutput else {
            throw CLIError.commandFailed(
                command: "aapt2 dump badging",
                exitCode: Int32(result.terminationStatus.exitCode),
                output: "Failed to read APK metadata"
            )
        }

        var packageName: String?
        var activity: String?

        for line in output.split(separator: "\n") {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.hasPrefix("package:"),
                let nameStart = trimmed.range(of: "name='"),
                let nameEnd = trimmed[nameStart.upperBound...].range(of: "'")
            {
                packageName = String(trimmed[nameStart.upperBound..<nameEnd.lowerBound])
            } else if trimmed.hasPrefix("launchable-activity:"),
                let nameStart = trimmed.range(of: "name='"),
                let nameEnd = trimmed[nameStart.upperBound...].range(of: "'")
            {
                activity = String(trimmed[nameStart.upperBound..<nameEnd.lowerBound])
            }
        }

        guard let packageName, let activity else {
            throw CLIError.invalidArgument(
                name: "apk",
                value: apkPath,
                reason: "Could not determine package name or launchable activity from APK"
            )
        }

        return (packageName, activity)
    }
}

// MARK: - Helpers

extension TerminationStatus {
    fileprivate var exitCode: Int {
        switch self {
        case .exited(let code), .unhandledException(let code):
            return Int(code)
        }
    }
}
