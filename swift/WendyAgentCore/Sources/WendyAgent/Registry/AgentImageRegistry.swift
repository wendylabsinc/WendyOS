import Crypto
import Foundation
import GRPCCore
import HTTPTypes
import Hummingbird
import HummingbirdCore
import HummingbirdTLS
import Logging
import NIOCore

/// One push listener and one loopback-only pull listener share the existing
/// blob store. Enrolled device credentials require an admitted operator grant;
/// the pull listener remains read-only for the local container backends.
struct AgentImageRegistry: Sendable {
    struct ListenerConfiguration: Sendable {
        enum Routes: Sendable { case pushAndPull, pullOnly }
        var host: String
        var port: Int
        var tls: RegistryTLS.Configuration?
        var routes: Routes
        var label: String
    }
    private let store: BlobStore
    private let configuration: ListenerConfiguration
    private let authority: BuildDeliveryAuthority?
    private let uploads: RegistryUploads
    private let logger = Logger(label: "sh.wendy.agent.registry")
    private static let digestHeader = HTTPField.Name("Docker-Content-Digest")!
    private static let uploadHeader = HTTPField.Name("Docker-Upload-UUID")!
    private static let maxManifestSize = 4 * 1024 * 1024

    init(
        store: BlobStore,
        configuration: ListenerConfiguration,
        authority: BuildDeliveryAuthority? = nil
    ) {
        self.store = store
        self.configuration = configuration
        self.authority = authority
        uploads = RegistryUploads(store: store, authority: authority)
    }
    private func authorized(
        _ request: Request,
        _ context: RegistryRequestContext,
        repository: String = "",
        operation: RegistryAuthorization.Operation,
        perform: @Sendable (RegistryAccess) async throws -> Response
    ) async throws -> Response {
        do {
            if !repository.isEmpty && !BlobStore.isValidRepository(repository) {
                throw HTTPError(.badRequest)
            }
            let access = try await RegistryAuthorization.access(
                request: request,
                context: context,
                tls: configuration.tls,
                authority: authority,
                repository: repository,
                operation: operation
            )
            return try await perform(access)
        } catch let error as BuildDeliveryContract.Failure {
            throw HTTPError(.forbidden, message: error.description)
        } catch let error as RPCError { throw HTTPError(.forbidden, message: error.message) }
    }
    private static func mediaType(_ data: Data) -> String {
        (try? JSONSerialization.jsonObject(with: data) as? [String: Any])?["mediaType"] as? String
            ?? "application/vnd.oci.image.manifest.v1+json"
    }
    private func manifest(
        _ request: Request,
        _ context: RegistryRequestContext,
        head: Bool
    ) async throws -> Response {
        let repo = context.parameters.get("repo") ?? ""
        let reference = context.parameters.get("reference") ?? ""
        guard BlobStore.isValidReference(reference) else { throw HTTPError(.notFound) }
        return try await authorized(
            request,
            context,
            repository: repo,
            operation: head ? .metadata : .read
        ) { access in
            if let session = access.session, let leaf = access.leaf, let authority {
                let claims = try await authority.policy(session, leaf: leaf, repository: repo)
                guard
                    BlobStore.isValidSHA256Digest(reference)
                        || claims.reference == repo + ":" + reference
                else { throw HTTPError(.forbidden) }
            }
            guard let url = store.manifestURL(repository: repo, reference: reference),
                let data = try? Data(contentsOf: url), data.count <= Self.maxManifestSize
            else { return Response(status: .notFound) }
            let headers: HTTPFields = [
                .contentType: Self.mediaType(data),
                Self.digestHeader: BlobStore.contentDigest(data),
                .contentLength: String(data.count),
            ]
            return Response(
                status: .ok,
                headers: headers,
                body: head ? .init() : .init(byteBuffer: ByteBuffer(bytes: data))
            )
        }
    }
    private func blob(
        _ request: Request,
        _ context: RegistryRequestContext,
        head: Bool
    ) async throws -> Response {
        let repo = context.parameters.get("repo") ?? ""
        let digest = context.parameters.get("digest") ?? ""
        guard BlobStore.isValidSHA256Digest(digest) else { throw HTTPError(.notFound) }
        return try await authorized(
            request,
            context,
            repository: repo,
            operation: head ? .metadata : .read
        ) { access in
            guard store.hasBlob(digest: digest) else { return Response(status: .notFound) }
            let url = store.blobURL(digest: digest)
            let size =
                (try FileManager.default.attributesOfItem(atPath: url.path)[.size] as? NSNumber)?
                .intValue ?? 0
            let headers: HTTPFields = [
                .contentType: "application/octet-stream", Self.digestHeader: digest,
                .contentLength: String(size),
            ]
            if head { return Response(status: .ok, headers: headers) }
            let body = ResponseBody(contentLength: size) { writer in
                let file = try FileHandle(forReadingFrom: url)
                defer { try? file.close() }
                while let data = try file.read(upToCount: 64 * 1024), !data.isEmpty {
                    try Task.checkCancellation()
                    try access.checkTime()
                    try await writer.write(ByteBuffer(bytes: data))
                }
                try await writer.finish(nil)
            }
            return Response(status: .ok, headers: headers, body: body)
        }
    }
    private static func uploadResponse(
        repo: String,
        id: String,
        size: Int64,
        status: HTTPResponse.Status = .accepted
    ) -> Response {
        Response(
            status: status,
            headers: [
                .location: "/v2/\(repo)/blobs/uploads/\(id)", uploadHeader: id,
                .range: "0-\(max(0,size - 1))",
            ]
        )
    }
    func buildRouter() -> Router<RegistryRequestContext> {
        let router = Router(context: RegistryRequestContext.self)
        router.addMiddleware { LogRequestsMiddleware(.info) }
        router.get("/v2") { request, context in
            try await authorized(request, context, operation: .ping) { _ in Response(status: .ok) }
        }
        router.head("/v2/{repo}/blobs/{digest}") { request, context in
            try await blob(request, context, head: true)
        }
        router.get("/v2/{repo}/blobs/{digest}") { request, context in
            try await blob(request, context, head: false)
        }
        router.head("/v2/{repo}/manifests/{reference}") { request, context in
            try await manifest(request, context, head: true)
        }
        router.get("/v2/{repo}/manifests/{reference}") { request, context in
            try await manifest(request, context, head: false)
        }
        if case .pushAndPull = configuration.routes { addWriteRoutes(to: router) }
        return router
    }
    private func addWriteRoutes(to router: Router<RegistryRequestContext>) {
        router.post("/v2/{repo}/blobs/uploads") { request, context in
            let repo = context.parameters.get("repo") ?? ""
            return try await authorized(request, context, repository: repo, operation: .stage) {
                access in
                let id = try await uploads.start(access: access, repository: repo)
                if let digest = request.uri.queryParameters["digest"].map(String.init) {
                    return try await RegistryAuthorization.beforeDeadline(
                        access: access,
                        channel: context.channel
                    ) {
                        _ = try await uploads.append(
                            id,
                            request: request,
                            access: access,
                            repository: repo
                        )
                        try await uploads.commit(
                            id,
                            digest: digest,
                            access: access,
                            repository: repo
                        )
                        return Response(
                            status: .created,
                            headers: [
                                .location: "/v2/\(repo)/blobs/\(digest)", Self.digestHeader: digest,
                            ]
                        )
                    }
                }
                // Cross-repository mounts deliberately take the ordinary upload path.
                return Self.uploadResponse(repo: repo, id: id, size: 0)
            }
        }
        router.get("/v2/{repo}/blobs/uploads/{id}") { request, context in
            let repo = context.parameters.get("repo") ?? ""
            let id = context.parameters.get("id") ?? ""
            return try await authorized(request, context, repository: repo, operation: .stage) {
                access in
                let size = try await uploads.size(id, access: access, repository: repo)
                return Self.uploadResponse(repo: repo, id: id, size: size, status: .noContent)
            }
        }
        router.patch("/v2/{repo}/blobs/uploads/{id}") { request, context in
            let repo = context.parameters.get("repo") ?? ""
            let id = context.parameters.get("id") ?? ""
            return try await authorized(request, context, repository: repo, operation: .stage) {
                access in
                try await RegistryAuthorization.beforeDeadline(
                    access: access,
                    channel: context.channel
                ) {
                    let size = try await uploads.append(
                        id,
                        request: request,
                        access: access,
                        repository: repo
                    )
                    return Self.uploadResponse(repo: repo, id: id, size: size)
                }
            }
        }
        router.put("/v2/{repo}/blobs/uploads/{id}") { request, context in
            let repo = context.parameters.get("repo") ?? ""
            let id = context.parameters.get("id") ?? ""
            let digest = request.uri.queryParameters["digest"].map(String.init) ?? ""
            return try await authorized(request, context, repository: repo, operation: .stage) {
                access in
                try await RegistryAuthorization.beforeDeadline(
                    access: access,
                    channel: context.channel
                ) {
                    _ = try await uploads.append(
                        id,
                        request: request,
                        access: access,
                        repository: repo
                    )
                    try await uploads.commit(id, digest: digest, access: access, repository: repo)
                    return Response(
                        status: .created,
                        headers: [
                            .location: "/v2/\(repo)/blobs/\(digest)", Self.digestHeader: digest,
                        ]
                    )
                }
            }
        }
        router.put("/v2/{repo}/manifests/{reference}") { request, context in
            let repo = context.parameters.get("repo") ?? ""
            let reference = context.parameters.get("reference") ?? ""
            guard BlobStore.isValidReference(reference) else { throw HTTPError(.badRequest) }
            return try await authorized(request, context, repository: repo, operation: .publish) {
                access in
                if let session = access.session, let leaf = access.leaf, let authority {
                    let claims = try await authority.policy(session, leaf: leaf, repository: repo)
                    guard
                        BlobStore.isValidSHA256Digest(reference)
                            || claims.reference == repo + ":" + reference
                    else { throw HTTPError(.forbidden) }
                }
                let declared: Int?
                if let length = request.headers[.contentLength] {
                    guard let count = Int(length), count >= 0, count <= Self.maxManifestSize else {
                        throw HTTPError(.contentTooLarge)
                    }
                    declared = count
                    if let session = access.session, let leaf = access.leaf, let authority {
                        try await authority.reserve(session, leaf: leaf, bytes: Int64(count))
                    }
                } else {
                    declared = nil
                }
                let data = try await RegistryAuthorization.beforeDeadline(
                    access: access,
                    channel: context.channel
                ) {
                    Data(
                        try await request.body.collect(upTo: Self.maxManifestSize).readableBytesView
                    )
                }
                guard declared == nil || declared == data.count else {
                    throw HTTPError(.badRequest)
                }
                if declared == nil, let session = access.session, let leaf = access.leaf,
                    let authority
                {
                    try await authority.reserve(session, leaf: leaf, bytes: Int64(data.count))
                }
                try access.checkTime()
                if let authority {
                    try await authority.publish(
                        data,
                        repository: repo,
                        reference: reference,
                        session: access.session,
                        leaf: access.leaf
                    )
                } else {
                    try store.writeManifest(data, repository: repo, reference: reference)
                }
                return Response(
                    status: .created,
                    headers: [Self.digestHeader: BlobStore.contentDigest(data)]
                )
            }
        }
    }

