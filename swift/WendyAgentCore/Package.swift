// swift-tools-version: 6.2.0
import PackageDescription

let swiftSettings: [SwiftSetting] = [
    // https://github.com/apple/swift-evolution/blob/main/proposals/0335-existential-any.md
    .enableUpcomingFeature("ExistentialAny"),

    // https://github.com/swiftlang/swift-evolution/blob/main/proposals/0444-member-import-visibility.md
    .enableUpcomingFeature("MemberImportVisibility"),

    // https://github.com/swiftlang/swift-evolution/blob/main/proposals/0409-access-level-on-imports.md
    .enableUpcomingFeature("InternalImportsByDefault"),

    .enableUpcomingFeature("InferIsolatedConformances"),
    .enableUpcomingFeature("NonisolatedNonsendingByDefault"),
]

let package = Package(
    name: "WendyAgentCore",
    platforms: [
        .macOS(.v15)
    ],
    products: [
        .library(name: "WendyAgentCore", targets: ["WendyAgentCore"]),
    ],
    dependencies: [
        .package(url: "https://github.com/grpc/grpc-swift-2.git", from: "2.2.1"),
        .package(url: "https://github.com/grpc/grpc-swift-nio-transport.git", from: "2.3.0"),
        .package(url: "https://github.com/grpc/grpc-swift-protobuf.git", from: "2.0.0"),
        .package(url: "https://github.com/grpc/grpc-swift-extras.git", from: "2.1.1"),
        .package(url: "https://github.com/apple/swift-log.git", from: "1.6.3"),
        .package(url: "https://github.com/apple/swift-protobuf.git", from: "1.37.0"),
        .package(url: "https://github.com/swift-server/swift-service-lifecycle.git", from: "2.9.1"),
    ],
    targets: [
        .testTarget(
            name: "WendyAgentCoreTests",
            dependencies: [
                .target(name: "WendyAgentCore"),
                .target(name: "WendyAgentGRPC"),
                .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "Logging", package: "swift-log"),
                .product(name: "SwiftProtobuf", package: "swift-protobuf"),
            ],
            path: "Tests/WendyAgentTests",
            swiftSettings: swiftSettings
        ),
        .target(
            name: "WendyAgentCore",
            dependencies: [
                .product(name: "Logging", package: "swift-log"),
                .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
                .product(name: "ServiceLifecycle", package: "swift-service-lifecycle"),
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "SwiftProtobuf", package: "swift-protobuf"),
                .product(name: "GRPCServiceLifecycle", package: "grpc-swift-extras"),
                .target(name: "WendyAgentGRPC"),
                .target(name: "WendyCloudGRPC"),
            ],
            path: "Sources/WendyAgent",
            swiftSettings: swiftSettings
        ),
        .target(
            name: "WendyAgentGRPC",
            dependencies: [
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
                .product(name: "SwiftProtobuf", package: "swift-protobuf"),
                .target(name: "OpenTelemetryGRPC"),
            ],
            swiftSettings: swiftSettings
        ),
        .target(
            name: "WendyCloudGRPC",
            dependencies: [
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
                .product(name: "SwiftProtobuf", package: "swift-protobuf"),
            ],
            swiftSettings: swiftSettings
        ),
        .target(
            name: "OpenTelemetryGRPC",
            dependencies: [
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
                .product(name: "SwiftProtobuf", package: "swift-protobuf"),
            ],
            swiftSettings: swiftSettings
        ),
    ],
    // WendyAgentCore intentionally requires Swift 6 language mode so
    // strict concurrency diagnostics match the mac prototype defaults.
    swiftLanguageModes: [.v6]
)
