import Crypto
import Foundation
import Logging
import NIOFoundationCompat
import Noora
import Subprocess
import _NIOFileSystem

public struct ContainerLayer: Sendable {
    public let path: URL
    public let hash: String
    public let diffID: String
    public let size: Int64
    public let gzip: Bool

    public var digest: String {
        "sha256:\(hash)"
    }
}

public struct Container: Sendable {
    public let layers: [ContainerLayer]
    public let config: DockerConfig
}

private let logger = Logger(label: "sh.wendy.container-builder")

public func buildDockerContainerLayers(
    image: ContainerImageSpec,
    imageName: String,
    outputDirectoryPath: URL
) async throws -> [ContainerLayer] {
    logger.debug(
        "Building container layers",
        metadata: ["image-name": .string(imageName), "output": .string(outputDirectoryPath.path())]
    )
    var layers = [ContainerLayer]()

    for (index, layer) in image.layers.enumerated() {
        var layerTarPath = outputDirectoryPath.appendingPathComponent("layer\(index).tar")
        let gzip: Bool
        let precalculatedSize: Int64?

        switch layer.content {
        case .files(let files):
            // TODO: Replace with custom .tar implementation for better throughput

            // Create a directory for this layer
            let layerDir = outputDirectoryPath.appendingPathComponent("layer\(index)")
            try FileManager.default.createDirectory(at: layerDir, withIntermediateDirectories: true)

            // Create each file in the layer
            for file in files {
                // Handle absolute paths in container file system by removing the leading slash
                var relativePath = file.destination
                if relativePath.hasPrefix("/") {
                    relativePath = String(relativePath.dropFirst())
                }

                let destinationURL = layerDir.appendingPathComponent(relativePath)

                // Ensure the parent directory exists with proper permissions
                let parentDir = destinationURL.deletingLastPathComponent()
                try FileManager.default.createDirectory(
                    at: parentDir,
                    withIntermediateDirectories: true,
                    attributes: [FileAttributeKey.posixPermissions: 0o755]
                )

                // Copy the file
                try FileManager.default.copyItem(at: file.source, to: destinationURL)

                // Set permissions if specified
                if let permissions = file.permissions {
                    try FileManager.default.setAttributes(
                        [.posixPermissions: permissions],
                        ofItemAtPath: destinationURL.path
                    )
                }
            }

            // Create a tarball from the directory
            try await createTarball(from: layerDir, to: layerTarPath)
            gzip = false
            precalculatedSize = nil

        case .tarball(let tarballURL, let uncompressedSize):
            // Use the tarball directly
            layerTarPath = tarballURL
            gzip = true  //tarballURL.path.hasSuffix(".gz")
            precalculatedSize = uncompressedSize
        }

        // If the layer has a predefined diffID, use it
        logger.debug(
            "Calculating diffID for layer",
            metadata: ["path": .string(layerTarPath.path())]
        )
        let layer = try await FileSystem.shared.withFileHandle(
            forReadingAt: FilePath(layerTarPath.path())
        ) { fileHandle in
            var sha = SHA256()
            var fileSize: Int64 = 0
            for try await chunk in fileHandle.readChunks() {
                sha.update(data: chunk.readableBytesView)
                fileSize += Int64(chunk.readableBytesView.count)
            }
            let layerSHA = sha.finalize()
                .map { String(format: "%02x", $0) }
                .joined()

            let diffID = layer.diffID ?? "sha256:\(layerSHA)"

            logger.trace(
                "Calculated diffID for layer",
                metadata: [
                    "path": .string(layerTarPath.path()), "diffID": .string(diffID),
                    "size": .string("\(fileSize) bytes"),
                ]
            )

            return ContainerLayer(
                path: layerTarPath,
                hash: layerSHA,
                diffID: diffID,
                size: precalculatedSize ?? fileSize,
                gzip: gzip
            )
        }
        layers.append(layer)
    }

    logger.trace("Built container layers")
    return layers
}

public func buildDockerContainer(
    image: ContainerImageSpec,
    imageName: String,
    tempDir: URL
) async throws -> Container {
    let layers = try await Noora().progressStep(
        message: "Building container layers",
        successMessage: "Container built",
        errorMessage: "Failed to build container",
        showSpinner: true
    ) { progress in
        try await buildDockerContainerLayers(
            image: image,
            imageName: imageName,
            outputDirectoryPath: tempDir
        )
    }

    // Create config.json
    let dateFormatter = ISO8601DateFormatter()
    let config = DockerConfig(
        architecture: image.architecture,
        created: dateFormatter.string(from: image.created),
        os: image.os,
        config: ContainerConfig(
            Cmd: image.cmd,
            Env: image.env,
            WorkingDir: image.workingDir
        ),
        rootfs: RootFS(
            type: "layers",
            diff_ids: layers.map(\.diffID)
        )
    )

    return Container(layers: layers, config: config)
}

