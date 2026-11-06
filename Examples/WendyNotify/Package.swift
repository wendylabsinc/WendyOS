// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "WendyNotify",
    platforms: [.macOS(.v15)],
    dependencies: [
        .package(url: "https://github.com/hummingbird-project/hummingbird.git", from: "2.10.0"),
        .package(url: "https://github.com/grpc/grpc-swift.git", from: "2.0.0"),
        .package(url: "https://github.com/grpc/grpc-swift-protobuf.git", from: "1.0.0"),
        .package(url: "https://github.com/grpc/grpc-swift-nio-transport.git", from: "1.0.0"),
        .package(url: "https://github.com/apple/swift-protobuf.git", from: "1.28.0"),
        .package(url: "https://github.com/apple/swift-container-plugin", from: "1.3.0"),
    ],
    targets: [
        .executableTarget(name: "WendyNotify", dependencies: [
            .product(name: "Hummingbird", package: "hummingbird"),
            .product(name: "GRPCCore", package: "grpc-swift"),
            .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
            .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
            .product(name: "SwiftProtobuf", package: "swift-protobuf"),
        ]),
        .testTarget(name: "WendyNotifyTests", dependencies: [
            "WendyNotify", .product(name: "HummingbirdTesting", package: "hummingbird"),
        ]),
    ]
)
