import AsyncAlgorithms
import ContainerdGRPC
import Crypto
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import GRPCServiceLifecycle
import HTTPTypes
import Hummingbird
import Logging
import NIOFoundationCompat
import OCIRegistryOpenAPI
import OpenAPIRuntime
import Synchronization

private actor UploadSessionStore {
    struct Session: Sendable {
        let repository: String
        let ref: String
        var offset: Int64
        var write:
            @Sendable (Containerd_Services_Content_V1_WriteContentRequest) async throws -> Void
        let finish: @Sendable () async -> Void
    }

    private var sessions: [String: Session] = [:]

    func insert(
        uuid: String,
        repository: String,
        ref: String,
        write:
            @Sendable @escaping (Containerd_Services_Content_V1_WriteContentRequest) async throws ->
            Void,
        finish: @Sendable @escaping () async -> Void
    ) {
        sessions[uuid] = Session(
            repository: repository,
            ref: ref,
            offset: 0,
            write: write,
            finish: finish
        )
    }

    func session(for uuid: String) -> Session? {
        sessions[uuid]
    }

    func updateOffset(_ offset: Int64, for uuid: String) {
        guard var session = sessions[uuid] else { return }
        session.offset = offset
        sessions[uuid] = session
    }

    func firstSession(for repository: String) -> Session? {
        sessions.values.first { $0.repository == repository }
    }

    func remove(uuid: String) {
        sessions.removeValue(forKey: uuid)
    }
}

public struct RegistryAPI: APIProtocol {
    public let client: GRPCClient<HTTP2ClientTransport.Posix>

    public init(client: GRPCClient<HTTP2ClientTransport.Posix>) {
        self.client = client
    }

    private static let uploadSessions = UploadSessionStore()

    public func listTags(
        _ input: OCIRegistryOpenAPI.Operations.ListTags.Input
    ) async throws -> OCIRegistryOpenAPI.Operations.ListTags.Output {
        let repositoryName = input.path.name
        let imagesClient = Containerd_Services_Images_V1_Images.Client(wrapping: self.client)

        do {
            let response = try await imagesClient.list(.with { _ in })
            let tagPrefix = repositoryName + ":"

            var tags = Set<String>()
            var repositoryExists = false

            for image in response.images {
                if image.name == repositoryName || image.name.hasPrefix(repositoryName + "@") {
                    repositoryExists = true
                }

                guard image.name.hasPrefix(tagPrefix) else {
                    continue
                }

                repositoryExists = true

                let tag = String(image.name.dropFirst(tagPrefix.count))
                guard !tag.isEmpty else {
                    continue
                }
                tags.insert(tag)
            }

            guard repositoryExists else {
                return .notFound(.init(body: .json(.init())))
            }

            var sortedTags = tags.sorted()
            if let last = input.query.last, !last.isEmpty {
                sortedTags.removeAll { $0 <= last }
            }

            if let limit = input.query.n, limit >= 0 {
                sortedTags = Array(sortedTags.prefix(limit))
            }

            return .ok(
                .init(
                    body: .json(
                        .init(
                            name: repositoryName,
                            tags: sortedTags
                        )
                    )
                )
            )
        } catch let error as RPCError where error.code == .notFound {
            return .notFound(.init(body: .json(.init())))
        }
    }

    public func cancelBlobUpload(
        _ input: OCIRegistryOpenAPI.Operations.CancelBlobUpload.Input
    ) async throws -> OCIRegistryOpenAPI.Operations.CancelBlobUpload.Output {
        let repositoryName = input.path.name
        let uuid = input.path.uuid

        guard let session = await Self.uploadSessions.session(for: uuid),
            session.repository == repositoryName
        else {
            return .notFound(.init(body: .json(.init())))
        }

        let contentClient = Containerd_Services_Content_V1_Content.Client(wrapping: self.client)

        do {
            _ = try await contentClient.abort(
                .with { request in
                    request.ref = session.ref
                }
            )
        } catch let error as RPCError where error.code == .notFound {
            _ = await Self.uploadSessions.remove(uuid: uuid)
            return .notFound(.init(body: .json(.init())))
        }

        await Self.uploadSessions.remove(uuid: uuid)
        return .noContent(.init())
    }