/// Builds a Docker-compatible container image from the given image specification.
/// The image is saved to the given path.
///
/// This currently follows the format expected by `docker load`, which is not
/// the same as the OCI Image Format Specification.
public func buildDockerContainerImage(
    image: ContainerImageSpec,
    imageName: String,
    outputPath: String
) async throws {
    // TODO: Implement this using the OCI Image Format Specification instead of Docker's format?
    // TODO: Write directly to a tar file instead of using a temporary directory
    let tempDir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)

    let container = try await buildDockerContainer(
        image: image,
        imageName: imageName,
        tempDir: tempDir
    )

    // Serialize and save config
    Noora().info("Creating config.json")
    let configData = try JSONEncoder().encode(container.config)
    let configPath = tempDir.appendingPathComponent("config.json")
    try configData.write(to: configPath)
    let configSHA = sha256(data: configData)

    // Create image manifest
    logger.debug("Creating image manifest")
    let imageTag = "latest"
    let repositories = [
        imageName: [
            imageTag: configSHA
        ]
    ]
    let repositoriesData = try JSONEncoder().encode(repositories)
    let repositoriesPath = tempDir.appendingPathComponent("repositories")
    try repositoriesData.write(to: repositoriesPath)

    // Create final container image tarball
    Noora().info("Creating container image")
    let imageDir = tempDir.appendingPathComponent("image")
    try FileManager.default.createDirectory(at: imageDir, withIntermediateDirectories: true)

    // Copy layers and config to image directory
    for layer in container.layers {
        let destinationPath = imageDir.appendingPathComponent("\(layer.hash).tar")
        try FileManager.default.copyItem(at: layer.path, to: destinationPath)
    }

    let imageConfigPath = imageDir.appendingPathComponent("\(configSHA).json")
    try configData.write(to: imageConfigPath)

    // Copy repositories file to image directory
    let imageRepositoriesPath = imageDir.appendingPathComponent("repositories")
    try repositoriesData.write(to: imageRepositoriesPath)

    // manifest.json
    let manifest: [DockerManifestEntry] = [
        DockerManifestEntry(
            Config: "\(configSHA).json",
            RepoTags: ["\(imageName):\(imageTag)"],
            Layers: container.layers.map { "\($0.hash).tar" }
        )
    ]

    let manifestData = try JSONEncoder().encode(manifest)
    let manifestPath = imageDir.appendingPathComponent("manifest.json")
    try manifestData.write(to: manifestPath)

    try await createTarball(from: imageDir, to: URL(fileURLWithPath: outputPath))
    logger.debug("Created container image tarball", metadata: ["output": .string(outputPath)])
}

// Calculate SHA256 hash using Swift Crypto
private func sha256(data: Data) -> String {
    let digest = SHA256.hash(data: data)
    return digest.map { String(format: "%02x", $0) }.joined()
}

/// Creates a tarball from the given source directory using tar via env lookup.
///
/// - Parameter sourceDir: The directory to create a tarball from.
/// - Parameter destinationURL: The URL to save the tarball to.
/// - Throws: An error if the tarball cannot be created.
private func createTarball(from sourceDir: URL, to destinationURL: URL) async throws {
    _ = try await Subprocess.run(
        .name("tar"),
        arguments: Subprocess.Arguments([
            "-cf", destinationURL.path, "-C", sourceDir.path, ".",
        ]),
        output: .discarded
    )
}

public struct ContainerConfig: Codable, Sendable {
    public var Cmd: [String]
    public var Env: [String]
    public var WorkingDir: String
}

public struct RootFS: Codable, Sendable {
    public var type: String
    public var diff_ids: [String]
}

public struct DockerConfig: Codable, Sendable {
    public var architecture: String
    public var created: String
    public var os: String
    public var config: ContainerConfig
    public var rootfs: RootFS
}

public struct DockerManifestEntry: Codable, Sendable {
    public var Config: String
    public var RepoTags: [String]
    public var Layers: [String]
}
