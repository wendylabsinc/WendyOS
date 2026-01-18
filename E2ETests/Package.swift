// swift-tools-version: 6.2.0
import PackageDescription

let package = Package(
    name: "WendyE2ETests",
    platforms: [
        .macOS(.v15)
    ],
    products: [
        .library(name: "E2ETestHarness", targets: ["E2ETestHarness"])
    ],
    dependencies: [
        // Reference parent package
        .package(path: ".."),
        // For subprocess execution
        .package(
            url: "https://github.com/swiftlang/swift-subprocess.git",
            exact: "0.2.1",
            traits: [.trait(name: "SubprocessSpan")]
        ),
        // For logging
        .package(url: "https://github.com/apple/swift-log.git", from: "1.6.3"),
        // For DNS resolution in discovery helper
        .package(url: "https://github.com/apple/swift-async-dns-resolver.git", from: "0.4.0"),
        // For gRPC transport
        .package(url: "https://github.com/grpc/grpc-swift-nio-transport.git", from: "2.3.0"),
        .package(url: "https://github.com/grpc/grpc-swift-2.git", from: "2.1.0"),
    ],
    targets: [
        .target(
            name: "E2ETestHarness",
            dependencies: [
                .product(name: "WendyAgentGRPC", package: "wendy-agent"),
                .product(name: "WendyShared", package: "wendy-agent"),
                .product(name: "Subprocess", package: "swift-subprocess"),
                .product(name: "Logging", package: "swift-log"),
                .product(name: "AsyncDNSResolver", package: "swift-async-dns-resolver"),
                .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
                .product(name: "GRPCCore", package: "grpc-swift-2"),
            ]
        ),
        .testTarget(
            name: "WendyE2ETests",
            dependencies: [
                .target(name: "E2ETestHarness"),
                .product(name: "WendyAgentGRPC", package: "wendy-agent"),
            ]
        ),
    ]
)
