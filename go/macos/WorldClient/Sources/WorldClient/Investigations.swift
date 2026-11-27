import Foundation

public enum InvestigationError: Error, LocalizedError, Sendable {
  case invalid, authority, originChanged, expired, clock, busy, full
  public var errorDescription: String? {
    switch self {
    case .invalid: "The investigation or capture-node acknowledgement does not match its saved intent."
    case .authority: "Separate evidence-read and capture-node-bound evidence-pin authority are required."
    case .originChanged: "This capture-node identity differs from the saved investigation."
    case .expired: "The original retention deadline has passed."
    case .clock: "The local clock moved backward. Current retention status cannot be confirmed."
    case .busy: "This investigation already has an operation in progress."
    case .full: "The bounded investigation journal is full."
    }
  }
}

public struct InvestigationOrigin: Codable, Sendable, Equatable, Identifiable {
  public let id, label, endpoint, audience, tenant, site, principal, keySHA256: String
  public var backend: String { "standalone-worldd" }
  func validate() throws {
    guard WorldClient.isDigest(id), WorldClient.isDigest(keySHA256),
      [label,audience,tenant,site,principal].allSatisfy({ !$0.isEmpty && $0.utf8.count <= 256 }),
      let url = URL(string: endpoint), ["https","http"].contains(url.scheme ?? ""), url.host != nil,
      id == WorldCredentials.digest(try binding.encoded()) else { throw InvestigationError.invalid }
  }
  var binding: JSONValue {
    .object(["endpoint":.string(endpoint),"audience":.string(audience),"tenant":.string(tenant),
             "site":.string(site),"principal":.string(principal),"key_sha256":.string(keySHA256),
             "backend":.string(backend)])
  }
}

public struct InvestigationEvidence: Codable, Sendable, Equatable, Identifiable {
  public let origin: InvestigationOrigin
  public let sensorID, sha256, contentType: String
  public let size: Int
  public let originalRetainUntilUnixMS: Int64
  public var id: String { origin.id + ":" + sensorID + ":" + sha256 }
  public init(origin: InvestigationOrigin, sensorID: String, sha256: String, size: Int,
              contentType: String, originalRetainUntilUnixMS: Int64) throws {
    self.origin=origin; self.sensorID=sensorID; self.sha256=sha256; self.size=size
    self.contentType=contentType; self.originalRetainUntilUnixMS=originalRetainUntilUnixMS
    try validate()
  }
  func validate() throws {
    try origin.validate()
    _ = try MissionInspectionEvidence(sha256: sha256, contentType: contentType, size: size)
    guard !sensorID.isEmpty, sensorID.utf8.count <= 128, originalRetainUntilUnixMS > 0 else { throw InvestigationError.invalid }
  }
}

public struct InvestigationPinRequest: Codable, Sendable, Equatable {
  public let id: String
  public let sha256: [String]
  public let retainUntilUnixMS: Int64
  enum CodingKeys: String, CodingKey { case id,sha256; case retainUntilUnixMS="retain_until_unix_ms" }
  func validate() throws {
    guard !id.isEmpty, id.utf8.count<=256, !id.contains("\0"), (1...32).contains(sha256.count),
      sha256.allSatisfy(WorldClient.isDigest), Set(sha256).count==sha256.count,
      sha256==sha256.sorted(), retainUntilUnixMS>0 else { throw InvestigationError.invalid }
  }
}
public struct InvestigationPinReceipt: Codable, Sendable, Equatable {
  public let id: String
  public let sha256: [String]
  public let retainUntilUnixMS: Int64
  public let state: String
  enum CodingKeys: String,CodingKey { case id,sha256,state; case retainUntilUnixMS="retain_until_unix_ms" }
  func validate(_ request: InvestigationPinRequest, releasing: Bool) throws {
    guard id==request.id, sha256==request.sha256, retainUntilUnixMS==request.retainUntilUnixMS,
      releasing ? state=="released" : ["active","released"].contains(state) else { throw InvestigationError.invalid }
  }
}
public enum InvestigationIntent: String,Codable,Sendable { case retain,release }
public enum InvestigationState: String,Codable,Sendable {
  case pending,sendingCreate,uncertainCreate,active,sendingRelease,uncertainRelease,released,canceled,unavailable,denied,expired
}
public struct InvestigationHold: Codable,Sendable,Identifiable {
  public let origin: InvestigationOrigin
  public let request: InvestigationPinRequest
  public var intent: InvestigationIntent
  public var state: InvestigationState
  public var createAttempted, createAcknowledged: Bool
  public var checkedAtUnixMS: Int64?
  public var lastError: String?
  public var id: String { origin.id }
}
public struct InvestigationEntry: Codable,Sendable,Identifiable {
  public let id,name,tenant,site,taskID,runID,resultSHA256: String
  public let createdUnixMS: Int64
  /// Saved historical context, not a current observation or an independently verified proof.
  public let result: JSONValue
  public let evidence: [InvestigationEvidence]
  public var holds: [InvestigationHold]
}

public protocol InvestigationTransport: Sendable {
  var origin: InvestigationOrigin { get }
  func pin(_ request: InvestigationPinRequest) async throws -> InvestigationPinReceipt
  func unpin(_ request: InvestigationPinRequest) async throws -> InvestigationPinReceipt
}
