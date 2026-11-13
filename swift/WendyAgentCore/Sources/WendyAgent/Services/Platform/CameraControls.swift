import Darwin
import Dispatch
import Foundation
import Logging

/// Blocking USB requests run on a dedicated serial queue, not a gRPC event
/// loop or the cooperative pool. No device handles leave this executor.
final class CameraControlExecutor: SerialExecutor {
    private let queue = DispatchQueue(label: "sh.wendy.agent.camera-controls")
    func enqueue(_ job: consuming ExecutorJob) {
        let job = UnownedJob(job)
        let executor = asUnownedSerialExecutor()
        queue.async { unsafe job.runSynchronously(on: executor) }
    }
}

struct CameraControlOutcome: Sendable {
    let name: String
    var applied: Bool
    var detail: String
}

protocol CameraControlManaging: Sendable {
    func list(uniqueID: String) async throws -> [UVCCameraControl]
    func set(
        uniqueID: String,
        values: [(String, Int32)],
        persist: Bool
    ) async throws -> [CameraControlOutcome]
    func reset(uniqueID: String, names: [String]) async throws -> [CameraControlOutcome]
    func restore(uniqueID: String) async throws
}

actor CameraControls: CameraControlManaging {
    private nonisolated let executor = CameraControlExecutor()
    nonisolated var unownedExecutor: UnownedSerialExecutor { executor.asUnownedSerialExecutor() }
    private let open: @Sendable (UVCIdentity) throws -> any UVCControlTransport
    private let storeURL: URL?
    private let logger = Logger(label: "sh.wendy.agent.camera-controls")
    private var saved: [String: [String: Int32]]?

    init(
        storeURL: URL? = FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(
                "Library/Application Support/sh.wendy.WendyAgentMac/camera-controls/controls.json"
            ),
        open: @escaping @Sendable (UVCIdentity) throws -> any UVCControlTransport = {
            try IOKitUVCTransport(identity: $0)
        }
    ) {
        self.storeURL = storeURL
        self.open = open
    }

    func list(uniqueID: String) throws -> [UVCCameraControl] {
        let session = try session(uniqueID)
        var result: [UVCCameraControl] = []
        for definition in session.definitions {
            do { result.append(try session.read(definition)) } catch {
                logger.debug(
                    "Camera control metadata unavailable",
                    metadata: ["control": "\(definition.name)", "error": "\(error)"]
                )
            }
        }
        guard !result.isEmpty else {
            throw UVCError.unsupported(
                "This camera exposes no readable standard scalar UVC controls"
            )
        }
        return result
    }

    func set(
        uniqueID: String,
        values: [(String, Int32)],
        persist: Bool
    ) throws -> [CameraControlOutcome] {
        guard !values.isEmpty, values.count <= 64, Set(values.map(\.0)).count == values.count else {
            throw UVCError.invalidValue("Provide 1–64 distinct controls")
        }
        try load()
        let session = try session(uniqueID)
        var results = apply(values, session: session)
        if persist {
            var next = saved ?? [:]
            for result in results where result.applied {
                next[uniqueID, default: [:]][result.name] = values.first { $0.0 == result.name }!.1
            }
            do { try save(next) } catch {
                for index in results.indices where results[index].applied {
                    results[index].applied = false
                    results[index].detail = "Applied on hardware, but persistence failed: \(error)"
                }
            }
        }
        return results
    }

    func reset(uniqueID: String, names: [String]) throws -> [CameraControlOutcome] {
        try load()
        let names = names.isEmpty ? Array((saved?[uniqueID] ?? [:]).keys).sorted() : names
        guard names.count <= 64, Set(names).count == names.count else {
            throw UVCError.invalidValue("Provide at most 64 distinct controls")
        }
        guard !names.isEmpty else { return [] }
        // Forget before touching hardware, even if a disconnected/inactive
        // control cannot be restored. Never reassert a value the user reset.
        var next = saved ?? [:]
        for name in names { next[uniqueID]?.removeValue(forKey: name) }
        if next[uniqueID]?.isEmpty == true { next.removeValue(forKey: uniqueID) }
        try save(next)
        let session = try session(uniqueID)
        var defaults: [(String, Int32)] = []
        var results: [CameraControlOutcome] = []
        for name in names {
            do {
                guard let definition = session.definitions.first(where: { $0.name == name }) else {
                    throw UVCError.unsupported("This camera has no control by that name")
                }
                defaults.append((name, try session.read(definition).defaultValue))
            } catch {
                results.append(CameraControlOutcome(name: name, applied: false, detail: "\(error)"))
            }
        }
        // Restore dependent defaults while the current manual modes still
        // permit writes, then restore automatic-mode defaults LAST. A readable
        // inactive control already at its own default needs no USB write.
        return results
            + UVCControlSession.ordered(defaults).reversed().map { name, value in
                do {
                    guard let definition = session.definitions.first(where: { $0.name == name })
                    else {
                        throw UVCError.unsupported("This camera has no control by that name")
                    }
                    if try session.read(definition).value == value {
                        return CameraControlOutcome(
                            name: name,
                            applied: true,
                            detail: "Already at camera default; override forgotten"
                        )
                    }
                    try session.write(name: name, value: value)
                    return CameraControlOutcome(name: name, applied: true, detail: "")
                } catch {
                    return CameraControlOutcome(name: name, applied: false, detail: "\(error)")
                }
            }
    }

    func restore(uniqueID: String) throws {
        try load()
        let values = (saved?[uniqueID] ?? [:]).sorted { $0.key < $1.key }.map { ($0.key, $0.value) }
        guard !values.isEmpty else { return }  // no USB open for untuned cameras
        let outcomes = apply(values, session: try session(uniqueID))
        for result in outcomes where !result.applied {
            logger.warning(
                "Persisted camera control could not be restored",
                metadata: ["control": "\(result.name)", "detail": "\(result.detail)"]
            )
        }
    }

    private func session(_ uniqueID: String) throws -> UVCControlSession {
        try UVCControlSession(transport: open(UVCIdentity(uniqueID: uniqueID)))
    }
    private func apply(
        _ values: [(String, Int32)],
        session: UVCControlSession
    ) -> [CameraControlOutcome] {
        UVCControlSession.ordered(values).map { name, value in
            do {
                try session.write(name: name, value: value)
                return CameraControlOutcome(name: name, applied: true, detail: "")
            } catch { return CameraControlOutcome(name: name, applied: false, detail: "\(error)") }
        }
    }

    private func load() throws {
        guard saved == nil else { return }
        guard let storeURL else {
            saved = [:]
            return
        }
        let fd = unsafe openStore(storeURL.path)
        if fd < 0 {
            if errno == ENOENT {
                saved = [:]
                return
            }
            throw UVCError.invalidValue("Cannot safely open camera-control store")
        }
        defer { _ = Darwin.close(fd) }
        var info = stat()
        guard unsafe fstat(fd, &info) == 0, info.st_mode & S_IFMT == S_IFREG,
            info.st_uid == getuid(), info.st_mode & 0o077 == 0, info.st_size <= 65536
        else { throw UVCError.invalidValue("Unsafe camera-control store ownership, mode or size") }
        let handle = FileHandle(fileDescriptor: fd, closeOnDealloc: false)
        let data = try handle.read(upToCount: 65537) ?? Data()
        guard data.count <= 65536 else {
            throw UVCError.invalidValue("Camera-control store is too large")
        }
        let value = try JSONDecoder().decode([String: [String: Int32]].self, from: data)
        guard value.count <= 64, value.values.allSatisfy({ $0.count <= 64 }) else {
            throw UVCError.invalidValue("Camera-control store is too large")
        }
        saved = value
    }
    private func save(_ next: [String: [String: Int32]]) throws {
        guard next.count <= 64, next.values.allSatisfy({ $0.count <= 64 }) else {
            throw UVCError.invalidValue("Camera-control store is too large")
        }
        if let storeURL {
            let data = try JSONEncoder().encode(next)
            guard data.count <= 65536 else {
                throw UVCError.invalidValue("Camera-control store is too large")
            }
            try FileManager.default.createDirectory(
                at: storeURL.deletingLastPathComponent(),
                withIntermediateDirectories: true,
                attributes: [.posixPermissions: 0o700]
            )
            try data.write(to: storeURL, options: .atomic)
            try FileManager.default.setAttributes(
                [.posixPermissions: 0o600],
                ofItemAtPath: storeURL.path
            )
        }
        saved = next
    }
}

private func openStore(_ path: String) -> Int32 {
    unsafe Darwin.open(path, O_RDONLY | O_CLOEXEC | O_NOFOLLOW)
}
