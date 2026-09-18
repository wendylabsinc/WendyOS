import Foundation
import SystemExtensions
import os

nonisolated private let systemExtensionLog = Logger(
    subsystem: "sh.wendy.WendyAgentMac",
    category: "SystemExtension"
)

struct SystemExtensionVersion: Equatable {
    let shortVersion: String
    let bundleVersion: String
}

enum SystemExtensionInstallationResult: Equatable {
    case ready
    case needsApproval
}

private struct InstalledSystemExtension: Sendable {
    let bundleShortVersion: String
    let bundleVersion: String
    let isEnabled: Bool
    let isUninstalling: Bool
    let isAwaitingUserApproval: Bool
}

nonisolated func compareSystemExtensionVersions(
    _ lhs: SystemExtensionVersion,
    _ rhs: SystemExtensionVersion
) -> ComparisonResult {
    let shortVersionOrder = lhs.shortVersion.compare(rhs.shortVersion, options: .numeric)
    if shortVersionOrder != .orderedSame { return shortVersionOrder }
    return lhs.bundleVersion.compare(rhs.bundleVersion, options: .numeric)
}

@MainActor
final class MeshSystemExtensionInstaller: NSObject {
    static let shared = MeshSystemExtensionInstaller()

    static let extensionID = "sh.wendy.WendyAgentMac.WendyNet"

    private var operation: Task<SystemExtensionInstallationResult, any Error>?
    private var propertiesContinuation: CheckedContinuation<[InstalledSystemExtension], any Error>?
    private var activationContinuation: CheckedContinuation<Void, any Error>?
    private var approvalHandler: (() -> Void)?

    func installOrUpdate(
        onNeedsApproval: @escaping () -> Void = {}
    ) async throws -> SystemExtensionInstallationResult {
        try await runOnce {
            let properties = try await self.installedProperties()
                .filter { !$0.isUninstalling }
            if properties.contains(where: \.isAwaitingUserApproval) {
                onNeedsApproval()
                return .needsApproval
            }

            let bundledVersion = try self.bundledVersion()
            if properties.contains(where: {
                compareSystemExtensionVersions(
                    SystemExtensionVersion(
                        shortVersion: $0.bundleShortVersion,
                        bundleVersion: $0.bundleVersion
                    ),
                    bundledVersion
                ) != .orderedAscending
            }) {
                return .ready
            }

            try await self.activate(onNeedsApproval: onNeedsApproval)
            return .ready
        }
    }

    private func runOnce(
        _ body: @escaping @MainActor () async throws -> SystemExtensionInstallationResult
    ) async throws -> SystemExtensionInstallationResult {
        if let operation {
            return try await operation.value
        }

        let operation = Task { @MainActor in try await body() }
        self.operation = operation
        defer { self.operation = nil }
        return try await operation.value
    }

    private func installedProperties() async throws -> [InstalledSystemExtension] {
        try await withCheckedThrowingContinuation { continuation in
            propertiesContinuation = continuation
            let request = OSSystemExtensionRequest.propertiesRequest(
                forExtensionWithIdentifier: Self.extensionID,
                queue: .main
            )
            request.delegate = self
            OSSystemExtensionManager.shared.submitRequest(request)
        }
    }

    private func bundledVersion() throws -> SystemExtensionVersion {
        let infoURL = Bundle.main.bundleURL
            .appendingPathComponent(
                "Contents/Library/SystemExtensions/\(Self.extensionID).systemextension/Contents/Info.plist"
            )
        let data = try Data(contentsOf: infoURL)
        guard
            let info = try PropertyListSerialization.propertyList(
                from: data,
                options: [],
                format: nil
            ) as? [String: Any],
            let shortVersion = info["CFBundleShortVersionString"] as? String,
            let bundleVersion = info["CFBundleVersion"] as? String
        else {
            throw CocoaError(.fileReadCorruptFile)
        }
        return SystemExtensionVersion(
            shortVersion: shortVersion,
            bundleVersion: bundleVersion
        )
    }

    private func activate(onNeedsApproval: @escaping () -> Void) async throws {
        let systemExtensionsURL = Bundle.main.bundleURL
            .appendingPathComponent("Contents/Library/SystemExtensions", isDirectory: true)
        let contents =
            (try? FileManager.default.contentsOfDirectory(
                at: systemExtensionsURL,
                includingPropertiesForKeys: nil
            ))?.map(\.lastPathComponent) ?? []

        systemExtensionLog.notice(
            "activating id=\(Self.extensionID, privacy: .public) host=\(Bundle.main.bundlePath, privacy: .public) bundled=\(contents.joined(separator: ","), privacy: .public)"
        )

        approvalHandler = onNeedsApproval
        try await withCheckedThrowingContinuation { continuation in
            activationContinuation = continuation
            let request = OSSystemExtensionRequest.activationRequest(
                forExtensionWithIdentifier: Self.extensionID,
                queue: .main
            )
            request.delegate = self
            OSSystemExtensionManager.shared.submitRequest(request)
        }
        approvalHandler = nil
    }

    private func finishActivation(_ result: Result<Void, any Error>) {
        guard let continuation = activationContinuation else { return }
        activationContinuation = nil
        approvalHandler = nil
        continuation.resume(with: result)
    }

    private func finishProperties(_ result: Result<[InstalledSystemExtension], any Error>) {
        guard let continuation = propertiesContinuation else { return }
        propertiesContinuation = nil
        continuation.resume(with: result)
    }
}

extension MeshSystemExtensionInstaller: OSSystemExtensionRequestDelegate {
    nonisolated func request(
        _ request: OSSystemExtensionRequest,
        actionForReplacingExtension existing: OSSystemExtensionProperties,
        withExtension ext: OSSystemExtensionProperties
    ) -> OSSystemExtensionRequest.ReplacementAction {
        let existingVersion = SystemExtensionVersion(
            shortVersion: existing.bundleShortVersion,
            bundleVersion: existing.bundleVersion
        )
        let newVersion = SystemExtensionVersion(
            shortVersion: ext.bundleShortVersion,
            bundleVersion: ext.bundleVersion
        )
        return compareSystemExtensionVersions(newVersion, existingVersion) == .orderedDescending
            ? .replace
            : .cancel
    }

    nonisolated func requestNeedsUserApproval(_ request: OSSystemExtensionRequest) {
        Task { @MainActor in self.approvalHandler?() }
    }

    nonisolated func request(
        _ request: OSSystemExtensionRequest,
        didFinishWithResult result: OSSystemExtensionRequest.Result
    ) {
        Task { @MainActor in self.finishActivation(.success(())) }
    }

    nonisolated func request(_ request: OSSystemExtensionRequest, didFailWithError error: any Error)
    {
        let nsError = error as NSError
        systemExtensionLog.error(
            "request failed domain=\(nsError.domain, privacy: .public) code=\(nsError.code) description=\(nsError.localizedDescription, privacy: .public)"
        )
        Task { @MainActor in
            self.finishProperties(.failure(error))
            self.finishActivation(.failure(error))
        }
    }

    nonisolated func request(
        _ request: OSSystemExtensionRequest,
        foundProperties properties: [OSSystemExtensionProperties]
    ) {
        let snapshots = properties.map {
            InstalledSystemExtension(
                bundleShortVersion: $0.bundleShortVersion,
                bundleVersion: $0.bundleVersion,
                isEnabled: $0.isEnabled,
                isUninstalling: $0.isUninstalling,
                isAwaitingUserApproval: $0.isAwaitingUserApproval
            )
        }
        Task { @MainActor in self.finishProperties(.success(snapshots)) }
    }
}