    public func completeBlobUpload(
        _ input: OCIRegistryOpenAPI.Operations.CompleteBlobUpload.Input
    ) async throws -> OCIRegistryOpenAPI.Operations.CompleteBlobUpload.Output {
        let logger = Logger(label: "sh.wendyengineer.containerd-registry.complete-blob-upload")
        let repositoryName = input.path.name
        let uuid = input.path.uuid
        let digest = input.query.digest

        guard isDigestReference(digest) else {
            return .badRequest(
                .init(
                    body: .json(
                        .init(
                            errors: [
                                .init(code: nil, message: "Not a digest reference", detail: nil)
                            ]
                        )
                    )
                )
            )
        }

        guard var session = await Self.uploadSessions.session(for: uuid),
            session.repository == repositoryName
        else {
            return .notFound(.init(body: .json(.init())))
        }

        let body = input.body

        do {
            if let body, case .binary(let httpBody) = body {
                for try await chunk in httpBody {
                    guard !chunk.isEmpty else { continue }
                    let data = Data(chunk)
                    try await session.write(
                        .with { request in
                            request.action = .write
                            request.ref = session.ref
                            request.offset = session.offset
                            request.data = data
                        }
                    )
                    session.offset += Int64(data.count)
                }
            }

            logger.info(
                "Committing blob upload session",
                metadata: [
                    "session-id": .string(uuid), "digest": .string(digest),
                    "bytes": .stringConvertible(session.offset),
                ]
            )
            try await session.write(
                .with { request in
                    request.action = .commit
                    request.ref = session.ref
                    request.offset = session.offset
                    request.expected = digest
                    request.labels = [
                        "containerd.io/gc.root": "true",
                        "sh.wendy.layer": "true",
                    ]
                }
            )
            await session.finish()
            await Self.uploadSessions.remove(uuid: uuid)

            // Verify the blob actually exists before returning success
            // This ensures the commit completed successfully and the blob is available
            let contentClient = Containerd_Services_Content_V1_Content.Client(wrapping: self.client)
            do {
                _ = try await contentClient.info(
                    .with { request in
                        request.digest = digest
                    }
                )
            } catch let error as RPCError where error.code == .notFound {
                // Blob doesn't exist yet, wait a bit and retry
                // This handles potential timing issues with containerd
                try await Task.sleep(nanoseconds: 100_000_000)  // 100ms
                do {
                    _ = try await contentClient.info(
                        .with { request in
                            request.digest = digest
                        }
                    )
                } catch let retryError as RPCError where retryError.code == .notFound {
                    return .badRequest(
                        .init(
                            body: .json(
                                .init(
                                    errors: [
                                        .init(
                                            code: nil,
                                            message:
                                                "Blob commit completed but blob is not available",
                                            detail: nil
                                        )
                                    ]
                                )
                            )
                        )
                    )
                }
            }
        } catch let error as RPCError {
            switch error.code {
            case .notFound:
                return .notFound(.init(body: .json(.init())))
            case .alreadyExists:
                // treat as success; verify blob exists
                let contentClient = Containerd_Services_Content_V1_Content.Client(
                    wrapping: self.client
                )
                do {
                    _ = try await contentClient.info(
                        .with { request in
                            request.digest = digest
                        }
                    )
                } catch let verifyError as RPCError where verifyError.code == .notFound {
                    return .badRequest(
                        .init(
                            body: .json(
                                .init(
                                    errors: [
                                        .init(
                                            code: nil,
                                            message: "Blob already exists but is not available",
                                            detail: nil
                                        )
                                    ]
                                )
                            )
                        )
                    )
                }
            case .failedPrecondition, .invalidArgument:
                return .badRequest(
                    .init(
                        body: .json(
                            .init(
                                errors: [
                                    .init(
                                        code: nil,
                                        message: "Failed to complete blob upload: \(error.message)",
                                        detail: nil
                                    )
                                ]
                            )
                        )
                    )
                )
            default:
                throw error
            }
        }

        _ = await Self.uploadSessions.remove(uuid: uuid)

        return .created(
            .init(
                headers: .init(
                    location: digest,
                    dockerContentDigest: digest
                )
            )
        )
    }

