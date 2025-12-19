import AsyncHTTPClient
import Foundation
import NIOCore
import NIOFoundationCompat

#if canImport(FoundationNetworking)
    import FoundationNetworking
#endif

// MARK: - Data Models

/// Stability level for a device
public enum DeviceStability: String, Codable {
    case stable = "stable"
    case experimental = "experimental"
    case deprecated = "deprecated"
}

/// Represents device manifest information
public struct DeviceManifest: Codable {
    public struct VersionInfo: Codable {
        public let release_date: String
        public let path: String
        public let size_bytes: Int
        public let is_latest: Bool
    }

    public let device_id: String
    public let versions: [String: VersionInfo]
}

/// Represents the main manifest containing references to all device manifests
public struct MainManifest: Codable {
    public struct DeviceInfo: Codable {
        public let latest: String
        public let manifest_path: String
        public let stability: DeviceStability?
    }

    public let last_updated: String
    public let devices: [String: DeviceInfo]
}

/// Information about a device from the manifest
public struct DeviceInfo: Codable {
    public let name: String
    public let latestVersion: String
    public let latestNightlyVersion: String?
    public let stability: DeviceStability

    public init(
        name: String,
        latestVersion: String,
        latestNightlyVersion: String? = nil,
        stability: DeviceStability = .stable
    ) {
        self.name = name
        self.latestVersion = latestVersion
        self.latestNightlyVersion = latestNightlyVersion
        self.stability = stability
    }
}

// MARK: - Protocols

/// Protocol defining manifest management functionality
public protocol ManifestManaging: Sendable {
    /// Fetches the latest image information for a specific device
    /// - Parameters:
    ///   - deviceName: The name of the device
    ///   - nightly: If true, fetches the latest nightly build; otherwise fetches the latest stable release
    /// - Returns: The image URL, size, and version string
    func getLatestImageInfo(
        for deviceName: String,
        nightly: Bool
    ) async throws -> (url: URL, size: Int, version: String)

    /// Fetches all available devices from the manifest
    /// - Returns: Array of available device information
    func getAvailableDevices() async throws -> [DeviceInfo]
}

// MARK: - Implementations

/// Manages fetching and parsing device manifests from GCS
public final class ManifestManager: ManifestManaging {
    private let baseUrl: String

    public init(baseUrl: String) {
        self.baseUrl = baseUrl
    }

    /// Helper method to fetch JSON data using AsyncHTTPClient
    private func fetchData(from url: URL) async throws -> Data {
        #if os(Windows)
        return try await URLSession.shared.data(from: url).0
        #else
        let request = HTTPClientRequest(url: url.absoluteString)
        let response = try await HTTPClient.shared.execute(
            request,
            deadline: NIODeadline.now() + .seconds(60)
        )

        // Check for successful response
        guard response.status == .ok else {
            throw ManifestError.httpFailure(response.status.code)
        }

        // Collect response body (10MB limit)
        let body = try await response.body.collect(upTo: 10 * 1024 * 1024)
        return Data(buffer: body)
        #endif
    }

    public func getLatestImageInfo(
        for deviceName: String,
        nightly: Bool = false
    ) async throws -> (url: URL, size: Int, version: String) {
        // Fetch the main manifest
        let mainManifestUrl = URL(string: "\(baseUrl)/manifests/master.json")!
        let mainManifestData = try await fetchData(from: mainManifestUrl)
        let mainManifest = try JSONDecoder().decode(MainManifest.self, from: mainManifestData)

        // Find the device in the main manifest
        guard let deviceInfo = mainManifest.devices[deviceName] else {
            throw ManifestError.deviceNotFound(deviceName)
        }

        // Check if the device has a manifest path
        guard !deviceInfo.manifest_path.isEmpty else {
            throw ManifestError.noManifestForDevice(deviceName)
        }

        // Fetch the device-specific manifest
        let deviceManifestUrl = URL(string: "\(baseUrl)/\(deviceInfo.manifest_path)")!
        let deviceManifestData = try await fetchData(from: deviceManifestUrl)
        let deviceManifest = try JSONDecoder().decode(DeviceManifest.self, from: deviceManifestData)

        let versionInfo: DeviceManifest.VersionInfo
        let versionString: String

        if nightly {
            // Find the latest nightly build
            let nightlyVersions = deviceManifest.versions.filter { $0.key.contains("-nightly") }

            guard !nightlyVersions.isEmpty else {
                throw ManifestError.noNightlyVersion(deviceName)
            }

            // Sort by release date to find the most recent nightly
            let sortedNightlyVersions = nightlyVersions.sorted { lhs, rhs in
                // Parse release dates and compare
                let lhsDate =
                    ISO8601DateFormatter().date(from: lhs.value.release_date) ?? Date.distantPast
                let rhsDate =
                    ISO8601DateFormatter().date(from: rhs.value.release_date) ?? Date.distantPast
                return lhsDate > rhsDate
            }

            let latestNightly = sortedNightlyVersions.first!
            versionInfo = latestNightly.value
            versionString = latestNightly.key
        } else {
            // Find the latest stable version
            guard !deviceInfo.latest.isEmpty,
                let stableVersionInfo = deviceManifest.versions[deviceInfo.latest]
            else {
                throw ManifestError.noLatestVersion(deviceName)
            }
            versionInfo = stableVersionInfo
            versionString = deviceInfo.latest
        }

        // Get the image URL
        let imageUrl = URL(string: "\(baseUrl)/\(versionInfo.path)")!

        return (imageUrl, versionInfo.size_bytes, versionString)
    }

