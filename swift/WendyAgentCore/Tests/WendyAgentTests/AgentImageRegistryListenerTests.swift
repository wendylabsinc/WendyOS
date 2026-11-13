import Crypto
import Foundation
import Hummingbird
import HummingbirdTesting
import NIOCore
import Testing

@testable import WendyAgentCore

@Suite("AgentImageRegistry listeners")
struct AgentImageRegistryListenerTests {
    private static func makeStore() -> BlobStore {
        BlobStore(
            root: FileManager.default.temporaryDirectory
                .appendingPathComponent("registry-\(UUID().uuidString)")
        )
    }

    private static func digest(of data: Data) -> String {
        "sha256:" + SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }

    private static func listener(
        _ routes: AgentImageRegistry.ListenerConfiguration.Routes,
        store: BlobStore
    ) -> AgentImageRegistry {
        AgentImageRegistry(
            store: store,
            configuration: .init(
                host: "127.0.0.1",
                port: 0,
                tls: nil,
                routes: routes,
                label: "test"
            )
        )
    }

    @Test("pull-only listener serves reads but exposes no write routes")
    func pullOnlyListenerIsReadOnly() async throws {
        let store = Self.makeStore()
        let blob = Data("layer-bytes".utf8)
        let blobDigest = Self.digest(of: blob)
        try store.writeBlob(blob, expectedDigest: blobDigest)
        let manifest = Data(#"{"mediaType":"application/vnd.oci.image.manifest.v1+json"}"#.utf8)
        try store.writeManifest(manifest, repository: "app", reference: "latest")

        let app = try Self.listener(.pullOnly, store: store).buildApplication()
        try await app.test(.live) { client in
            try await client.execute(uri: "/v2", method: .get) { response in
                #expect(response.status == .ok)
            }
            try await client.execute(uri: "/v2/app/blobs/\(blobDigest)", method: .head) {
                response in
                #expect(response.status == .ok)
            }
            try await client.execute(uri: "/v2/app/blobs/\(blobDigest)", method: .get) { response in
                #expect(response.status == .ok)
                #expect(response.body == ByteBuffer(bytes: blob))
            }
            try await client.execute(uri: "/v2/app/manifests/latest", method: .get) { response in
                #expect(response.status == .ok)
                #expect(response.body == ByteBuffer(bytes: manifest))
            }
            // Write routes must not exist on the pull listener.
            try await client.execute(uri: "/v2/app/blobs/uploads", method: .post) { response in
                #expect(response.status == .notFound)
            }
            try await client.execute(
                uri: "/v2/app/manifests/latest",
                method: .put,
                body: .init(data: manifest)
            ) { response in
                #expect(response.status == .notFound)
            }
        }
    }

    @Test("push listener completes monolithic and chunked pushes")
    func pushListenerAcceptsPushes() async throws {
        let store = Self.makeStore()
        let app = try Self.listener(.pushAndPull, store: store).buildApplication()
        try await app.test(.live) { client in
            // Monolithic: POST with ?digest= carries the whole blob.
            let blob = Data("monolithic-layer".utf8)
            let blobDigest = Self.digest(of: blob)
            try await client.execute(
                uri: "/v2/app/blobs/uploads?digest=\(blobDigest)",
                method: .post,
                body: .init(data: blob)
            ) { response in
                #expect(response.status == .created)
            }
            #expect(store.hasBlob(digest: blobDigest))

            // Chunked: POST starts a session, PATCH appends, PUT commits.
            let chunked = Data("chunked-layer-contents".utf8)
            let chunkedDigest = Self.digest(of: chunked)
            let location = try await client.execute(
                uri: "/v2/app/blobs/uploads",
                method: .post
            ) { response -> String in
                #expect(response.status == .accepted)
                return try #require(response.headers[.location])
            }
            let half = chunked.count / 2
            try await client.execute(
                uri: location,
                method: .patch,
                body: .init(data: chunked.prefix(half))
            ) { response in
                #expect(response.status == .accepted)
            }
            try await client.execute(
                uri: "\(location)?digest=\(chunkedDigest)",
                method: .put,
                body: .init(data: chunked.suffix(from: half))
            ) { response in
                #expect(response.status == .created)
            }
            #expect(store.hasBlob(digest: chunkedDigest))

            // Manifest PUT then read-back.
            let manifest = Data(
                #"{"mediaType":"application/vnd.oci.image.manifest.v1+json"}"#.utf8
            )
            try await client.execute(
                uri: "/v2/app/manifests/latest",
                method: .put,
                body: .init(data: manifest)
            ) { response in
                #expect(response.status == .created)
            }
            try await client.execute(uri: "/v2/app/manifests/latest", method: .get) { response in
                #expect(response.status == .ok)
                #expect(response.body == ByteBuffer(bytes: manifest))
            }
        }
    }

    @Test("run() serves until its task is cancelled, then stops listening")
    func runServesUntilCancelled() async throws {
        let (bound, boundContinuation) = AsyncStream.makeStream(of: Int.self)
        let registry = Self.listener(.pullOnly, store: Self.makeStore())
        let task = Task {
            try await registry.run { channel in
                boundContinuation.yield(channel.localAddress?.port ?? 0)
            }
        }
        var ports = bound.makeAsyncIterator()
        let port = try #require(await ports.next())
        let url = try #require(URL(string: "http://127.0.0.1:\(port)/v2"))

        let (_, response) = try await URLSession.shared.data(from: url)
        #expect((response as? HTTPURLResponse)?.statusCode == 200)

        task.cancel()
        // How run() ends is racy: when Hummingbird's date-cache timer sees the
        // cancellation first, its service group reports it as an unexpected
        // finish. The agent only relies on the listener going away.
        _ = try? await task.value
        await #expect(throws: (any Error).self) {
            _ = try await URLSession.shared.data(from: url)
        }
    }

    /// The listener is one part of the agent process. If it trapped SIGTERM,
    /// `killall` would stop only the registry and leave the agent running
    /// (WDY-3784). Runs in a child process: HummingbirdTesting's servers trap
    /// SIGTERM in this one.
    @Test("SIGTERM still ends the process while a listener runs")
    func sigtermEndsTheProcessWhileListening() async {
        await #expect(processExitsWith: .signal(SIGTERM)) {
            let (bound, boundContinuation) = AsyncStream.makeStream(of: Void.self)
            let registry = AgentImageRegistryListenerTests.listener(
                .pullOnly,
                store: AgentImageRegistryListenerTests.makeStore()
            )
            let task = Task {
                try await registry.run { _ in boundContinuation.yield() }
            }
            for await _ in bound { break }

            kill(getpid(), SIGTERM)
            // Reached only if the signal was trapped; exiting normally fails the test.
            try? await Task.sleep(for: .seconds(5))
            task.cancel()
        }
    }
}

extension ByteBuffer {
    fileprivate init(data: Data) {
        self.init(bytes: data)
    }
}
