// swift-tools-version: 6.2
import PackageDescription

// Generator dependencies are independent of the agent's runtime dependencies.
let package = Package(
    name: "ProtoGeneration",
    dependencies: [
        .package(url: "https://github.com/grpc/grpc-swift-2.git", exact: "2.3.0"),
        .package(url: "https://github.com/grpc/grpc-swift-protobuf.git", exact: "2.3.0"),
        .package(url: "https://github.com/apple/swift-protobuf.git", exact: "1.39.0"),
    ]
)