    public func patchBlobUpload(
        _ input: OCIRegistryOpenAPI.Operations.PatchBlobUpload.Input
    ) async throws -> OCIRegistryOpenAPI.Operations.PatchBlobUpload.Output {
        let repositoryName = input.path.name
        let uuid = input.path.uuid

        guard var session = await Self.uploadSessions.session(for: uuid),
            session.repository == repositoryName
        else {
            return .badRequest(
                .init(
                    body: .json(
                        .init(
                            errors: [
                                .init(
                                    code: nil,
                                    message: "Failed to complete blob upload",
                                    detail: nil
                                )
                            ]
                        )
                    )
                )
            )
        }

        guard case .binary(let body) = input.body else {
            return .badRequest(
                .init(
                    body: .json(
                        .init(
                            errors: [
                                .init(code: nil, message: "Invalid body", detail: nil)
                            ]
                        )
                    )
                )
            )
        }

        let contentRange: (start: Int64, end: Int64)?
        do {
            contentRange = try parseContentRange(input.headers.contentRange)
        } catch {
            return .rangeNotSatisfiable(.init())
        }

        let contentClient = Containerd_Services_Content_V1_Content.Client(wrapping: self.client)
        do {
            try await streamChunk(
                from: body,
                session: &session,
                contentClient: contentClient
            )
            // Persist the updated offset back to the store
            await Self.uploadSessions.updateOffset(session.offset, for: uuid)
        } catch {
            return .badRequest(
                .init(
                    body: .json(
                        .init(
                            errors: [
                                .init(
                                    code: nil,
                                    message:
                                        "Failed to patch blob upload: \(error.localizedDescription)",
                                    detail: nil
                                )
                            ]
                        )
                    )
                )
            )
        }

        if let range = contentRange {
            let expectedEnd = range.end
            let actualEnd = session.offset
            guard actualEnd == expectedEnd else {
                return .rangeNotSatisfiable(.init())
            }
        }

        return .accepted(
            .init(
                headers: .init(
                    location: uuid,
                    range: rangeHeaderValue(forUploadedBytes: session.offset)
                )
            )
        )
    }

    public func resumeBlobUpload(
        _ input: OCIRegistryOpenAPI.Operations.ResumeBlobUpload.Input
    ) async throws -> OCIRegistryOpenAPI.Operations.ResumeBlobUpload.Output {
        let repositoryName = input.path.name
        let uuid = input.path.uuid

        guard let session = await Self.uploadSessions.session(for: uuid),
            session.repository == repositoryName
        else {
            return .notFound(.init(body: .json(.init())))
        }

        let range = rangeHeaderValue(forUploadedBytes: session.offset)

        return .noContent(.init(headers: .init(range: range)))
    }

    public func initiateBlobUpload(
        _ input: OCIRegistryOpenAPI.Operations.InitiateBlobUpload.Input
    ) async throws -> OCIRegistryOpenAPI.Operations.InitiateBlobUpload.Output {
        let repositoryName = input.path.name
        let uuid = UUID().uuidString.lowercased()

        let logger = Logger(label: "sh.wendyengineer.containerd-registry.initiate-blob-upload")
        let contentClient = Containerd_Services_Content_V1_Content.Client(wrapping: self.client)

        Task {
            try await contentClient.write { writer in
                let (stream, continuation) = AsyncStream<Void>.makeStream()
                await Self.uploadSessions.insert(
                    uuid: uuid,
                    repository: repositoryName,
                    ref: uuid,
                    write: { message in
                        do {
                            try await writer.write(message)
                        } catch {
                            logger.error(
                                "Failed to write chunk",
                                metadata: ["error": "\(error)", "session-id": .string(uuid)]
                            )
                            continuation.finish()
                            throw error
                        }
                    },
                    finish: {
                        logger.info(
                            "Finished writing blob upload session",
                            metadata: ["session-id": .string(uuid)]
                        )
                        continuation.finish()
                    }
                )

                for await _ in stream {}
            } onResponse: { response in
                do {
                    for try await _ in response.messages {}
                    return ()
                } catch {
                    logger.error(
                        "Failed to read response",
                        metadata: ["error": "\(error)", "session-id": .string(uuid)]
                    )
                    throw error
                }
            }
        }

        return .accepted(
            .init(
                headers: .init(
                    location: uuid,
                    range: rangeHeaderValue(forUploadedBytes: 0)
                )
            )
        )
    }