    public func getAvailableDevices() async throws -> [DeviceInfo] {
        // Fetch the main manifest
        let mainManifestUrl = URL(string: "\(baseUrl)/manifests/master.json")!
        let mainManifestData = try await fetchData(from: mainManifestUrl)
        let mainManifest = try JSONDecoder().decode(MainManifest.self, from: mainManifestData)

        // Fetch device manifests to get nightly versions
        var deviceInfos: [DeviceInfo] = []
        for (name, info) in mainManifest.devices {
            var latestNightlyVersion: String? = nil

            // Only fetch device manifest if it has a manifest path
            if !info.manifest_path.isEmpty {
                do {
                    let deviceManifestUrl = URL(string: "\(baseUrl)/\(info.manifest_path)")!
                    let deviceManifestData = try await fetchData(from: deviceManifestUrl)
                    let deviceManifest = try JSONDecoder().decode(
                        DeviceManifest.self,
                        from: deviceManifestData
                    )

                    // Find the latest nightly build
                    let nightlyVersions = deviceManifest.versions.filter {
                        $0.key.contains("-nightly")
                    }
                    if !nightlyVersions.isEmpty {
                        let sortedNightlyVersions = nightlyVersions.sorted { lhs, rhs in
                            let lhsDate =
                                ISO8601DateFormatter().date(from: lhs.value.release_date)
                                ?? Date.distantPast
                            let rhsDate =
                                ISO8601DateFormatter().date(from: rhs.value.release_date)
                                ?? Date.distantPast
                            return lhsDate > rhsDate
                        }
                        latestNightlyVersion = sortedNightlyVersions.first?.key
                    }
                } catch {
                    // If we can't fetch the device manifest, just skip the nightly version
                    latestNightlyVersion = nil
                }
            }

            deviceInfos.append(
                DeviceInfo(
                    name: name,
                    latestVersion: info.latest,
                    latestNightlyVersion: latestNightlyVersion,
                    stability: info.stability ?? .stable
                )
            )
        }

        return deviceInfos.sorted { $0.name < $1.name }
    }
}

// MARK: - Factory

/// Factory for creating ManifestManager instances
public enum ManifestManagerFactory {
    /// Creates and returns a default ManifestManager instance
    public static func createManifestManager(
        baseUrl: String = "https://storage.googleapis.com/wendyos-images-public"
    ) -> ManifestManaging {
        return ManifestManager(baseUrl: baseUrl)
    }
}

// MARK: - Errors

/// Errors related to manifest operations
public enum ManifestError: Error, LocalizedError {
    case deviceNotFound(String)
    case noManifestForDevice(String)
    case noLatestVersion(String)
    case noNightlyVersion(String)
    case httpFailure(UInt)

    public var errorDescription: String? {
        switch self {
        case .deviceNotFound(let deviceName):
            return "Device '\(deviceName)' not found in the manifest"
        case .noManifestForDevice(let deviceName):
            return "No manifest available for device '\(deviceName)'"
        case .noLatestVersion(let deviceName):
            return "No latest version found for device '\(deviceName)'"
        case .noNightlyVersion(let deviceName):
            return "No nightly version found for device '\(deviceName)'"
        case .httpFailure(let status):
            return "HTTP request failed with status code: '\(status)'"
        }
    }
}
