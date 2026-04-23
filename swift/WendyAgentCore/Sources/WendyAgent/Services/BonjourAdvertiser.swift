import Dispatch
import Foundation
import Logging
import dnssd

struct BonjourAdvertiser {
    struct Runtime {
        let registration: BonjourRegistration
        let task: Task<Void, Error>
    }

    let port: Int
    let displayName: String
    let deviceID: String

    private let logger = Logger(label: "sh.wendy.agent.bonjour")

    func start() async throws -> Runtime {
        let registration = BonjourRegistration(port: self.port, txtData: self.txtData)
        try await registration.start()

        self.logger.info("Advertising \(self.displayName) as _wendyos._udp on port \(self.port)")

        let task = Task {
            try await registration.waitForShutdown()
        }

        return Runtime(registration: registration, task: task)
    }

    private var txtData: Data {
        let txtFields = ["displayname=\(self.displayName)", "id=\(self.deviceID)"]
        return txtFields.reduce(into: Data()) { data, field in
            data.append(UInt8(field.utf8.count))
            data.append(contentsOf: field.utf8)
        }
    }
}

actor BonjourRegistration {
    nonisolated var unownedExecutor: UnownedSerialExecutor {
        BonjourRegistrationActor.shared.unownedExecutor
    }

    private let port: Int
    private let txtData: Data

    private var serviceRef: DNSServiceRef?
    private var readyContinuation: CheckedContinuation<Void, Error>?
    private var shutdownContinuation: CheckedContinuation<Void, Error>?
    private var hasRegistered = false
    private var isFinished = false
    private var completionError: (any Error)?

    init(port: Int, txtData: Data) {
        self.port = port
        self.txtData = txtData
    }

    func start() async throws {
        try await withCheckedThrowingContinuation { continuation in
            self.beginStart(continuation: continuation)
        }
    }

    func waitForShutdown() async throws {
        try await withCheckedThrowingContinuation { continuation in
            if self.isFinished {
                self.resume(continuation: continuation, with: self.completionError)
            } else {
                precondition(self.shutdownContinuation == nil)
                self.shutdownContinuation = continuation
            }
        }
    }

    func shutdown() async {
        self.finish(error: nil)
    }

    private func beginStart(continuation: CheckedContinuation<Void, Error>) {
        precondition(self.readyContinuation == nil)
        self.readyContinuation = continuation

        let port = self.port
        let txtData = self.txtData
        // BonjourAdvertiser.Runtime keeps this actor alive until shutdown has
        // finished, so the DNS-SD callback context can borrow rather than
        // retain it.
        let context = Unmanaged.passUnretained(self).toOpaque()

        var serviceRef: DNSServiceRef?
        let error = txtData.withUnsafeBytes { buffer in
            DNSServiceRegister(
                &serviceRef,
                0,
                0,
                nil,
                "_wendyos._udp.",
                nil,
                nil,
                UInt16(port).bigEndian,
                UInt16(buffer.count),
                buffer.baseAddress,
                Self.handleRegistrationCallback,
                context
            )
        }

        guard error == kDNSServiceErr_NoError, let serviceRef else {
            self.readyContinuation = nil
            continuation.resume(throwing: BonjourError.registrationFailed(error))
            return
        }

        let queueError = DNSServiceSetDispatchQueue(
            serviceRef,
            BonjourRegistrationActor.dispatchQueue
        )
        guard queueError == kDNSServiceErr_NoError else {
            DNSServiceRefDeallocate(serviceRef)
            self.readyContinuation = nil
            continuation.resume(throwing: BonjourError.registrationFailed(queueError))
            return
        }

        self.serviceRef = serviceRef
    }

    private func handleRegistrationCallback(
        flags: DNSServiceFlags,
        errorCode: DNSServiceErrorType
    ) {
        if errorCode != kDNSServiceErr_NoError {
            self.finish(error: BonjourError.registrationFailed(errorCode))
            return
        }

        let hasAddFlag = (flags & DNSServiceFlags(kDNSServiceFlagsAdd)) != 0
        guard hasAddFlag else {
            self.finish(error: BonjourError.registrationLost)
            return
        }

        guard !self.hasRegistered else { return }
        self.hasRegistered = true

        let continuation = self.readyContinuation
        self.readyContinuation = nil
        continuation?.resume(returning: ())
    }

    private func finish(error: (any Error)?) {
        guard !self.isFinished else { return }

        self.isFinished = true
        self.completionError = error

        if let serviceRef = self.serviceRef {
            DNSServiceRefDeallocate(serviceRef)
            self.serviceRef = nil
        }

        if let continuation = self.readyContinuation {
            self.readyContinuation = nil
            self.resume(continuation: continuation, with: error)
        }

        if let continuation = self.shutdownContinuation {
            self.shutdownContinuation = nil
            self.resume(continuation: continuation, with: error)
        }
    }

    private func resume(
        continuation: CheckedContinuation<Void, Error>,
        with error: (any Error)?
    ) {
        if let error {
            continuation.resume(throwing: error)
        } else {
            continuation.resume(returning: ())
        }
    }

    private static let handleRegistrationCallback: DNSServiceRegisterReply = {
        serviceRef,
        flags,
        errorCode,
        _,
        _,
        _,
        context in
        guard let context else { return }

        let registration = Unmanaged<BonjourRegistration>
            .fromOpaque(context)
            .takeUnretainedValue()

        Task { @BonjourRegistrationActor in
            await registration.handleRegistrationCallback(flags: flags, errorCode: errorCode)
        }
    }
}

enum BonjourError: Error {
    case registrationFailed(DNSServiceErrorType)
    case registrationLost
}

extension BonjourError: LocalizedError {
    var errorDescription: String? {
        switch self {
        case .registrationFailed(let code):
            return "Bonjour registration failed (DNS-SD error \(code))."
        case .registrationLost:
            return "Bonjour registration stopped unexpectedly."
        }
    }
}