    /// Builds the Hummingbird application for this listener. Throws when the
    /// TLS configuration is present but unusable (bad PEM) — callers must
    /// treat that as "listener stays down", never fall back to plain HTTP.
    ///
    /// The "listening" log fires from `onServerRunning` so it reports the
    /// actually-bound address (tests bind port 0).
    func buildApplication(
        onServerRunning: @escaping @Sendable (any Channel) async -> Void = { _ in }
    ) throws -> Application<RouterResponder<RegistryRequestContext>> {
        let server: HTTPServerBuilder
        if let tls = self.configuration.tls {
            server = try .tls(.http1(), configuration: RegistryTLS.channelConfiguration(tls))
        } else {
            server = .http1()
        }
        let logger = self.logger
        let label = self.configuration.label
        let scheme = self.configuration.tls == nil ? "http" : "https"
        return Application(
            router: self.buildRouter(),
            server: server,
            configuration: .init(
                address: .hostname(self.configuration.host, port: self.configuration.port)
            ),
            onServerRunning: { channel in
                logger.info(
                    "Agent image registry listening",
                    metadata: [
                        "listener": "\(label)",
                        "scheme": "\(scheme)",
                        "address":
                            "\(channel.localAddress.map(String.init(describing:)) ?? "unknown")",
                    ]
                )
                await onServerRunning(channel)
            }
        )
    }

    /// Serves until the calling task is cancelled, which is how the agent
    /// stops the listener.
    ///
    /// Traps no signals. By default `runService()` has swift-service-lifecycle
    /// ignore SIGTERM and SIGINT for the whole process and shut down only this
    /// listener on them, so `killall` left the rest of the agent running and
    /// every later SIGTERM was ignored too (WDY-3784).
    func run(
        onServerRunning: @escaping @Sendable (any Channel) async -> Void = { _ in }
    ) async throws {
        try await self.buildApplication(onServerRunning: onServerRunning)
            .runService(gracefulShutdownSignals: [])
    }
}
