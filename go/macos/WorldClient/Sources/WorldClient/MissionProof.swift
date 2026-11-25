import CryptoKit
import Foundation

/// Historical proofs establish who signed these exact bytes. They never grant
/// current execution authority or establish historical revocation state.
enum MissionProof {
    static func grant(_ signed: JSONValue, credentials: MissionCredentials, root: Data, operation: String, issued: Int64) throws -> Curve25519.Signing.PublicKey {
        guard let g = signed["grant"], g["version"]?.int == 1,
              g["tenant"]?.string == credentials.tenant, g["site"]?.string == credentials.site,
              g["domain"]?.string == "simulation", g["scopes"]?.array?.contains(.string(operation)) == true,
              let kind = g["kind"]?.string, operation == "propose" ? ["agent", "operator"].contains(kind) : kind == "operator",
              let before = g["not_before_unix_ms"]?.int, let after = g["not_after_unix_ms"]?.int,
              before > 0, after > before, after - before <= 31_536_000_000, issued >= before,
              operation == "stop" || issued < after,
              let rawKey = g["public_key"]?.string.flatMap({ Data(base64Encoded: $0) }), rawKey.count == 32,
              let signature = signed["signature"]?.string.flatMap({ Data(base64Encoded: $0) }) else { throw MissionError.invalidJournal }
        let canonical = try GoAuthorityJSON.signedGrant(signed)
        guard let marker = canonical.range(of: Data(",\"signature\":".utf8), options: .backwards) else { throw MissionError.invalidJournal }
        var message = Data("wendy.world.authority.v1/grant\0".utf8)
        message.append(canonical[canonical.index(canonical.startIndex, offsetBy: 9)..<marker.lowerBound])
        guard try Curve25519.Signing.PublicKey(rawRepresentation: root).isValidSignature(signature, for: message) else { throw MissionError.invalidJournal }
        return try Curve25519.Signing.PublicKey(rawRepresentation: rawKey)
    }
    static func proposal(record: JSONValue, body: Data, credentials: MissionCredentials, root: Data) throws {
        guard let proof = record["proof"], let signedGrant = proof["grant"], let g = signedGrant["grant"],
              Set(proof.object?.keys.map { $0 } ?? []) == ["version", "grant", "audience", "operation", "body_sha256", "nonce", "issued_at_unix_ms", "expires_at_unix_ms", "signature"],
              proof["version"]?.int == 1, proof["audience"]?.string == credentials.audience, proof["operation"]?.string == "propose",
              proof["body_sha256"]?.string == WorldCredentials.digest(body),
              let nonce = proof["nonce"]?.string, nonce.utf8.count >= 16, MissionWire.identifier(nonce),
              let issued = proof["issued_at_unix_ms"]?.int, let expires = proof["expires_at_unix_ms"]?.int,
              issued > 0, expires > issued, expires - issued <= 60_000,
              let accepted = record["accepted_at_unix_ms"]?.int, accepted >= issued, accepted < expires,
              record["principal"] == g["principal"], record["kind"] == g["kind"], record["source"] == g["source"], record["grant_id"] == g["id"],
              let signature = proof["signature"]?.string.flatMap({ Data(base64Encoded: $0) }) else { throw MissionError.invalidProposal }
        let key = try grant(signedGrant, credentials: credentials, root: root, operation: "propose", issued: issued)
        let canonicalGrant = try GoAuthorityJSON.signedGrant(signedGrant)
        var message = Data("wendy.world.authority.v1/request\0".utf8)
        message.append(GoAuthorityJSON.object([("version", Data("1".utf8)), ("grant_digest", GoAuthorityJSON.string(WorldCredentials.digest(canonicalGrant))),
            ("audience", GoAuthorityJSON.string(credentials.audience)), ("operation", GoAuthorityJSON.string("propose")),
            ("body_digest", GoAuthorityJSON.string(WorldCredentials.digest(body))), ("nonce", GoAuthorityJSON.string(nonce)),
            ("issued", Data(String(issued).utf8)), ("expires", Data(String(expires).utf8))]))
        guard key.isValidSignature(signature, for: message) else { throw MissionError.invalidProposal }
    }
    static func stoppedRecord(_ record: JSONValue, taskID: String, credentials: MissionCredentials, context: MissionContext, originalJSON: Data? = nil) throws {
        if let originalJSON {
            guard try JSONValue.decode(originalJSON, limit: 128 << 10) == record else { throw MissionError.mismatchedStatus }
        }
        guard let spec = record["spec"], spec["id"]?.string == taskID, spec["site"]?.string == credentials.site,
              spec["assignee"]?.string == context.assignee, spec["domain"]?.string == "simulation",
              let version = record["status"]?["Version"]?.uint, version > 0,
              let phase = record["status"]?["Phase"]?.string, ["succeeded", "failed", "canceled", "rejected"].contains(phase),
              let proof = record["proof"], proof["version"]?.int == 1, proof["audience"]?.string == credentials.audience,
              let operation = proof["operation"]?.string, ["mission", "stop"].contains(operation),
              let signedGrant = proof["grant"], let issued = proof["issued_at_unix_ms"]?.int,
              let expires = proof["expires_at_unix_ms"]?.int, issued > 0, expires > issued, expires - issued <= 86_400_000,
              Set(proof.object?.keys.map { $0 } ?? []) == ["version", "grant", "audience", "operation", "payload", "issued_at_unix_ms", "expires_at_unix_ms", "signature"],
              let signature = proof["signature"]?.string.flatMap({ Data(base64Encoded: $0) }) else { throw MissionError.mismatchedStatus }
        let payload: Data
        if operation == "mission" {
            guard proof["payload"] == spec else { throw MissionError.mismatchedStatus }
            if let trigger = spec["trigger"] {
                guard trigger["component"]?["key"]?["tenant"]?.string == credentials.tenant else { throw MissionError.mismatchedStatus }
            }
            if let originalJSON {
                let rawSpec = try MissionRawJSON.field(originalJSON, path: ["spec"])
                let rawProofSpec = try MissionRawJSON.field(originalJSON, path: ["proof", "payload"])
                payload = try specBytes(rawSpec)
                guard try specBytes(rawProofSpec) == payload else { throw MissionError.mismatchedStatus }
            } else { payload = try specBytes(spec) }
        } else {
            guard proof["payload"] == .object(["task_id": .string(taskID)]), phase == "canceled",
                  spec["type"]?.string == "", spec["class"]?.string == "", spec["mode"]?.string == "" else { throw MissionError.mismatchedStatus }
            payload = GoAuthorityJSON.object([("task_id", GoAuthorityJSON.string(taskID))])
        }
        let key = try grant(signedGrant, credentials: credentials, root: context.rootPublicKey, operation: operation, issued: issued)
        var message = Data("wendy.world.authority.v1/instruction\0".utf8)
        message.append(GoAuthorityJSON.object([("version", Data("1".utf8)), ("grant_digest", GoAuthorityJSON.string(WorldCredentials.digest(try GoAuthorityJSON.signedGrant(signedGrant)))),
            ("audience", GoAuthorityJSON.string(credentials.audience)), ("operation", GoAuthorityJSON.string(operation)), ("payload", payload),
            ("issued", Data(String(issued).utf8)), ("expires", Data(String(expires).utf8))]))
        guard key.isValidSignature(signature, for: message) else { throw MissionError.mismatchedStatus }
    }
    static func specBytes(_ raw: Data) throws -> Data {
        let value = try JSONValue.decode(raw, limit: 64 << 10)
        let trigger = try value["trigger"].map { _ in try MissionRawJSON.field(raw, path: ["trigger"], limit: 64 << 10) }
        return try specBytes(value, rawTrigger: trigger)
    }
    static func specBytes(_ value: JSONValue, rawTrigger: Data? = nil) throws -> Data {
        let required = ["id", "type", "site", "assignee", "domain", "class", "mode", "frame", "not_after_unix_ms", "area", "required_sensors", "minimum_energy", "fallback", "target", "speed_mps", "tolerance_m", "stop_speed_mps"]
        let optional = ["go2", "trigger", "inspection_destination", "control_context", "proposal"]
        guard let object = value.object, Set(object.keys).isSubset(of: Set(required + optional)), Set(required).isSubset(of: Set(object.keys)) else { throw MissionError.mismatchedStatus }
        var fields: [(String, Data)] = []
        let destination = try value["inspection_destination"].map(MissionInspectionDestination.init(json:))
        if destination != nil, value["trigger"] == nil { throw MissionError.mismatchedStatus }
        for name in optional {
            guard let v = value[name] else { continue }
            switch name {
            case "trigger":
                let canonicalTrigger: Data
                if let rawTrigger {
                    guard try JSONValue.decode(rawTrigger) == v else { throw MissionError.mismatchedStatus }
                    canonicalTrigger = try NativeTriggerWire.trigger(canonical: rawTrigger)
                } else { canonicalTrigger = try NativeTriggerWire.trigger(v) }
                guard let component = v["component"], component["key"]?["site"] == value["site"],
                      component["frame"] == value["frame"] else { throw MissionError.mismatchedStatus }
                _ = try NativeTriggerWire.review(component, fixedTarget: MissionWire.vector(value["target"]), inspectionDestination: destination)
                fields.append((name, canonicalTrigger))
            case "inspection_destination": fields.append((name, try destination!.canonical()))
            case "control_context":
                guard Set(v.object?.keys.map { $0 } ?? []) == ["context", "signature"], let policy = v["context"], let signature = v["signature"]?.string else { throw MissionError.mismatchedStatus }
                fields.append((name, GoAuthorityJSON.object([("context", try MissionContext.policyBytes(policy)), ("signature", GoAuthorityJSON.string(signature))])))
            case "proposal": fields.append((name, try MissionWire.ordered(v, keys: ["audience", "id", "body_sha256"])))
            // The native simulated inspector cannot interpret other adapter bindings.
            // A stop may still have succeeded; an unfamiliar proof stays uncertain.
            default: throw MissionError.unsupported
            }
        }
        for name in required {
            if name == "frame" { fields.append((name, try MissionWire.ordered(value[name]!, keys: ["id", "revision"]))) }
            else if name == "area" { fields.append((name, try MissionWire.ordered(value[name]!, keys: ["min", "max"]))) }
            else { fields.append((name, try MissionWire.value(value[name]!))) }
        }
        return GoAuthorityJSON.object(fields)
    }
}
