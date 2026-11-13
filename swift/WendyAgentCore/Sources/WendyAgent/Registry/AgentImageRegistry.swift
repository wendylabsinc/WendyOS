import Crypto
import Foundation
import HTTPTypes
import Hummingbird
import HummingbirdCore
import HummingbirdTLS
import Logging
import NIOCore

/// Minimal OCI Distribution v2 server backed by `BlobStore`. Handles the subset
/// docker/container use to push and pull: version check, blob uploads
/// (monolithic + chunked), blob read, manifest put/get.
///
/// Repository names are treated as a single path segment (`{repo}`); nested
/// repository names (e.g. `library/app`) are out of scope for this minimal
/// server. The behavior contract is what matters here — routes, statuses,
/// digest verification, and `Location` headers — verified end-to-end in
/// Task 9.
///
/// One instance serves one listener. The agent runs two over a shared
/// `BlobStore` (see `LocalRegistryRef`): a wildcard push listener (plain HTTP
/// unprovisioned, TLS + client-cert verification provisioned) and a
/// loopback-only plain-HTTP pull listener for the on-device container
/// backends. Blob and manifest writes are digest-addressed and atomic, so two
/// instances over one store directory are concurrent-safe; the in-memory
/// chunked-upload sessions are per-instance, which is fine because only the
/// push listener registers write routes.
struct AgentImageRegistry: Sendable {
    /// One listener's shape: where it binds, whether it terminates TLS, and
    /// which route set it exposes.
    struct ListenerConfiguration: Sendable {
        enum Routes: Sendable {
            /// Full OCI push+pull surface (uploads, manifest PUT, reads).
            case pushAndPull
            /// Read-only surface (blob/manifest GET+HEAD, /v2 check).
            case pullOnly
        }

        var host: String
        var port: Int
        /// Non-nil terminates TLS with the device identity and verifies client
        /// certificates; nil serves plain HTTP.
        var tls: RegistryTLS.Configuration?
        var routes: Routes
        /// Short name ("push"/"pull") for log lines.
        var label: String
    }

    /// Maximum accepted blob size (container image layers can be several GiB).
    private static let maxBlobSize = 8 * 1024 * 1024 * 1024
    /// Maximum accepted manifest size (JSON, always small).
    private static let maxManifestSize = 16 * 1024 * 1024

    private let store: BlobStore
    private let configuration: ListenerConfiguration
    private let logger = Logger(label: "sh.wendy.agent.registry")
    private let uploads = UploadBuffers()

    /// `Docker-Content-Digest` — clients (notably Apple `container`) require this
    /// header on blob and manifest GET/HEAD responses to resolve a tag/reference
    /// to its content digest. The literal is a valid HTTP field token.
    private static let dockerContentDigest = HTTPField.Name("Docker-Content-Digest")!

    /// The canonical `sha256:<hex>` content digest of `data`.
    private static func contentDigest(of data: Data) -> String {
        let hex = SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
        return "sha256:\(hex)"
    }

    /// The manifest's own `mediaType`, defaulting to the OCI image manifest type.
    /// Clients (Apple `container`) require `Content-Type` on both GET *and* HEAD
    /// manifest responses.
    private static func manifestMediaType(_ data: Data) -> String {
        (try? JSONSerialization.jsonObject(with: data) as? [String: Any])?["mediaType"]
            as? String ?? "application/vnd.oci.image.manifest.v1+json"
    }

    init(store: BlobStore, configuration: ListenerConfiguration) {
        self.store = store
        self.configuration = configuration
    }

    /// Accumulates in-progress chunked uploads keyed by upload UUID.
    private actor UploadBuffers {
        private var buffers: [String: Data] = [:]

        func start() -> String {
            let id = UUID().uuidString
            buffers[id] = Data()
            return id
        }

        func append(_ data: Data, to id: String) {
            buffers[id, default: Data()].append(data)
        }

        func take(_ id: String) -> Data? {
            buffers.removeValue(forKey: id)
        }
    }

