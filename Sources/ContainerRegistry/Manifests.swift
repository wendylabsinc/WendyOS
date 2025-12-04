//===----------------------------------------------------------------------===//
//
// This source file is part of the SwiftContainerPlugin open source project
//
// Copyright (c) 2024 Apple Inc. and the SwiftContainerPlugin project authors
// Licensed under Apache License v2.0
//
// See LICENSE.txt for license information
// See CONTRIBUTORS.txt for the list of SwiftContainerPlugin project authors
//
// SPDX-License-Identifier: Apache-2.0
//
//===----------------------------------------------------------------------===//

extension RegistryClient {
    public func putManifest(
        repository: String,
        reference: String,
        manifest: ImageManifest
    ) async throws -> String {
        // See https://github.com/opencontainers/distribution-spec/blob/main/spec.md#pushing-manifests
        precondition(repository.count > 0, "repository must not be an empty string")
        precondition(reference.count > 0, "reference must not be an empty string")

        let httpResponse = try await executeRequestThrowing(
            // All blob uploads have Content-Type: application/octet-stream on the wire, even if mediatype is different
            .put(
                repository,
                path: "manifests/\(reference)",
                contentType: manifest.mediaType ?? "application/vnd.oci.image.manifest.v1+json"
            ),
            uploading: manifest,
            expectingStatus: .created,
            decodingErrors: [.notFound]
        )

        // The distribution spec says the response MUST contain a Location header
        // providing a URL from which the saved manifest can be downloaded.
        // However some registries return URLs which cannot be fetched, and
        // ECR does not set this header at all.
        // If the header is not present, create a suitable value.
        // https://github.com/opencontainers/distribution-spec/blob/main/spec.md#pulling-manifests
        return httpResponse.response.headerFields[.location]
            ?? registryURL.distributionEndpoint(
                forRepository: repository,
                andEndpoint: "manifests/\(manifest.digest)"
            )
            .absoluteString
    }

    /// Encodes and uploads an image index.
    ///
    /// - Parameters:
    ///   - repository: Name of the destination repository.
    ///   - reference: Optional tag to apply to this index.
    ///   - index: Index to be uploaded.
    /// - Returns: An ContentDescriptor object representing the
    ///            uploaded index.
    /// - Throws: If the index cannot be encoded or the upload fails.
    ///
    /// An index is a type of manifest.   Manifests are not treated as blobs
    /// by the distribution specification.   They have their own MIME types
    /// and are uploaded to different endpoint.
    public func putIndex(
        repository: String,
        reference: String?,
        index: ImageIndex
    ) async throws -> ContentDescriptor {
        // See https://github.com/opencontainers/distribution-spec/blob/main/spec.md#pushing-manifests

        let encoded = try encoder.encode(index)
        let digest = digest(of: encoded)
        let mediaType = index.mediaType ?? "application/vnd.oci.image.index.v1+json"

        let _ = try await executeRequestThrowing(
            .put(
                repository,
                path: "manifests/\(reference ?? digest)",
                contentType: mediaType
            ),
            uploading: encoded,
            expectingStatus: .created,
            decodingErrors: [.notFound]
        )

        return ContentDescriptor(
            mediaType: mediaType,
            digest: digest,
            size: Int64(encoded.count)
        )
    }

    public func getManifest(repository: String, reference: String) async throws -> ImageManifest {
        // See https://github.com/opencontainers/distribution-spec/blob/main/spec.md#pulling-manifests
        precondition(repository.count > 0, "repository must not be an empty string")
        precondition(reference.count > 0, "reference must not be an empty string")

        return try await executeRequestThrowing(
            .get(
                repository,
                path: "manifests/\(reference)",
                accepting: [
                    "application/vnd.oci.image.manifest.v1+json",
                    "application/vnd.docker.distribution.manifest.v2+json",
                ]
            ),
            decodingErrors: [.notFound]
        )
        .data
    }

    public func getIndex(repository: String, reference: String) async throws -> ImageIndex {
        precondition(repository.count > 0, "repository must not be an empty string")
        precondition(reference.count > 0, "reference must not be an empty string")

        return try await executeRequestThrowing(
            .get(
                repository,
                path: "manifests/\(reference)",
                accepting: [
                    "application/vnd.oci.image.index.v1+json",
                    "application/vnd.docker.distribution.manifest.list.v2+json",
                ]
            ),
            decodingErrors: [.notFound]
        )
        .data
    }
}