    public func getBlobUploadStatus(
        _ input: OCIRegistryOpenAPI.Operations.GetBlobUploadStatus.Input
    ) async throws -> OCIRegistryOpenAPI.Operations.GetBlobUploadStatus.Output {
        let repositoryName = input.path.name

        guard let session = await Self.uploadSessions.firstSession(for: repositoryName) else {
            return .notFound(.init(body: .json(.init())))
        }

        return .noContent(
            .init(headers: .init(range: rangeHeaderValue(forUploadedBytes: session.offset)))
        )
    }

    public func deleteBlob(
        _ input: OCIRegistryOpenAPI.Operations.DeleteBlob.Input
    ) async throws -> OCIRegistryOpenAPI.Operations.DeleteBlob.Output {
        let content = Containerd_Services_Content_V1_Content.Client(wrapping: self.client)
        do {
            _ = try await content.delete(
                .with {
                    $0.digest = input.path.digest
                }
            )
            return .accepted(.init())
        } catch let error as RPCError where error.code == .notFound {
            return .notFound(.init(body: .json(.init())))
        }
    }

    public func headManifest(
        _ input: Operations.HeadManifest.Input
    ) async throws -> Operations.HeadManifest.Output {
        do {
            let content = Containerd_Services_Content_V1_Content.Client(wrapping: self.client)
            let manifestInfo = try await content.info(
                .with {
                    $0.digest = input.path.reference
                }
            )
            return .ok(
                .init(
                    headers: .init(
                        dockerContentDigest: input.path.reference,
                        contentLength: Int(manifestInfo.info.size)
                    )
                )
            )
        } catch let error as RPCError where error.code == .notFound {
            return .notFound(.init(body: .json(.init())))
        }
    }

    public func headBlob(
        _ input: Operations.HeadBlob.Input
    ) async throws -> Operations.HeadBlob.Output {
        do {
            let content = Containerd_Services_Content_V1_Content.Client(wrapping: self.client)
            let blobInfo = try await content.info(
                .with {
                    $0.digest = input.path.digest
                }
            )
            return .ok(
                .init(
                    headers: .init(
                        contentLength: Int(blobInfo.info.size),
                        dockerContentDigest: input.path.digest
                    )
                )
            )
        } catch let error as RPCError where error.code == .notFound {
            return .notFound(.init(body: .json(.init())))
        }
    }

    public func getBlob(
        _ input: OCIRegistryOpenAPI.Operations.GetBlob.Input
    ) async throws -> OCIRegistryOpenAPI.Operations.GetBlob.Output {
        do {
            let content = Containerd_Services_Content_V1_Content.Client(wrapping: self.client)
            let blobInfo = try await content.info(
                .with {
                    $0.digest = input.path.digest
                }
            )
            let range = input.headers.range.flatMap { range in
                range.split(separator: "=").last
            }.flatMap { byteRange -> (offset: Int64, size: Int64?)? in
                let parts = byteRange.split(separator: "-")
                guard let first = parts.first, let offset = Int64(first) else {
                    return nil
                }

                guard parts.count == 2, let last = parts.last, let size = Int64(last) else {
                    return (offset, nil)
                }
                return (offset, size)
            }
            let expectedLength: HTTPBody.Length
            let contentLength: Int

            if let range {
                if let size = range.size {
                    expectedLength = .known(size)
                    contentLength = Int(size)
                } else {
                    let remaining = max(blobInfo.info.size - range.offset, 0)
                    expectedLength = .known(remaining)
                    contentLength = Int(remaining)
                }
            } else {
                expectedLength = .known(blobInfo.info.size)
                contentLength = Int(blobInfo.info.size)
            }

            let body = try await content.read(
                .with {
                    $0.digest = input.path.digest
                    if let range {
                        $0.offset = range.offset
                        $0.size = range.size ?? 0
                    }
                }
            ) { response in
                let sequence = response.messages.map { $0.data }
                return HTTPBody(sequence, length: expectedLength, iterationBehavior: .single)
            }

            return .ok(
                .init(
                    headers: .init(
                        contentLength: contentLength,
                        dockerContentDigest: input.path.digest
                    ),
                    body: .binary(body)
                )
            )
        } catch let error as RPCError where error.code == .notFound {
            return .notFound(.init(body: .json(.init())))
        }
    }

