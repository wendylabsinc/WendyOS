// swift-tools-version: 6.1
import PackageDescription

let package = Package(
    name: "HelloVLM",
    platforms: [
        .macOS(.v15)
    ],
    dependencies: [
        .package(url: "https://github.com/hummingbird-project/hummingbird.git", from: "2.0.0"),
        .package(url: "https://github.com/wendylabsinc/swift-json-schema.git", from: "0.1.0"),
        .package(url: "https://github.com/apple/swift-argument-parser.git", from: "1.7.1"),
        .package(url: "https://github.com/apple/swift-container-plugin", from: "1.3.0"),
        .package(url: "https://github.com/tayloraswift/swift-jpeg.git", from: "2.0.0"),
    ],
    targets: [
        .systemLibrary(
            name: "CLinuxVideo"
        ),
        .executableTarget(
            name: "HelloVLM",
            dependencies: [
                .product(name: "Hummingbird", package: "hummingbird"),
                .product(name: "ArgumentParser", package: "swift-argument-parser"),
                .product(name: "JPEG", package: "swift-jpeg"),
                .target(name: "CLinuxVideo", condition: .when(platforms: [.linux])),
            ],
            resources: [
                .copy("Resources")
            ],
            plugins: [
                .plugin(name: "JSONSchemaPlugin", package: "swift-json-schema")
            ]
        ),
        .testTarget(
            name: "HelloVLMTests",
            dependencies: [
                "HelloVLM",
                .product(name: "JPEG", package: "swift-jpeg"),
            ]
        ),
    ]
)
