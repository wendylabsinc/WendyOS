import CryptoKit
import Darwin
import Foundation
import Security

public enum AnnotationClockError: Error, LocalizedError {
    case unavailable, invalidProof
    public var errorDescription: String? { "Verified gateway-relative time is unavailable. Text can be saved untimed; it will not be dated automatically on reconnect." }
}

public struct AnnotationClockPins: Codable, Sendable, Equatable {
    let rootPublicKey: Data
    let gatewayPrincipal: String
    let gatewayKeySHA256: String
    public let runID: String
    let clockDomain: String
    public init(json: JSONValue) throws {
        guard let fields = json.object, Set(fields.keys) == ["root_public_key", "gateway_principal", "gateway_key_sha256", "run_id", "clock_domain"],
              let root = json["root_public_key"]?.string.flatMap({ Data(base64Encoded: $0) }), root.count == 32,
              let principal = json["gateway_principal"]?.string, !principal.isEmpty, principal.utf8.count <= 256,
              let key = json["gateway_key_sha256"]?.string, WorldClient.isDigest(key),
              let run = json["run_id"]?.string, !run.isEmpty, run.utf8.count <= 245,
              json["clock_domain"]?.string == "simulation:" + run else { throw AnnotationClockError.invalidProof }
        rootPublicKey = root; gatewayPrincipal = principal; gatewayKeySHA256 = key; runID = run; clockDomain = "simulation:" + run
    }
}

public struct AnnotationClockReading: Sendable, Equatable {
    public let bootID: String
    public let nanoseconds: Int64
    public init(bootID: String, nanoseconds: Int64) { self.bootID = bootID; self.nanoseconds = nanoseconds }
}

/// mach_continuous_time advances during sleep (Apple mach_time.h). The kernel
/// boot-session UUID is never inferred from wall time. Sandboxes without that
/// sysctl get a process-only identity, so persisted anchors cannot cross launch.
public enum AnnotationMonotonicClock {
    private static let processID = "process:" + UUID().uuidString.lowercased()
    public static func read() throws -> AnnotationClockReading {
        var info = mach_timebase_info_data_t()
        guard mach_timebase_info(&info) == KERN_SUCCESS, info.denom > 0 else { throw AnnotationClockError.unavailable }
        let ticks = mach_continuous_time()
        let product = ticks.multipliedFullWidth(by: UInt64(info.numer))
        guard product.high < UInt64(info.denom) else { throw AnnotationClockError.unavailable }
        let nanos = UInt64(info.denom).dividingFullWidth(product).quotient
        guard nanos <= Int64.max else { throw AnnotationClockError.unavailable }
        var size = 0
        var identity = processID
        if sysctlbyname("kern.bootsessionuuid", nil, &size, nil, 0) == 0, size > 1, size <= 64 {
            var bytes = [CChar](repeating: 0, count: size)
            if sysctlbyname("kern.bootsessionuuid", &bytes, &size, nil, 0) == 0,
               let end = bytes.firstIndex(of: 0), let value = String(bytes: bytes[..<end].map { UInt8(bitPattern: $0) }, encoding: .utf8),
               let uuid = UUID(uuidString: value) { identity = "boot:" + uuid.uuidString.lowercased() }
        }
        return AnnotationClockReading(bootID: identity, nanoseconds: Int64(nanos))
    }
}

public struct AnnotationTimeInterval: Codable, Sendable, Equatable {
    public let lowerUnixMS: Int64
    public let upperUnixMS: Int64
}