    /// Builds the route table. Write routes (blob uploads, manifest PUT) are
    /// registered only for `.pushAndPull` listeners; the pull listener exposes
    /// just the read surface the container backends need.
    func buildRouter() -> Router<BasicRequestContext> {
        let router = Router()
        router.addMiddleware {
            LogRequestsMiddleware(.info)
        }
        let store = self.store

        router.get("/v2") { _, _ in
            Response(status: .ok)
        }

        if case .pushAndPull = self.configuration.routes {
            self.addWriteRoutes(to: router)
        }

        router.head("/v2/{repo}/blobs/{digest}") { _, context -> Response in
            let digest = context.parameters.get("digest") ?? ""
            guard BlobStore.isValidSHA256Digest(digest) else { return Response(status: .notFound) }
            guard store.hasBlob(digest: digest) else { return Response(status: .notFound) }
            return Response(
                status: .ok,
                headers: [
                    .contentType: "application/octet-stream",
                    Self.dockerContentDigest: digest,
                ]
            )
        }

        router.get("/v2/{repo}/blobs/{digest}") { _, context -> Response in
            let digest = context.parameters.get("digest") ?? ""
            guard BlobStore.isValidSHA256Digest(digest) else { return Response(status: .notFound) }
            guard let data = store.readBlob(digest: digest) else {
                return Response(status: .notFound)
            }
            return Response(
                status: .ok,
                headers: [
                    .contentType: "application/octet-stream",
                    Self.dockerContentDigest: digest,
                ],
                body: .init(byteBuffer: ByteBuffer(bytes: data))
            )
        }

        router.head("/v2/{repo}/manifests/{reference}") { _, context -> Response in
            let repo = context.parameters.get("repo") ?? ""
            let reference = context.parameters.get("reference") ?? ""
            guard BlobStore.isValidRepository(repo), BlobStore.isValidReference(reference) else {
                return Response(status: .notFound)
            }
            guard let url = store.manifestURL(repository: repo, reference: reference),
                let data = try? Data(contentsOf: url)
            else { return Response(status: .notFound) }
            return Response(
                status: .ok,
                headers: [
                    .contentType: Self.manifestMediaType(data),
                    Self.dockerContentDigest: Self.contentDigest(of: data),
                ]
            )
        }

        router.get("/v2/{repo}/manifests/{reference}") { _, context -> Response in
            let repo = context.parameters.get("repo") ?? ""
            let reference = context.parameters.get("reference") ?? ""
            guard BlobStore.isValidRepository(repo), BlobStore.isValidReference(reference) else {
                return Response(status: .notFound)
            }
            guard let url = store.manifestURL(repository: repo, reference: reference),
                let data = try? Data(contentsOf: url)
            else { return Response(status: .notFound) }
            // Content-Type must echo the stored manifest's mediaType; default to OCI.
            return Response(
                status: .ok,
                headers: [
                    .contentType: Self.manifestMediaType(data),
                    Self.dockerContentDigest: Self.contentDigest(of: data),
                ],
                body: .init(byteBuffer: ByteBuffer(bytes: data))
            )
        }

        return router
    }

    /// Registers the push-side (write) routes: blob uploads and manifest PUT.
    private func addWriteRoutes(to router: Router<BasicRequestContext>) {
        let store = self.store
        let uploads = self.uploads
        let maxBlobSize = Self.maxBlobSize
        let maxManifestSize = Self.maxManifestSize

        // Begin an upload session, or perform a monolithic push when `digest`
        // is supplied on the initial POST (the whole blob is the body).
        router.post("/v2/{repo}/blobs/uploads") { request, context -> Response in
            let repo = context.parameters.get("repo") ?? ""
            if let digest = request.uri.queryParameters["digest"].map(String.init) {
                let buffer = try await request.body.collect(upTo: maxBlobSize)
                do {
                    try store.writeBlob(Data(buffer.readableBytesView), expectedDigest: digest)
                } catch is BlobStore.BlobError {
                    return Response(status: .badRequest)
                }
                return Response(
                    status: .created,
                    headers: [.location: "/v2/\(repo)/blobs/\(digest)"]
                )
            }
            let id = await uploads.start()
            return Response(
                status: .accepted,
                headers: [
                    .location: "/v2/\(repo)/blobs/uploads/\(id)",
                    HTTPField.Name("Docker-Upload-UUID")!: id,
                ]
            )
        }

        // Chunk append (PATCH) — used by chunked pushers.
        router.patch("/v2/{repo}/blobs/uploads/{id}") { request, context -> Response in
            let repo = context.parameters.get("repo") ?? ""
            let id = context.parameters.get("id") ?? ""
            let buffer = try await request.body.collect(upTo: maxBlobSize)
            await uploads.append(Data(buffer.readableBytesView), to: id)
            return Response(
                status: .accepted,
                headers: [.location: "/v2/\(repo)/blobs/uploads/\(id)"]
            )
        }

        // Commit upload (PUT ...?digest=<d>), optionally with a final chunk body.
        router.put("/v2/{repo}/blobs/uploads/{id}") { request, context -> Response in
            let repo = context.parameters.get("repo") ?? ""
            let id = context.parameters.get("id") ?? ""
            let digest = request.uri.queryParameters["digest"].map(String.init) ?? ""
            var data = await uploads.take(id) ?? Data()
            let buffer = try await request.body.collect(upTo: maxBlobSize)
            data.append(Data(buffer.readableBytesView))
            do {
                try store.writeBlob(data, expectedDigest: digest)
            } catch is BlobStore.BlobError {
                return Response(status: .badRequest)
            }
            return Response(
                status: .created,
                headers: [.location: "/v2/\(repo)/blobs/\(digest)"]
            )
        }

        router.put("/v2/{repo}/manifests/{reference}") { request, context -> Response in
            let repo = context.parameters.get("repo") ?? ""
            let reference = context.parameters.get("reference") ?? ""
            guard BlobStore.isValidRepository(repo), BlobStore.isValidReference(reference) else {
                return Response(status: .badRequest)
            }
            let buffer = try await request.body.collect(upTo: maxManifestSize)
            try store.writeManifest(
                Data(buffer.readableBytesView),
                repository: repo,
                reference: reference
            )
            return Response(status: .created)
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
    ) throws -> Application<RouterResponder<BasicRequestContext>> {
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
