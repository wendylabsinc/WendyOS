// swift-tools-version: 6.1.2
import PackageDescription

#if compiler(>=6.2.1)
    let hasSpan = true
#else
    let hasSpan = false
#endif

let package = Package(
    name: "wendy-agent",
    platforms: [
        .macOS(.v15)
    ],
    products: [
        .executable(name: "wendy-agent", targets: ["wendy-agent"]),
        .executable(name: "wendy", targets: ["wendy"]),
        .executable(name: "wendy-helper", targets: ["wendy-helper"]),
        .executable(name: "wendy-network-daemon", targets: ["wendy-network-daemon"]),
    ],
    dependencies: [
        .package(url: "https://github.com/swift-server/async-http-client.git", from: "1.25.2"),
        .package(url: "https://github.com/hummingbird-project/hummingbird.git", from: "2.0.2"),
        .package(url: "https://github.com/vapor/jwt-kit.git", from: "5.0.0"),
        .package(url: "https://github.com/apple/swift-argument-parser.git", from: "1.5.0"),
        .package(url: "https://github.com/apple/swift-log.git", from: "1.6.3"),
        .package(url: "https://github.com/grpc/grpc-swift-2.git", from: "2.1.0"),
        .package(url: "https://github.com/grpc/grpc-swift-protobuf.git", from: "2.0.0"),
        .package(url: "https://github.com/orlandos-nl/DNSClient.git", from: "2.5.0"),
        // .package(url: "https://github.com/grpc/grpc-swift-nio-transport.git", from: "2.0.0"),
        .package(
            url: "https://github.com/Joannis/grpc-swift-nio-transport.git",
            branch: "jo/custom-verification-of-server"
        ),
        .package(url: "https://github.com/apple/swift-certificates.git", from: "1.12.0"),
        .package(url: "https://github.com/swift-server/swift-service-lifecycle.git", from: "2.7.0"),
        .package(url: "https://github.com/apple/swift-nio.git", from: "2.81.0"),
        .package(url: "https://github.com/apple/swift-crypto.git", from: "3.12.2"),
        //        .package(url: "https://github.com/tuist/Noora.git", from: "0.32.0"),
        .package(
            url: "https://github.com/tuist/Noora.git",
            revision: "259b4ad29366ba327148aaaea0e59b1c78953831"
        ),
        .package(
            url: "https://github.com/swiftlang/swift-subprocess.git",
            exact: "0.2.1",
            traits: hasSpan ? [.trait(name: "SubprocessSpan")] : []
        ),
        .package(url: "https://github.com/apple/swift-http-types.git", from: "1.4.0"),
        .package(url: "https://github.com/apple/swift-async-dns-resolver.git", from: "0.4.0"),
        .package(url: "https://github.com/apple/swift-openapi-generator.git", from: "1.10.3"),
        .package(
            url: "https://github.com/swift-server/swift-openapi-async-http-client.git",
            from: "1.1.0"
        ),
        .package(url: "https://github.com/edgeengineer/dbus.git", from: "0.2.3"),
        .package(url: "https://github.com/apple/swift-system.git", from: "1.4.2"),
    ],
    targets: [
        /// The main executable provided by wendy-cli.
        .executableTarget(
            name: "wendy",
            dependencies: [
                .product(name: "ArgumentParser", package: "swift-argument-parser"),
                .product(name: "Logging", package: "swift-log"),
                .product(name: "_NIOFileSystem", package: "swift-nio"),
                .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
                .product(name: "AsyncDNSResolver", package: "swift-async-dns-resolver"),
                .product(name: "SystemPackage", package: "swift-system"),
                .product(name: "NIOFoundationCompat", package: "swift-nio"),
                .product(
                    name: "OpenAPIAsyncHTTPClient",
                    package: "swift-openapi-async-http-client"
                ),
                .product(
                    name: "Hummingbird",
                    package: "hummingbird"
                ),
                .product(
                    name: "JWTKit",
                    package: "jwt-kit"
                ),
                .product(name: "Noora", package: "Noora"),
                .product(name: "DNSClient", package: "DNSClient"),
                .target(name: "WendyAgentGRPC"),
                .target(name: "WendyCloudGRPC"),
                .target(name: "WendyCLI"),
                .target(name: "WendyShared"),
                .target(name: "Imager"),
                .target(name: "ContainerRegistry"),
                .target(name: "ContainerdGRPC"),
                .target(name: "DownloadSupport"),
                .target(name: "AppConfig"),
                .target(name: "CliXPCProtocol"),
                .target(name: "WendySDK"),
                .target(name: "DockerOpenAPI"),
            ],
            path: "Sources/Wendy",
            resources: [
                .copy("Resources")
            ]
        ),

        .target(
            name: "WendySDK",
            dependencies: [
                .product(name: "X509", package: "swift-certificates"),
                .product(name: "Crypto", package: "swift-crypto"),
            ]
        ),

        .target(
            name: "DockerOpenAPI",
            dependencies: [
                .product(name: "OpenAPIAsyncHTTPClient", package: "swift-openapi-async-http-client")
            ],
            plugins: [.plugin(name: "OpenAPIGenerator", package: "swift-openapi-generator")]
        ),

        /// Contains everything WendyCLI, except for the command line interface.
        .target(
            name: "WendyCLI",
            dependencies: [
                .target(name: "ContainerBuilder"),
                .product(name: "Subprocess", package: "swift-subprocess"),
                .product(name: "Logging", package: "swift-log"),
            ]
        ),

        /// Tools to build OCI-compliant container images.
        .target(
            name: "ContainerBuilder",
            dependencies: [
                .product(name: "Subprocess", package: "swift-subprocess"),
                .product(name: "_NIOFileSystem", package: "swift-nio"),
                .product(name: "Crypto", package: "swift-crypto"),
                .product(name: "NIOFoundationCompat", package: "swift-nio"),
                .target(name: "ContainerRegistry"),
            ]
        ),

        /// The main executable provided by wendy-agent.
        .executableTarget(
            name: "wendy-agent",
            dependencies: [
                .product(name: "ArgumentParser", package: "swift-argument-parser"),
                .product(name: "Logging", package: "swift-log"),
                .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
                .product(name: "ServiceLifecycle", package: "swift-service-lifecycle"),
                .product(name: "_NIOFileSystem", package: "swift-nio"),
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "DBUS", package: "dbus"),
                .product(name: "Subprocess", package: "swift-subprocess"),
                .target(name: "WendyCloudGRPC"),
                .target(name: "WendyAgentGRPC"),
                .target(name: "ContainerdGRPC"),
                .target(name: "WendyShared"),
                .target(name: "AppConfig"),
                .target(name: "ContainerRegistry"),
                .target(name: "WendySDK"),
                .target(name: "OpenTelemetryGRPC"),
            ],
            path: "Sources/WendyAgent"
        ),

        /// Shared components used by both wendy and wendy-agent.
        .target(
            name: "ContainerRegistry",
            dependencies: [
                .product(name: "Crypto", package: "swift-crypto"),
                .product(name: "HTTPTypes", package: "swift-http-types"),
                .product(name: "HTTPTypesFoundation", package: "swift-http-types"),
                .product(name: "AsyncHTTPClient", package: "async-http-client"),
            ]
        ),
        .target(
            name: "WendyShared",
            dependencies: [
                .product(name: "Logging", package: "swift-log"),
                .product(name: "AsyncDNSResolver", package: "swift-async-dns-resolver"),
                .product(name: "Subprocess", package: "swift-subprocess"),
                .product(name: "DNSClient", package: "DNSClient"),
            ]
        ),
        .target(
            name: "WendyAgentGRPC",
            dependencies: [
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
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
        .target(
            name: "ContainerdGRPC",
            dependencies: [
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
                .target(name: "ContainerdGRPCTypes"),
            ]
        ),
        .target(
            name: "ContainerdGRPCTypes",
            dependencies: [
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
            ],
        ),
        .target(
            name: "Imager",
            dependencies: [
                .product(name: "Subprocess", package: "swift-subprocess"),
                .product(name: "AsyncHTTPClient", package: "async-http-client"),
                .product(name: "_NIOFileSystem", package: "swift-nio"),
                .target(name: "DownloadSupport"),
            ]
        ),
        .target(
            name: "DownloadSupport",
            dependencies: [
                .product(name: "AsyncHTTPClient", package: "async-http-client"),
                .product(name: "_NIOFileSystem", package: "swift-nio"),
                .product(name: "NIOFoundationCompat", package: "swift-nio"),
            ]
        ),
        .target(
            name: "AppConfig",
            dependencies: [
                .product(name: "ArgumentParser", package: "swift-argument-parser")
            ]
        ),

        /// Tests for WendyCLI components
        .testTarget(
            name: "WendyCLITests",
            dependencies: [
                .target(name: "wendy"),
                .target(name: "wendy-agent"),
                .target(name: "WendyAgentGRPC"),
                .target(name: "WendySDK"),
                .target(name: "wendy-helper", condition: .when(platforms: [.macOS])),
            ]
        ),

        .testTarget(
            name: "ImagerTests",
            dependencies: [
                .target(name: "Imager")
            ]
        ),

        /// The wendy helper daemon for USB device monitoring
        .executableTarget(
            name: "wendy-helper",
            dependencies: [
                .product(name: "ArgumentParser", package: "swift-argument-parser"),
                .product(name: "Logging", package: "swift-log"),
                .product(name: "SystemPackage", package: "swift-system"),
                .target(name: "WendyShared"),
                // Reuse existing device discovery components
                .target(name: "wendy"),  // For device discovery protocols
            ],
            path: "Sources/WendyHelper"
        ),

        /// XPC Protocol for communication between CLI and privileged daemon
        .target(
            name: "CliXPCProtocol",
            dependencies: []
        ),

        /// The privileged network daemon for macOS
        .executableTarget(
            name: "wendy-network-daemon",
            dependencies: [
                .product(name: "ArgumentParser", package: "swift-argument-parser"),
                .product(name: "Logging", package: "swift-log"),
                .target(name: "WendyShared"),
                .target(name: "CliXPCProtocol"),
            ],
            path: "Sources/WendyNetworkDaemon",
            exclude: [
                "wendy-network-daemon.entitlements"
            ]
        ),

        .testTarget(
            name: "WendyHelperMacOSTests",
            dependencies: [
                .target(name: "wendy-helper"),
                .target(name: "WendyShared"),
            ]
        ),

        .testTarget(
            name: "WendyAgentTests",
            dependencies: [
                .target(name: "wendy-agent"),
                .target(name: "WendyAgentGRPC"),
            ]
        ),

        .testTarget(
            name: "IntegrationTests",
            dependencies: [
                .target(name: "wendy"),
                .target(name: "wendy-agent"),
            ]
        ),

    ]
)