public struct AnnotationClockEvidence: Codable, Sendable, Equatable {
    public let request: JSONValue
    public let observation: JSONValue
    public let rttNS: Int64
    public let elapsedNS: Int64
    enum CodingKeys: String, CodingKey { case request, observation; case rttNS = "rtt_ns", elapsedNS = "elapsed_ns" }
    public func interval() throws -> AnnotationTimeInterval {
        guard rttNS >= 0, rttNS <= 2_000_000_000, elapsedNS >= 0, elapsedNS <= 3_600_000_000_000,
              let sample = observation["observation"]?["sample_unix_ms"]?.int, sample > 0, sample <= 253_402_214_400_000 else { throw AnnotationClockError.invalidProof }
        let drift = ((rttNS + elapsedNS) * 100 + 999_999) / 1_000_000
        func ceilMS(_ value: Int64) -> Int64 { (value + 999_999) / 1_000_000 }
        return AnnotationTimeInterval(lowerUnixMS: sample + elapsedNS / 1_000_000 - ceilMS(drift) - 1,
                                      upperUnixMS: sample + ceilMS(rttNS + elapsedNS) + ceilMS(drift) + 1)
    }
    func canonical() throws -> Data {
        GoAuthorityJSON.object([("request", try ClockWire.signed(request, message: "request", keys: ClockWire.requestKeys)),
            ("observation", try ClockWire.signed(observation, message: "observation", keys: ClockWire.observationKeys)),
            ("rtt_ns", Data(String(rttNS).utf8)), ("elapsed_ns", Data(String(elapsedNS).utf8))])
    }
    func verify(credentials: WorldCredentials) throws {
        guard let pins = credentials.clockPins else { throw AnnotationClockError.unavailable }
        let req = request["request"], obs = observation["observation"]
        guard request["version"]?.int == 1, observation["version"]?.int == 1,
              let sample = obs?["sample_unix_ms"]?.int,
              let nonce = req?["nonce"]?.string, WorldClient.isDigest(nonce),
              obs?["request_sha256"]?.string == WorldCredentials.digest(try ClockWire.signed(request, message: "request", keys: ClockWire.requestKeys)),
              obs?["nonce"] == req?["nonce"],
              obs?["operator_principal"]?.string == credentials.principal,
              obs?["operator_key_sha256"]?.string == WorldCredentials.digest(credentials.key.publicKey.rawRepresentation),
              obs?["gateway_principal"]?.string == pins.gatewayPrincipal,
              let boot = obs?["gateway_boot_id"]?.string, WorldClient.isDigest(boot),
              obs?["reference"]?.string == "simulation-gateway-relative-v1",
              obs?["max_rtt_ms"]?.int == 2000, obs?["max_holdover_ms"]?.int == 3_600_000,
              obs?["drift_ppm"]?.int == 100, obs?["quantization_ms"]?.int == 1 else { throw AnnotationClockError.invalidProof }
        for (field, expected) in [("tenant", credentials.tenant), ("site", credentials.site), ("audience", credentials.audience), ("run_id", pins.runID), ("clock_domain", pins.clockDomain)] {
            guard req?[field]?.string == expected, obs?[field]?.string == expected else { throw AnnotationClockError.invalidProof }
        }
        let operatorKey = try ClockWire.verifyGrant(request["grant"], pins: pins, credentials: credentials, principal: credentials.principal, kind: "operator", scope: "annotation-write", sample: sample)
        guard operatorKey.rawRepresentation == credentials.key.publicKey.rawRepresentation,
              request["grant"]?["grant"]?["source"] == credentials.grant["grant"]?["source"] else { throw AnnotationClockError.invalidProof }
        let gatewayKey = try ClockWire.verifyGrant(observation["grant"], pins: pins, credentials: credentials, principal: pins.gatewayPrincipal, kind: "device", scope: "annotation-clock-server", sample: sample)
        guard WorldCredentials.digest(gatewayKey.rawRepresentation) == pins.gatewayKeySHA256 else { throw AnnotationClockError.invalidProof }
        try ClockWire.verifySigned(request, message: "request", keys: ClockWire.requestKeys, key: operatorKey)
        try ClockWire.verifySigned(observation, message: "observation", keys: ClockWire.observationKeys, key: gatewayKey)
        _ = try interval()
    }
}

public struct AnnotationClockAnchor: Codable, Sendable, Equatable {
    public let evidence: AnnotationClockEvidence
    public let bootID: String
    public let receivedNS: Int64
    public func at(_ reading: AnnotationClockReading, credentials: WorldCredentials) throws -> AnnotationClockEvidence {
        guard receivedNS >= 0, reading.nanoseconds >= 0, evidence.elapsedNS == 0, !bootID.isEmpty, bootID.utf8.count <= 128, reading.bootID == bootID, reading.nanoseconds >= receivedNS else { throw AnnotationClockError.unavailable }
        let current = AnnotationClockEvidence(request: evidence.request, observation: evidence.observation, rttNS: evidence.rttNS, elapsedNS: reading.nanoseconds - receivedNS)
        try current.verify(credentials: credentials)
        return current
    }
}

