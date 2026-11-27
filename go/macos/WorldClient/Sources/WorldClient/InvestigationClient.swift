import CryptoKit
import Foundation

/// An explicitly separate capability. Viewer and mission credential types are unchanged.
public struct InvestigationCredentials: Sendable {
  let endpoint: URL
  let audience,tenant,site,principal,keySHA256: String
  let grant: JSONValue, canonicalGrant: Data
  let key: Curve25519.Signing.PrivateKey
  let before,after: Int64
  public init(data: Data) throws {
    _ = try MissionRawJSON.field(data,path:[],limit:128<<10)
    let v = try JSONValue.decode(data)
    guard Set(v.object?.keys.map{$0} ?? []).isSubset(of:["url","audience","grant","private_key","allow_insecure_simulation"]),
      let address=v["url"]?.string,let url=URL(string:address),let p=URLComponents(url:url,resolvingAgainstBaseURL:false),
      p.host?.isEmpty==false,p.user==nil,p.password==nil,p.query==nil,p.fragment==nil,p.path.isEmpty || p.path=="/",
      p.scheme=="https" || (p.scheme=="http" && v["allow_insecure_simulation"]?.bool==true),
      let audience=v["audience"]?.string,let grant=v["grant"],let g=grant["grant"],g["version"]?.int==1,
      g["domain"]?.string=="simulation",g["kind"]?.string=="operator",g["publish_audience"]?.string==audience,
      g["scopes"]?.array == [.string("evidence-pin")],
      let tenant=g["tenant"]?.string,let site=g["site"]?.string,let principal=g["principal"]?.string,
      [audience,tenant,site,principal].allSatisfy({!$0.isEmpty && $0.utf8.count<=256}),
      let before=g["not_before_unix_ms"]?.int,let after=g["not_after_unix_ms"]?.int,before>0,after>before,
      let secret=v["private_key"]?.string.flatMap({Data(base64Encoded:$0)}),secret.count==64,
      let pub=g["public_key"]?.string.flatMap({Data(base64Encoded:$0)}),pub.count==32 else { throw InvestigationError.authority }
    let key=try Curve25519.Signing.PrivateKey(rawRepresentation:secret.prefix(32))
    guard key.publicKey.rawRepresentation==pub,secret.suffix(32)==pub else { throw InvestigationError.authority }
    self.key=key;self.grant=grant;canonicalGrant=try GoAuthorityJSON.signedGrant(grant)
    endpoint=URL(string:address.hasSuffix("/") ? String(address.dropLast()):address)!
    self.audience=audience;self.tenant=tenant;self.site=site;self.principal=principal
    self.before=before;self.after=after;keySHA256=WorldCredentials.digest(pub)
  }
  func authorization(body:Data,now:Int64) throws -> String {
    guard now>=before,now<after,body.count<=128<<10,now<=Int64.max-30000 else { throw InvestigationError.authority }
    let nonce=UUID().uuidString.lowercased(),expires=min(after,now+30000)
    let o=GoAuthorityJSON.object,s=GoAuthorityJSON.string,n: (Int64)->Data={Data(String($0).utf8)}
    let digest=WorldCredentials.digest(body)
    let payload=o([("version",n(1)),("grant_digest",s(WorldCredentials.digest(canonicalGrant))),
      ("audience",s(audience)),("operation",s("evidence-pin")),("body_digest",s(digest)),("nonce",s(nonce)),
      ("issued",n(now)),("expires",n(expires))])
    let signature=try key.signature(for:Data("wendy.world.authority.v1/request\0".utf8)+payload)
    return o([("version",n(1)),("grant",canonicalGrant),("audience",s(audience)),("operation",s("evidence-pin")),
      ("body_sha256",s(digest)),("nonce",s(nonce)),("issued_at_unix_ms",n(now)),("expires_at_unix_ms",n(expires)),
      ("signature",s(signature.base64EncodedString()))]).base64EncodedString()
  }
}

