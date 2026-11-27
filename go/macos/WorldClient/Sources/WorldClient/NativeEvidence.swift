import Foundation

/// One original capture owner's media reference. This is not a media URL or a
/// claim that the client independently verified its input proof.
public struct NativeEvidenceReference: Sendable, Equatable, Codable {
    public let source: JSONValue
    public let sha256: String
    public let size: Int
    public let contentType: String
    /// Local observation binding only; never added to the HTTP range request.
    public let proofEnvelopeSHA256: String
    public let cacheKey: String

    public init(source: JSONValue, sha256: String, size: Int, contentType: String, proofEnvelopeSHA256: String) throws {
        guard let fields = source.object,
              Set(fields.keys) == ["device", "app", "service", "component_type", "stream"],
              source["component_type"]?.string == "wendy.sensor.capture.v1",
              WorldClient.isDigest(sha256), WorldClient.isDigest(proofEnvelopeSHA256), (1...(8 << 20)).contains(size),
              ["image/png", "image/jpeg", "audio/wav"].contains(contentType) else { throw WorldClientError.invalidEvidence }
        for field in fields.values {
            guard let text = field.string, !text.isEmpty, text.utf8.count <= 512,
                  text == text.trimmingCharacters(in: .whitespacesAndNewlines),
                  text.unicodeScalars.allSatisfy({ !CharacterSet.controlCharacters.contains($0) }) else { throw WorldClientError.invalidEvidence }
        }
        self.source = source; self.sha256 = sha256; self.size = size; self.contentType = contentType
        self.proofEnvelopeSHA256 = proofEnvelopeSHA256
        var binding = Self.json(source, sha256, size, contentType).object!
        binding["proof_envelope_sha256"] = .string(proofEnvelopeSHA256)
        cacheKey = WorldCredentials.digest(try JSONValue.object(binding).encoded())
    }
    public var raw: JSONValue { Self.json(source, sha256, size, contentType) }
    private static func json(_ source: JSONValue, _ sha: String, _ size: Int, _ mime: String) -> JSONValue {
        .object(["source": source, "sha256": .string(sha), "size": .integer(Int64(size)), "content_type": .string(mime)])
    }
    public init(from decoder: any Decoder) throws {
        let value = try JSONValue(from: decoder)
        guard let source = value["source"], let sha = value["sha256"]?.string,
              let size = value["size"]?.int, let count = Int(exactly: size),
              let mime = value["content_type"]?.string,
              let proof = value["proof_envelope_sha256"]?.string else { throw WorldClientError.invalidEvidence }
        try self.init(source: source, sha256: sha, size: count, contentType: mime, proofEnvelopeSHA256: proof)
    }
    public func encode(to encoder: any Encoder) throws {
        var binding = raw.object!
        binding["proof_envelope_sha256"] = .string(proofEnvelopeSHA256)
        try JSONValue.object(binding).encode(to: encoder)
    }
    var media: MissionInspectionEvidence {
        get throws { try MissionInspectionEvidence(sha256: sha256, contentType: contentType, size: size) }
    }
}
