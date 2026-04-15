import dnssd
import Foundation
import Logging
import ServiceLifecycle

struct BonjourAdvertiser: Service {
    let port: Int
    let displayName: String
    let deviceID: String

    private let logger = Logger(label: "sh.wendy.agent.bonjour")

    func run() async throws {
        let txtFields = ["displayname=\(displayName)", "id=\(deviceID)"]
        let txtData = txtFields.reduce(into: Data()) { data, field in
            data.append(UInt8(field.utf8.count))
            data.append(contentsOf: field.utf8)
        }

        var serviceRef: DNSServiceRef?
        let err = txtData.withUnsafeBytes { buf in
            DNSServiceRegister(
                &serviceRef,
                0,
                0,
                nil,
                "_wendyos._udp.",
                nil,
                nil,
                UInt16(port).bigEndian,
                UInt16(buf.count),
                buf.baseAddress,
                nil,
                nil
            )
        }

        guard err == kDNSServiceErr_NoError, let serviceRef else {
            throw BonjourError.registrationFailed(err)
        }

        logger.info("Advertising \(displayName) as _wendyos._udp on port \(port)")

        defer { DNSServiceRefDeallocate(serviceRef) }
        try await gracefulShutdown()
    }
}

enum BonjourError: Error {
    case registrationFailed(DNSServiceErrorType)
}