    public func deleteManifest(
        _ input: OCIRegistryOpenAPI.Operations.DeleteManifest.Input
    ) async throws -> OCIRegistryOpenAPI.Operations.DeleteManifest.Output {
        let repositoryName = input.path.name
        let reference = input.path.reference
        let imagesClient = Containerd_Services_Images_V1_Images.Client(wrapping: self.client)
        let contentClient = Containerd_Services_Content_V1_Content.Client(wrapping: self.client)
        let isDigestReference = self.isDigestReference(reference)
        var imagesByName: [String: Containerd_Services_Images_V1_Image] = [:]
        var digestToDelete: String?

        if isDigestReference {
            digestToDelete = reference
        } else {
            let taggedName = repositoryName + ":" + reference
            do {
                let image = try await imagesClient.get(
                    .with {
                        $0.name = taggedName
                    }
                ).image
                guard image.hasTarget else {
                    return .notFound(.init(body: .json(.init())))
                }
                digestToDelete = image.target.digest
                imagesByName[taggedName] = image
            } catch let error as RPCError where error.code == .notFound {
                return .notFound(.init(body: .json(.init())))
            }
        }

        guard let digest = digestToDelete else {
            return .notFound(.init(body: .json(.init())))
        }

        var manifestExists = false
        do {
            _ = try await contentClient.info(
                .with { info in
                    info.digest = digest
                }
            )
            manifestExists = true
        } catch let error as RPCError where error.code == .notFound {
            manifestExists = false
        }

        do {
            let response = try await imagesClient.list(.with { _ in })
            for image in response.images {
                guard image.hasTarget else { continue }
                guard image.target.digest == digest else { continue }
                guard imageBelongsToRepository(image.name, repository: repositoryName) else {
                    continue
                }
                imagesByName[image.name] = image
            }
        } catch let error as RPCError where error.code == .notFound {
            // If the image service reports not found on list, treat as no images.
        }

        if imagesByName.isEmpty && !manifestExists {
            return .notFound(.init(body: .json(.init())))
        }

        var deletedAny = false
        for (name, image) in imagesByName {
            do {
                _ = try await imagesClient.delete(
                    .with { request in
                        request.name = name
                        if image.hasTarget {
                            request.target = image.target
                        }
                    }
                )
                deletedAny = true
            } catch let error as RPCError where error.code == .notFound {
                continue
            }
        }

        if manifestExists {
            do {
                _ = try await contentClient.delete(
                    .with { request in
                        request.digest = digest
                    }
                )
                deletedAny = true
            } catch let error as RPCError where error.code == .notFound {
                // Manifest already removed; ignore.
            }
        }

        if deletedAny {
            return .accepted(.init())
        } else {
            return .notFound(.init(body: .json(.init())))
        }
    }

