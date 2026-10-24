import Foundation

struct HubRepository: Identifiable { let id, name, detail, license: String; let downloads: Int64 }
struct HubModelFile: Identifiable {
    let name, quantization: String
    let bytes: Int64
    var unavailableReason: String = ""
    var id: String { name }
    var estimatedMemory: Int64 { bytes + bytes / 4 + 4 * 1_073_741_824 }
}
struct HubModelDetails { let revision, license: String; let files: [HubModelFile] }
struct ModelRunProgress { let model, progress, error: String; let done: Bool; let port: UInt32 }
enum ModelFit: Int { case unknown, comfortable, tight, tooLarge
    var title: String { switch self { case .unknown: "Capacity unknown"; case .comfortable: "Fits estimated budget"; case .tight: "Close to memory limit"; case .tooLarge: "Exceeds available capacity" } }
}
struct ModelHardware {
    let os, architecture, gpu, jetpack, warning: String
    let memoryTotal, memoryAvailable, diskFree, gpuTotal, gpuAvailable: Int64
    let diskKnown, cuda, sharedMemory: Bool
    var canInstall: Bool { os == "linux" && ["arm64", "aarch64", "amd64", "x86_64"].contains(architecture) }
    var suggestedAccelerator: UInt32 {
        guard cuda else { return 0 }
        if jetpack.hasPrefix("5") { return 2 }
        if jetpack.hasPrefix("6") { return 3 }
        return 1
    }
    var computeName: String {
        switch suggestedAccelerator {
        case 1: return "NVIDIA GPU"
        case 2: return "NVIDIA GPU · JetPack 5"
        case 3: return "NVIDIA GPU · JetPack 6"
        default: return "CPU"
        }
    }
    func budget(accelerator: UInt32) -> (total: Int64, available: Int64) {
        accelerator != 0 && !sharedMemory ? (gpuTotal, gpuAvailable) : (memoryTotal, memoryAvailable)
    }
    func fit(_ file: HubModelFile, accelerator: UInt32, install: Bool) -> ModelFit {
        let disk = file.bytes + (install ? 12 * 1_073_741_824 : 0)
        if diskKnown && disk > diskFree { return .tooLarge }
        let budget = budget(accelerator: accelerator)
        guard budget.total > 0 else { return .unknown }
        if file.estimatedMemory > budget.total || file.estimatedMemory > budget.available { return .tooLarge }
        if file.estimatedMemory > budget.available - budget.available / 5 { return .tight }
        return diskKnown ? .comfortable : .unknown
    }
}

extension GoClient {
    // Catalog and hardware reads must not queue behind a long-running stream or
    // prevent the user from selecting another device.
    func performModelRequest<T>(_ work: @escaping () throws -> T) async throws -> T {
        try await withCheckedThrowingContinuation { continuation in
            DispatchQueue.global(qos: .userInitiated).async { continuation.resume(with: Result { try work() }) }
        }
    }
    func searchHubModels(_ query: String) async throws -> [HubRepository] {
        try await performModelRequest {
            try Self.withCString(query) {
                guard let p = WDSearchModels($0) else { throw WendyError(message: "Missing model catalog.") }
                defer { WDModelSearchFree(p) }; try Self.check(p.pointee.status)
                return UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map {
                    HubRepository(id: Self.string($0.id), name: Self.string($0.name), detail: Self.string($0.description), license: Self.string($0.license), downloads: $0.downloads)
                }
            }
        }
    }
    func hubModelDetails(_ repo: String) async throws -> HubModelDetails {
        try await performModelRequest {
            try Self.withCString(repo) {
                guard let p = WDModelFiles($0) else { throw WendyError(message: "Missing model files.") }
                defer { WDModelFilesFree(p) }; try Self.check(p.pointee.status)
                let files = UnsafeBufferPointer(start: p.pointee.items, count: p.pointee.count).map { HubModelFile(name: Self.string($0.name), quantization: Self.string($0.quantization), bytes: $0.bytes, unavailableReason: Self.string($0.unavailable_reason)) }
                return HubModelDetails(revision: Self.string(p.pointee.revision), license: Self.string(p.pointee.license), files: files)
            }
        }
    }
    func modelHardware(session: UInt64) async throws -> ModelHardware {
        try await performModelRequest {
            guard let p = WDModelHardware(session) else { throw WendyError(message: "Missing device hardware.") }
            defer { WDModelHardwareFree(p) }; try Self.check(p.pointee.status)
            let h = p.pointee
            return ModelHardware(os: Self.string(h.os), architecture: Self.string(h.architecture), gpu: Self.string(h.gpu), jetpack: Self.string(h.jetpack), warning: Self.string(h.warning), memoryTotal: h.memory_total, memoryAvailable: h.memory_available, diskFree: h.disk_free, gpuTotal: h.gpu_total, gpuAvailable: h.gpu_available, diskKnown: h.disk_known != 0, cuda: h.cuda != 0, sharedMemory: h.shared_memory != 0)
        }
    }
    func runModel(session: UInt64, job: UInt64, port: UInt32, accelerator: UInt32, repo: String, file: String, revision: String, install: Bool, acceptWarning: Bool) async throws {
        try await performModelRequest {
            let strings = CStringStorage(); defer { withExtendedLifetime(strings) {} }
            try Self.finish(WDRunModel(session, job, port, WDModelAccelerator(rawValue: accelerator), strings.add(repo), strings.add(file), strings.add(revision), install ? 1 : 0, acceptWarning ? 1 : 0))
        }
    }
    func modelRunStatus(session: UInt64, job: UInt64) async throws -> ModelRunProgress {
        try await performModelRequest {
            guard let p = WDModelRunStatus(session, job) else { throw WendyError(message: "Missing download status.") }
            defer { WDModelRunFree(p) }; try Self.check(p.pointee.status)
            return ModelRunProgress(model: Self.string(p.pointee.model), progress: Self.string(p.pointee.progress), error: Self.string(p.pointee.error), done: p.pointee.done != 0, port: p.pointee.port)
        }
    }
    func cancelModelRun(session: UInt64, job: UInt64) async throws { try await performModelRequest { try Self.finish(WDCancelModelRun(session, job)) } }
}