enum ClockWire {
    static let requestKeys = ["tenant", "site", "audience", "run_id", "clock_domain", "nonce"]
    static let observationKeys = ["request_sha256", "tenant", "site", "audience", "run_id", "clock_domain", "nonce", "operator_principal", "operator_key_sha256", "gateway_principal", "gateway_boot_id", "sample_unix_ms", "reference", "max_rtt_ms", "max_holdover_ms", "drift_ppm", "quantization_ms"]
    static func message(_ value: JSONValue?, keys: [String]) throws -> Data {
        guard let fields = value?.object, Set(fields.keys) == Set(keys) else { throw AnnotationClockError.invalidProof }
        return try GoAuthorityJSON.object(keys.map { key in
            if let text = fields[key]?.string { return (key, GoAuthorityJSON.string(text)) }
            if let number = fields[key]?.int { return (key, Data(String(number).utf8)) }
            throw AnnotationClockError.invalidProof
        })
    }
    static func signed(_ value: JSONValue, message name: String, keys: [String]) throws -> Data {
        guard let fields = value.object, Set(fields.keys) == ["version", "grant", name, "signature"], value["version"]?.int == 1,
              let grant = value["grant"], let signature = value["signature"]?.string else { throw AnnotationClockError.invalidProof }
        return try GoAuthorityJSON.object([("version", Data("1".utf8)), ("grant", GoAuthorityJSON.signedGrant(grant)), (name, message(value[name], keys: keys)), ("signature", GoAuthorityJSON.string(signature))])
    }
    static func signingBytes(grant: JSONValue, message: Data, name: String) throws -> Data {
        var data = Data(("wendy.world.authority.v1/annotation-clock-" + name + "\0").utf8)
        data.append(GoAuthorityJSON.object([("version", Data("1".utf8)), ("grant_sha256", GoAuthorityJSON.string(WorldCredentials.digest(try GoAuthorityJSON.signedGrant(grant)))), ("message", message)]))
        return data
    }
    static func verifySigned(_ value: JSONValue, message name: String, keys: [String], key: Curve25519.Signing.PublicKey) throws {
        _ = try signed(value, message: name, keys: keys)
        guard let grant = value["grant"], let signature = value["signature"]?.string.flatMap({ Data(base64Encoded: $0) }),
              key.isValidSignature(signature, for: try signingBytes(grant: grant, message: message(value[name], keys: keys), name: name)) else { throw AnnotationClockError.invalidProof }
    }
    static func verifyGrant(_ signed: JSONValue?, pins: AnnotationClockPins, credentials: WorldCredentials, principal: String, kind: String, scope: String, sample: Int64) throws -> Curve25519.Signing.PublicKey {
        guard let signed, let g = signed["grant"], g["version"]?.int == 1,
              g["tenant"]?.string == credentials.tenant, g["site"]?.string == credentials.site, g["domain"]?.string == "simulation",
              g["principal"]?.string == principal, g["kind"]?.string == kind,
              g["publish_audience"]?.string == credentials.audience, g["scopes"]?.array?.contains(.string(scope)) == true,
              let before = g["not_before_unix_ms"]?.int, let after = g["not_after_unix_ms"]?.int, before > 0, after > before, after - before <= 31_536_000_000, before <= sample, sample < after,
              let rawKey = g["public_key"]?.string.flatMap({ Data(base64Encoded: $0) }),
              let signature = signed["signature"]?.string.flatMap({ Data(base64Encoded: $0) }) else { throw AnnotationClockError.invalidProof }
        let canonical = try GoAuthorityJSON.signedGrant(signed)
        guard let marker = canonical.range(of: Data(",\"signature\":".utf8), options: .backwards) else { throw AnnotationClockError.invalidProof }
        var bytes = Data("wendy.world.authority.v1/grant\0".utf8)
        bytes.append(canonical[canonical.index(canonical.startIndex, offsetBy: 9)..<marker.lowerBound])
        let root = try Curve25519.Signing.PublicKey(rawRepresentation: pins.rootPublicKey)
        guard root.isValidSignature(signature, for: bytes) else { throw AnnotationClockError.invalidProof }
        return try Curve25519.Signing.PublicKey(rawRepresentation: rawKey)
    }
}