    // public func putManifest(_ input: OCIRegistryOpenAPI.Operations.PutManifest.Input) async throws -> OCIRegistryOpenAPI.Operations.PutManifest.Output {
    public func putManifest(
        _ request: Request,
        context: some RequestContext
    ) async throws -> Response {
        let repositoryName = try context.parameters.require("name")
        let reference = try context.parameters.require("reference")

        let contentClient = Containerd_Services_Content_V1_Content.Client(wrapping: self.client)
        let imagesClient = Containerd_Services_Images_V1_Images.Client(wrapping: self.client)
        let logger = Logger(label: "sh.wendyengineer.containerd-registry.put-manifest")

        // Get the raw manifest bytes from the request body
        // This ensures we compute the digest from Docker's exact bytes
        let manifestMediaType: String
        let manifestAnnotations: [String: String]
        let dependentDescriptors: [Components.Schemas.Descriptor]

        // Body is now binary (raw bytes) - collect Docker's exact bytes
        var body = ByteBuffer()
        for try await chunk in request.body {
            body.writeImmutableBuffer(chunk)
        }
        let manifestData = Data(buffer: body)

        switch request.headers[.contentType] {
        case "application/vnd.docker.distribution.manifest.v2+json":
            let dockerManifest = try JSONDecoder().decode(
                Components.Schemas.DockerManifest.self,
                from: manifestData
            )
            manifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"
            manifestAnnotations = [:]
            dependentDescriptors = [dockerManifest.config] + dockerManifest.layers
        case "application/vnd.oci.image.manifest.v1+json":
            let ociManifest = try JSONDecoder().decode(
                Components.Schemas.Manifest.self,
                from: manifestData
            )
            manifestMediaType = "application/vnd.oci.image.manifest.v1+json"
            manifestAnnotations = ociManifest.annotations?.additionalProperties ?? [:]
            dependentDescriptors = [ociManifest.config] + ociManifest.layers
        default:
            do {
                let dockerManifest = try JSONDecoder().decode(
                    Components.Schemas.DockerManifest.self,
                    from: manifestData
                )
                manifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"
                manifestAnnotations = [:]
                dependentDescriptors = [dockerManifest.config] + dockerManifest.layers
            } catch {
                let ociManifest = try JSONDecoder().decode(
                    Components.Schemas.Manifest.self,
                    from: manifestData
                )
                manifestMediaType = "application/vnd.oci.image.manifest.v1+json"
                manifestAnnotations = ociManifest.annotations?.additionalProperties ?? [:]
                dependentDescriptors = [ociManifest.config] + ociManifest.layers
            }
        }

        // Compute digest from Docker's exact bytes (not re-encoded)
        let manifestDigest = computeDigest(for: manifestData)
        let manifestSize = Int64(manifestData.count)

        logger.info(
            "Computed manifest digest from raw bytes",
            metadata: [
                "digest": .string(manifestDigest),
                "digest-length": .stringConvertible(manifestDigest.count),
                "manifest-size": .stringConvertible(manifestSize),
                "reference": .string(reference),
            ]
        )

        if self.isDigestReference(reference) && reference != manifestDigest {
            throw HTTPError(.badRequest, message: "Reference digest does not match manifest digest")
        }

        for descriptor in dependentDescriptors {
            do {
                logger.debug(
                    "Checking dependent descriptor",
                    metadata: ["digest": .string(descriptor.digest)]
                )
                _ = try await contentClient.info(
                    .with { request in
                        request.digest = descriptor.digest
                    }
                )
            } catch let error as RPCError where error.code == .notFound {
                logger.info(
                    "Dependent descriptor not found",
                    metadata: ["digest": .string(descriptor.digest)]
                )
                throw HTTPError(.notFound, message: "Dependent descriptor not found")
            } catch let error as RPCError where error.code == .invalidArgument {
                logger.error(
                    "Invalid digest format for dependent descriptor",
                    metadata: [
                        "error": "\(error)", "digest": .string(descriptor.digest),
                        "digest-length": .stringConvertible(descriptor.digest.count),
                    ]
                )
                throw HTTPError(.badRequest, message: "Invalid digest format: \(descriptor.digest)")
            } catch {
                logger.error(
                    "Failed to get dependent descriptor info",
                    metadata: ["error": "\(error)", "digest": .string(descriptor.digest)]
                )
                throw error
            }
        }

        var manifestExists = false
        do {
            _ = try await contentClient.info(
                .with { request in
                    request.digest = manifestDigest
                }
            )
            manifestExists = true
        } catch let error as RPCError where error.code == .notFound {
            logger.info("Manifest not found", metadata: ["digest": .string(manifestDigest)])
        } catch {
            logger.error(
                "Failed to get manifest info",
                metadata: ["error": "\(error)", "digest": .string(manifestDigest)]
            )
            throw error
        }

        if !manifestExists {
            let writeRef = "manifest-\(UUID().uuidString)"
            let referencedDigests = dependentDescriptors.map(\.digest)
            let labels = Dictionary(
                uniqueKeysWithValues: referencedDigests.enumerated().map { index, digest in
                    ("containerd.io/gc.ref.content.\(index)", digest)
                }
            )

            do {
                logger.info(
                    "Writing manifest",
                    metadata: [
                        "digest": .string(manifestDigest), "size": .stringConvertible(manifestSize),
                    ]
                )
                try await contentClient.write(
                    requestProducer: { [manifestData] writer in
                        try await writer.write(
                            Containerd_Services_Content_V1_WriteContentRequest.with { request in
                                request.action = .write
                                request.ref = writeRef
                                request.total = manifestSize
                                request.expected = manifestDigest
                                request.offset = 0
                                request.data = manifestData
                                request.labels = labels
                            }
                        )
                        try await writer.write(
                            Containerd_Services_Content_V1_WriteContentRequest.with { request in
                                request.action = .commit
                                request.ref = writeRef
                                request.offset = manifestSize
                                request.expected = manifestDigest
                            }
                        )
                    },
                    onResponse: { response in
                        for try await _ in response.messages {}
                        return ()
                    }
                )
            } catch let error as RPCError where error.code == .alreadyExists {
                // Content already committed; continue.
                logger.info(
                    "Content already committed, skipping",
                    metadata: ["digest": .string(manifestDigest)]
                )
            } catch {
                logger.error(
                    "Failed to write manifest",
                    metadata: ["error": "\(error)", "digest": .string(manifestDigest)]
                )
                throw error
            }
        }

        let targetDescriptor = Containerd_Types_Descriptor.with { descriptor in
            descriptor.mediaType = manifestMediaType
            descriptor.digest = manifestDigest
            descriptor.size = manifestSize
            descriptor.annotations = manifestAnnotations
        }

        func upsertImage(named name: String) async throws {
            let image = Containerd_Services_Images_V1_Image.with { image in
                image.name = name
                image.target = targetDescriptor
            }

            do {
                logger.debug(
                    "Creating image",
                    metadata: ["name": .string(name), "digest": .string(targetDescriptor.digest)]
                )
                _ = try await imagesClient.create(
                    .with { request in
                        request.image = image
                    }
                )
            } catch let error as RPCError {
                if error.code == .alreadyExists {
                    logger.debug(
                        "Image already exists, updating",
                        metadata: [
                            "name": .string(name), "digest": .string(targetDescriptor.digest),
                        ]
                    )
                    _ = try await imagesClient.update(
                        .with { request in
                            request.image = image
                            request.updateMask.paths = ["target"]
                        }
                    )
                } else {
                    logger.error(
                        "Invalid argument when creating image",
                        metadata: [
                            "error": "\(error)", "name": .string(name),
                            "digest": .string(targetDescriptor.digest),
                        ]
                    )
                    throw error
                }
            } catch {
                logger.error(
                    "Failed to upsert image",
                    metadata: ["error": "\(error)", "name": .string(name)]
                )
                throw error
            }
        }

        var imageNames: Set<String> = [repositoryName]
        imageNames.insert("\(repositoryName)@\(manifestDigest)")
        if self.isDigestReference(reference) {
            imageNames.insert("\(repositoryName)@\(reference)")
        } else {
            imageNames.insert("\(repositoryName):\(reference)")
        }

        for name in imageNames {
            try await upsertImage(named: name)
        }

        return Response(
            status: .created,
            headers: [
                .location: reference,
                HTTPField.Name("Docker-Content-Digest")!: manifestDigest,
            ]
        )
    }