public actor InvestigationClient: InvestigationTransport {
  public nonisolated let origin: InvestigationOrigin
  private let credentials: InvestigationCredentials
  private let reader: WorldClient
  private let readCredentials: WorldCredentials
  private var latest: Int64=0
  public init(label:String,credentials:InvestigationCredentials,readCredentials:WorldCredentials) throws {
    guard !label.isEmpty,label.utf8.count<=256,readCredentials.scopes==["read","evidence-read"],
      readCredentials.endpoint==credentials.endpoint,readCredentials.audience==credentials.audience,
      readCredentials.tenant==credentials.tenant,readCredentials.site==credentials.site else { throw InvestigationError.authority }
    let binding:JSONValue = .object(["endpoint":.string(credentials.endpoint.absoluteString),"audience":.string(credentials.audience),
      "tenant":.string(credentials.tenant),"site":.string(credentials.site),"principal":.string(credentials.principal),
      "key_sha256":.string(credentials.keySHA256),"backend":.string("standalone-worldd")])
    origin=InvestigationOrigin(id:WorldCredentials.digest(try binding.encoded()),label:label,endpoint:credentials.endpoint.absoluteString,
      audience:credentials.audience,tenant:credentials.tenant,site:credentials.site,principal:credentials.principal,keySHA256:credentials.keySHA256)
    self.credentials=credentials;self.readCredentials=readCredentials;reader=WorldClient(credentials:readCredentials)
  }
  private func clock() throws -> Int64 {
    let now=Int64(Date().timeIntervalSince1970*1000)
    guard now>=latest else { throw InvestigationError.clock };latest=now;return now
  }
  public func authorizeCachedRead() throws {
    let now=try clock()
    _=try readCredentials.signedReadAuthorization(operation:"evidence-read",nonce:UUID().uuidString,issuedAtUnixMS:now)
  }
  public func evidence(_ reference:MissionInspectionEvidence) async throws -> VerifiedEvidence {
    _=try clock()
    let value=try await reader.downloadEvidence(digest:reference.sha256,maxBytes:reference.size)
    let now=try clock()
    guard value.sha256==reference.sha256,value.contentType==reference.contentType,value.data.count==reference.size,
      value.retainUntil.timeIntervalSince1970*1000>Double(now) else { throw InvestigationError.invalid }
    return value
  }
  public func pin(_ request:InvestigationPinRequest) async throws -> InvestigationPinReceipt { try await send(request,releasing:false) }
  public func unpin(_ request:InvestigationPinRequest) async throws -> InvestigationPinReceipt { try await send(request,releasing:true) }
  private func send(_ request:InvestigationPinRequest,releasing:Bool) async throws -> InvestigationPinReceipt {
    try request.validate();let now=try clock()
    guard now<request.retainUntilUnixMS else { throw InvestigationError.expired }
    let body=releasing ? try JSONValue.object(["id":.string(request.id)]).encoded():try JSONEncoder().encode(request)
    var q=URLRequest(url:credentials.endpoint.appendingPathComponent(releasing ? "v1/evidence/unpin":"v1/evidence/pin"),timeoutInterval:3)
    q.httpMethod="POST";q.httpBody=body;q.setValue("application/json",forHTTPHeaderField:"Content-Type")
    q.setValue(try credentials.authorization(body:body,now:now),forHTTPHeaderField:"X-World-Authorization")
    let raw=try await BoundedRequest(limit:128<<10,resourceTimeout:3).run(q)
    _=try MissionRawJSON.field(raw,path:[],limit:128<<10)
    let fields=try JSONValue.decode(raw)
    guard Set(fields.object?.keys.map{$0} ?? []) == ["id","sha256","retain_until_unix_ms","state"] else {throw InvestigationError.invalid}
    let receipt=try JSONDecoder().decode(InvestigationPinReceipt.self,from:raw)
    try receipt.validate(request,releasing:releasing)
    let done=try clock()
    guard done<request.retainUntilUnixMS,done<credentials.after else { throw InvestigationError.expired }
    return receipt
  }
}
