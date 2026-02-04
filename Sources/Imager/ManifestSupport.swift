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

        public var date: Date? {
            ISO8601DateFormatter().date(from: release_date)
        }
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
    public let latestVersionReleaseDate: Date?
    public let latestNightlyReleaseDate: Date?
    public let latestVersionPath: String?
    public let latestNightlyPath: String?
    public let stability: DeviceStability

    public init(
        name: String,
        latestVersion: String,
        latestNightlyVersion: String? = nil,
        latestVersionReleaseDate: Date? = nil,
        latestNightlyReleaseDate: Date? = nil,
        latestVersionPath: String? = nil,
        latestNightlyPath: String? = nil,
        stability: DeviceStability = .stable
    ) {
        self.name = name
        self.latestVersion = latestVersion
        self.latestNightlyVersion = latestNightlyVersion
        self.latestVersionReleaseDate = latestVersionReleaseDate
        self.latestNightlyReleaseDate = latestNightlyReleaseDate
        self.latestVersionPath = latestVersionPath
        self.latestNightlyPath = latestNightlyPath
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
    /// - Returns: The image URL, size, version string, and release date (nil if date couldn't be parsed)
    func getLatestImageInfo(
        for deviceName: String,
        nightly: Bool
    ) async throws -> (url: URL, size: Int, version: String, releaseDate: Date?)

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

    /// Compares two semantic version strings (e.g., "0.9.10-nightly" vs "0.10.0-nightly")
    /// Returns true if lhs > rhs (for descending sort)
    private func compareSemanticVersions(_ lhs: String, _ rhs: String) -> Bool {
        // Extract numeric version parts before any suffix (handles "-nightly", "-rc1-nightly", etc.)
        func extractNumericVersion(_ version: String) -> [Int] {
            // Take everything before the first "-" as the base version
            let baseVersion = version.split(separator: "-").first.map(String.init) ?? version

            // Split by "." and parse each component as an integer
            return baseVersion.split(separator: ".").compactMap { Int($0) }
        }

        let lhsParts = extractNumericVersion(lhs)
        let rhsParts = extractNumericVersion(rhs)

        // Compare each version component
        let maxLength = max(lhsParts.count, rhsParts.count)
        for i in 0..<maxLength {
            let lhsComponent = i < lhsParts.count ? lhsParts[i] : 0
            let rhsComponent = i < rhsParts.count ? rhsParts[i] : 0

            if lhsComponent != rhsComponent {
                return lhsComponent > rhsComponent
            }
        }

        // All numeric components are equal, use lexicographic comparison as final tiebreaker
        // This ensures deterministic sorting even for unparseable or equal versions
        return lhs > rhs
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
    ) async throws -> (url: URL, size: Int, version: String, releaseDate: Date?) {
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

            // Sort by release date first, then by semantic version as a tiebreaker
            let sortedNightlyVersions = nightlyVersions.sorted { lhs, rhs in
                if let lhsDate = lhs.value.date, let rhsDate = rhs.value.date, lhsDate != rhsDate {
                    return lhsDate > rhsDate
                }

                // Dates are equal or unparseable, use semantic version as tiebreaker
                return self.compareSemanticVersions(lhs.key, rhs.key)
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

        return (imageUrl, versionInfo.size_bytes, versionString, versionInfo.date)
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
            var latestNightlyReleaseDate: Date? = nil
            var latestVersionReleaseDate: Date? = nil
            var latestNightlyPath: String? = nil
            var latestVersionPath: String? = nil

            // Only fetch device manifest if it has a manifest path
            if !info.manifest_path.isEmpty {
                do {
                    let deviceManifestUrl = URL(string: "\(baseUrl)/\(info.manifest_path)")!
                    let deviceManifestData = try await fetchData(from: deviceManifestUrl)
                    let deviceManifest = try JSONDecoder().decode(
                        DeviceManifest.self,
                        from: deviceManifestData
                    )

                    // Capture stable release date (if available).
                    if !info.latest.isEmpty,
                        let stableVersion = deviceManifest.versions[info.latest]
                    {
                        latestVersionReleaseDate = stableVersion.date
                        latestVersionPath = stableVersion.path
                    }

                    // Find the latest nightly build
                    let nightlyVersions = deviceManifest.versions.filter {
                        $0.key.contains("-nightly")
                    }
                    if !nightlyVersions.isEmpty {
                        let sortedNightlyVersions = nightlyVersions.sorted { lhs, rhs in
                            if let lhsDate = lhs.value.date, let rhsDate = rhs.value.date,
                                lhsDate != rhsDate
                            {
                                return lhsDate > rhsDate
                            }

                            // Dates are equal or unparseable, use semantic version as tiebreaker
                            return self.compareSemanticVersions(lhs.key, rhs.key)
                        }
                        if let latestNightly = sortedNightlyVersions.first {
                            latestNightlyVersion = latestNightly.key
                            latestNightlyReleaseDate = latestNightly.value.date
                            latestNightlyPath = latestNightly.value.path
                        }
                    }
                } catch {
                    // If we can't fetch the device manifest, just skip the nightly version
                    latestNightlyVersion = nil
                    latestNightlyReleaseDate = nil
                    latestVersionReleaseDate = nil
                    latestNightlyPath = nil
                    latestVersionPath = nil
                }
            }

            deviceInfos.append(
                DeviceInfo(
                    name: name,
                    latestVersion: info.latest,
                    latestNightlyVersion: latestNightlyVersion,
                    latestVersionReleaseDate: latestVersionReleaseDate,
                    latestNightlyReleaseDate: latestNightlyReleaseDate,
                    latestVersionPath: latestVersionPath,
                    latestNightlyPath: latestNightlyPath,
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