    public func getManifest(
        _ input: OCIRegistryOpenAPI.Operations.GetManifest.Input
    ) async throws -> OCIRegistryOpenAPI.Operations.GetManifest.Output {
        do {
            let images = Containerd_Services_Images_V1_Images.Client(wrapping: self.client)
            let content = Containerd_Services_Content_V1_Content.Client(wrapping: self.client)
            let image = try await images.get(
                .with {
                    $0.name = input.path.name
                }
            ).image
            let manifest = try await content.read(
                .with {
                    $0.digest = image.target.digest
                }
            ) { manifest in
                var data = Data()
                for try await message in manifest.messages {
                    data.append(message.data)
                }
                return try JSONDecoder().decode(ImageManifest.self, from: data)
            }

            return .ok(
                .init(
                    headers: .init(
                        dockerContentDigest: image.target.digest,
                        contentType: image.target.mediaType
                    ),
                    body: .applicationVnd_oci_image_manifest_v1Json(
                        .init(
                            schemaVersion: manifest.schemaVersion,
                            config: .init(
                                mediaType: image.target.mediaType,
                                size: Int(image.target.size),
                                digest: image.target.digest,
                                annotations: .init(additionalProperties: image.target.annotations)
                            ),
                            layers: manifest.layers.map { layer in
                                return .init(
                                    mediaType: layer.mediaType,
                                    size: Int(layer.size),
                                    digest: layer.digest,
                                    annotations: layer.annotations.map { annotations in
                                        return .init(additionalProperties: annotations)
                                    }
                                )
                            },
                            annotations: .init(additionalProperties: image.target.annotations)
                        )
                    )
                )
            )
        } catch let error as RPCError where error.code == .notFound {
            return .notFound(.init(body: .json(.init())))
        }
    }

