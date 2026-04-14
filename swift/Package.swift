// swift-tools-version: 6.2.0
import PackageDescription

let package = Package(
    name: "WendyAgent",
    platforms: [
        .macOS(.v15)
    ],
    products: [
        .library(name: "WendyAgent", targets: ["WendyAgent"]),
    ],
    dependencies: [
        .package(url: "https://github.com/grpc/grpc-swift-2.git", from: "2.2.1"),
        .package(url: "https://github.com/grpc/grpc-swift-nio-transport.git", from: "2.3.0"),
        .package(url: "https://github.com/grpc/grpc-swift-protobuf.git", from: "2.0.0"),
        .package(url: "https://github.com/grpc/grpc-swift-extras.git", from: "2.1.1"),
        .package(url: "https://github.com/apple/swift-log.git", from: "1.6.3"),
        .package(url: "https://github.com/swift-server/swift-service-lifecycle.git", from: "2.9.1"),
    ],
    targets: [
        .testTarget(
            name: "WendyAgentTests",
            dependencies: [
                .target(name: "WendyAgent"),
                .target(name: "WendyAgentGRPC"),
                .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
                .product(name: "GRPCCore", package: "grpc-swift-2"),
            ],
            path: "Tests/WendyAgentTests"
        ),
        .target(
            name: "WendyAgent",
            dependencies: [
                .product(name: "Logging", package: "swift-log"),
                .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
                .product(name: "ServiceLifecycle", package: "swift-service-lifecycle"),
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCServiceLifecycle", package: "grpc-swift-extras"),
                .target(name: "WendyAgentGRPC"),
                .target(name: "WendyCloudGRPC"),
            ],
            path: "Sources/WendyAgent"
        ),
        .target(
            name: "WendyAgentGRPC",
            dependencies: [
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
                .target(name: "OpenTelemetryGRPC"),
            ]
        ),
        .target(
            name: "WendyCloudGRPC",
            dependencies: [
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
            ]
        ),
        .target(
            name: "OpenTelemetryGRPC",
            dependencies: [
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
            ]
        ),
    ]
)
