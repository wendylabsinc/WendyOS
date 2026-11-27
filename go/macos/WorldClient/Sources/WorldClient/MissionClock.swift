import CryptoKit
import Foundation

public struct MissionClockPins: Sendable {
    let value: AnnotationClockPins
    public init(json: JSONValue) throws { value = try AnnotationClockPins(json: json) }
}

/// This proof is memory-only. It never uses a phone wall clock or a persisted
/// annotation anchor to authorize a mission.
struct MissionClockAnchor: Sendable {
    let observation: JSONValue
    let received: AnnotationClockReading
    let rttNS: Int64
    let purpose: String
    func interval(at now: AnnotationClockReading) throws -> AnnotationTimeInterval {
        guard received.nanoseconds >= 0, now.bootID == received.bootID, now.nanoseconds >= received.nanoseconds,
              rttNS >= 0, rttNS <= 2_000_000_000 else { throw MissionError.clockUnavailable }
        let elapsed = now.nanoseconds - received.nanoseconds
        guard elapsed <= 5_000_000_000, let sample = observation["observation"]?["sample_unix_ms"]?.int,
              sample > 0, sample < 253_402_214_400_000 else { throw MissionError.clockUnavailable }
        let drift = ((rttNS + elapsed) * 100 + 999_999) / 1_000_000
        func ceilMS(_ ns: Int64) -> Int64 { (ns + 999_999) / 1_000_000 }
        return AnnotationTimeInterval(lowerUnixMS: sample + elapsed / 1_000_000 - ceilMS(drift) - 1,
                                      upperUnixMS: sample + ceilMS(rttNS + elapsed) + ceilMS(drift) + 1)
    }
}

enum MissionClockWire {
    static let requestKeys = ["tenant", "site", "audience", "run_id", "clock_domain", "purpose", "nonce"]
    static let observationKeys = ["request_sha256", "tenant", "site", "audience", "run_id", "clock_domain", "purpose", "nonce", "operator_principal", "operator_key_sha256", "gateway_principal", "gateway_boot_id", "sample_unix_ms", "reference", "max_rtt_ms", "max_holdover_ms", "drift_ppm", "quantization_ms"]
    static func signing(grant: JSONValue, message: Data, name: String) throws -> Data {
        var data = Data(("wendy.world.authority.v1/mission-clock-" + name + "\0").utf8)
        data.append(GoAuthorityJSON.object([("version", Data("1".utf8)), ("grant_sha256", GoAuthorityJSON.string(WorldCredentials.digest(try GoAuthorityJSON.signedGrant(grant)))), ("message", message)]))
        return data
    }
    static func request(credentials: MissionCredentials, pins: MissionClockPins, purpose: String, nonce: String) throws -> Data {
        guard ["mission", "stop"].contains(purpose), WorldClient.isDigest(nonce) else { throw MissionError.clockUnavailable }
        let body = GoAuthorityJSON.object([("tenant", GoAuthorityJSON.string(credentials.tenant)), ("site", GoAuthorityJSON.string(credentials.site)),
            ("audience", GoAuthorityJSON.string(credentials.audience)), ("run_id", GoAuthorityJSON.string(pins.value.runID)),
            ("clock_domain", GoAuthorityJSON.string(pins.value.clockDomain)), ("purpose", GoAuthorityJSON.string(purpose)), ("nonce", GoAuthorityJSON.string(nonce))])
        let signature = try credentials.key.signature(for: signing(grant: credentials.grant, message: body, name: "request"))
        return GoAuthorityJSON.object([("version", Data("1".utf8)), ("grant", credentials.canonicalGrant), ("request", body), ("signature", GoAuthorityJSON.string(signature.base64EncodedString()))])
    }
    static func verify(observation: JSONValue, request: Data, credentials: MissionCredentials, pins: MissionClockPins, purpose: String) throws {
        let req = try JSONValue.decode(request)["request"], obs = observation["observation"]
        guard observation["version"]?.int == 1,
              let sample = obs?["sample_unix_ms"]?.int, sample > 0,
              obs?["request_sha256"]?.string == WorldCredentials.digest(request),
              obs?["nonce"] == req?["nonce"], obs?["purpose"]?.string == purpose,
              obs?["operator_principal"]?.string == credentials.principal,
              obs?["operator_key_sha256"]?.string == WorldCredentials.digest(credentials.key.publicKey.rawRepresentation),
              obs?["gateway_principal"]?.string == pins.value.gatewayPrincipal,
              let boot = obs?["gateway_boot_id"]?.string, WorldClient.isDigest(boot),
              obs?["reference"]?.string == "simulation-gateway-relative-mission-v1",
              obs?["max_rtt_ms"]?.int == 2000, obs?["max_holdover_ms"]?.int == 5000,
              obs?["drift_ppm"]?.int == 100, obs?["quantization_ms"]?.int == 1,
              sample >= credentials.notBefore, purpose == "stop" || sample < credentials.notAfter else { throw MissionError.clockUnavailable }
        for (field, expected) in [("tenant", credentials.tenant), ("site", credentials.site), ("audience", credentials.audience), ("run_id", pins.value.runID), ("clock_domain", pins.value.clockDomain)] {
            guard obs?[field]?.string == expected else { throw MissionError.clockUnavailable }
        }
        try credentials.verifyRoot(pins.value.rootPublicKey)
        guard let signedGrant = observation["grant"], let g = signedGrant["grant"],
              g["version"]?.int == 1, g["tenant"]?.string == credentials.tenant, g["site"]?.string == credentials.site,
              g["domain"]?.string == "simulation", g["kind"]?.string == "device", g["principal"]?.string == pins.value.gatewayPrincipal,
              g["publish_audience"]?.string == credentials.audience,
              g["scopes"]?.array?.contains(.string("mission-clock-server")) == true,
              let before = g["not_before_unix_ms"]?.int, let after = g["not_after_unix_ms"]?.int,
              before > 0, after > before, after - before <= 31_536_000_000, before <= sample, sample < after,
              let publicData = g["public_key"]?.string.flatMap({ Data(base64Encoded: $0) }),
              WorldCredentials.digest(publicData) == pins.value.gatewayKeySHA256,
              let grantSignature = signedGrant["signature"]?.string.flatMap({ Data(base64Encoded: $0) }),
              let signature = observation["signature"]?.string.flatMap({ Data(base64Encoded: $0) }) else { throw MissionError.clockUnavailable }
        let canonicalGrant = try GoAuthorityJSON.signedGrant(signedGrant)
        guard let marker = canonicalGrant.range(of: Data(",\"signature\":".utf8), options: .backwards) else { throw MissionError.clockUnavailable }
        var grantMessage = Data("wendy.world.authority.v1/grant\0".utf8)
        grantMessage.append(canonicalGrant[canonicalGrant.index(canonicalGrant.startIndex, offsetBy: 9)..<marker.lowerBound])
        guard try Curve25519.Signing.PublicKey(rawRepresentation: pins.value.rootPublicKey).isValidSignature(grantSignature, for: grantMessage) else { throw MissionError.clockUnavailable }
        _ = try ClockWire.signed(observation, message: "observation", keys: observationKeys)
        let message = try ClockWire.message(obs, keys: observationKeys)
        guard try Curve25519.Signing.PublicKey(rawRepresentation: publicData).isValidSignature(signature, for: signing(grant: signedGrant, message: message, name: "observation")) else { throw MissionError.clockUnavailable }
    }
}
