import Dispatch
import Foundation
import Logging
import dnssd

struct BonjourAdvertiser {
    struct Runtime {
        let registration: BonjourRegistration
        let task: Task<Void, any Error>
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

final class BonjourRegistration: @unchecked Sendable {
    private let port: Int
    private let txtData: Data
    private let queue = DispatchQueue(label: "sh.wendy.agent.bonjour.registration")

    private var serviceRef: DNSServiceRef?
    private var readyContinuation: CheckedContinuation<Void, any Error>?
    private var shutdownContinuation: CheckedContinuation<Void, any Error>?
    private var hasRegistered = false
    private var isFinished = false
    private var completionError: (any Error)?

    init(port: Int, txtData: Data) {
        self.port = port
        self.txtData = txtData
    }

    func start() async throws {
        try await withCheckedThrowingContinuation { continuation in
            self.queue.async {
                self.startOnQueue(continuation: continuation)
            }
        }
    }

    func waitForShutdown() async throws {
        try await withCheckedThrowingContinuation { continuation in
            self.queue.async {
                if self.isFinished {
                    self.resume(continuation: continuation, with: self.completionError)
                } else {
                    precondition(self.shutdownContinuation == nil)
                    self.shutdownContinuation = continuation
                }
            }
        }
    }

    func shutdown() async {
        await withCheckedContinuation { continuation in
            self.queue.async {
                self.finishOnQueue(error: nil)
                continuation.resume()
            }
        }
    }

    private func startOnQueue(continuation: CheckedContinuation<Void, any Error>) {
        precondition(self.readyContinuation == nil)
        self.readyContinuation = continuation

        var serviceRef: DNSServiceRef?
        let error = self.txtData.withUnsafeBytes { buffer in
            DNSServiceRegister(
                &serviceRef,
                0,
                0,
                nil,
                "_wendyos._udp.",
                nil,
                nil,
                UInt16(self.port).bigEndian,
                UInt16(buffer.count),
                buffer.baseAddress,
                Self.handleRegistrationCallback,
                Unmanaged.passUnretained(self).toOpaque()
            )
        }

        guard error == kDNSServiceErr_NoError, let serviceRef else {
            self.readyContinuation = nil
            continuation.resume(throwing: BonjourError.registrationFailed(error))
            return
        }

        let queueError = DNSServiceSetDispatchQueue(serviceRef, self.queue)
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
            self.finishOnQueue(error: BonjourError.registrationFailed(errorCode))
            return
        }

        let hasAddFlag = (flags & DNSServiceFlags(kDNSServiceFlagsAdd)) != 0
        guard hasAddFlag else {
            self.finishOnQueue(error: BonjourError.registrationLost)
            return
        }

        guard !self.hasRegistered else { return }
        self.hasRegistered = true

        let continuation = self.readyContinuation
        self.readyContinuation = nil
        continuation?.resume(returning: ())
    }

    private func finishOnQueue(error: (any Error)?) {
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
        continuation: CheckedContinuation<Void, any Error>,
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
        registration.handleRegistrationCallback(flags: flags, errorCode: errorCode)
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