    public func checkVersionSupport(
        _ input: OCIRegistryOpenAPI.Operations.CheckVersionSupport.Input
    ) async throws -> OCIRegistryOpenAPI.Operations.CheckVersionSupport.Output {
        return .ok(.init(headers: .init(dockerDistributionAPIVersion: "registry/2.0")))
    }

    private static let digestAlgorithmCharacters = Set(
        "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+_.-"
    )
    private static let digestHexCharacters = Set("0123456789abcdefABCDEF")

    private func isDigestReference(_ reference: String) -> Bool {
        let parts = reference.split(separator: ":", maxSplits: 1, omittingEmptySubsequences: false)
        guard parts.count == 2,
            !parts[0].isEmpty,
            !parts[1].isEmpty
        else {
            return false
        }
        return parts[0].allSatisfy { Self.digestAlgorithmCharacters.contains($0) }
            && parts[1].allSatisfy { Self.digestHexCharacters.contains($0) }
    }

    private func imageBelongsToRepository(_ imageName: String, repository: String) -> Bool {
        if imageName == repository {
            return true
        }
        guard imageName.hasPrefix(repository) else {
            return false
        }
        let boundaryIndex = imageName.index(imageName.startIndex, offsetBy: repository.count)
        guard boundaryIndex < imageName.endIndex else {
            return false
        }
        let separator = imageName[boundaryIndex]
        return separator == ":" || separator == "@"
    }

    private func computeDigest(for data: Data) -> String {
        let digest = SHA256.hash(data: data)
        let hex = digest.map { String(format: "%02x", $0) }.joined()
        return "sha256:\(hex)"
    }

    private func rangeHeaderValue(forUploadedBytes uploadedBytes: Int64) -> String {
        guard uploadedBytes > 0 else {
            return "0-0"
        }
        return "0-\(uploadedBytes - 1)"
    }

    private func parseContentRange(_ rawValue: String?) throws -> (start: Int64, end: Int64)? {
        guard let rawValue else {
            return nil
        }

        let trimmed = rawValue.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else {
            throw UploadValidationError.invalidRange
        }

        let rangeComponent: Substring
        if let spaceIndex = trimmed.firstIndex(of: " ") {
            rangeComponent = trimmed[trimmed.index(after: spaceIndex)...]
        } else {
            rangeComponent = Substring(trimmed)
        }

        let rangeWithoutTotal = rangeComponent.split(separator: "/").first ?? rangeComponent
        let bounds = rangeWithoutTotal.split(separator: "-")
        guard bounds.count == 2,
            let start = Int64(bounds[0]),
            let end = Int64(bounds[1]),
            start >= 0,
            end >= start
        else {
            throw UploadValidationError.invalidRange
        }

        return (start, end)
    }

    private func streamChunk(
        from body: OpenAPIRuntime.HTTPBody,
        session: inout UploadSessionStore.Session,
        contentClient: Containerd_Services_Content_V1_Content.Client<HTTP2ClientTransport.Posix>
    ) async throws {
        let logger = Logger(label: "sh.wendyengineer.containerd-registry.stream-chunk")
        for try await chunk in body {
            guard !chunk.isEmpty else { continue }
            let data = Data(chunk)
            logger.trace(
                "Writing chunk",
                metadata: [
                    "digest": .string(session.ref), "bytes": .stringConvertible(session.offset),
                    "chunk-size": .stringConvertible(data.count),
                ]
            )
            try await session.write(
                .with { request in
                    request.action = .write
                    request.ref = session.ref
                    request.offset = session.offset
                    request.data = data
                }
            )
            session.offset += Int64(data.count)
        }
    }

    private enum UploadValidationError: Error {
        case invalidRange
        case invalidDigest
        case inconsistentRange
    }
}
