// swift-tools-version: 6.2.0
import PackageDescription

#if os(Windows)
    let packageDependencies: [Package.Dependency] = [
        .package(path: "../async-http-client"),
        .package(path: "../hummingbird"),
        .package(path: "../DNSClient"),
        .package(path: "../grpc-swift-nio-transport"),
        .package(path: "../swift-nio"),
        .package(path: "../swift-nio-ssl"),
        .package(path: "../swift-nio-extras"),
        .package(path: "../Rainbow"),
    ]
#else
    let packageDependencies: [Package.Dependency] = [
        .package(url: "https://github.com/swift-server/async-http-client.git", from: "1.25.2"),
        .package(url: "https://github.com/hummingbird-project/hummingbird.git", from: "2.0.2"),
        .package(url: "https://github.com/orlandos-nl/DNSClient.git", from: "2.6.1"),
        .package(
            url: "https://github.com/grpc/grpc-swift-nio-transport.git",
            from: "2.3.0"
        ),
        .package(url: "https://github.com/apple/swift-nio.git", from: "2.92.0"),
    ]
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
    dependencies: packageDependencies + [
        .package(url: "https://github.com/apple/swift-argument-parser.git", from: "1.5.0"),
        .package(url: "https://github.com/apple/swift-log.git", from: "1.6.3"),
        .package(url: "https://github.com/grpc/grpc-swift-2.git", from: "2.2.1"),
        .package(url: "https://github.com/grpc/grpc-swift-extras.git", from: "2.1.1"),
        .package(url: "https://github.com/grpc/grpc-swift-protobuf.git", from: "2.0.0"),
        .package(url: "https://github.com/apple/swift-certificates.git", from: "1.12.0"),
        .package(url: "https://github.com/swift-server/swift-service-lifecycle.git", from: "2.9.1"),
        .package(url: "https://github.com/apple/swift-crypto.git", from: "3.12.2"),
        .package(
            url: "https://github.com/wendylabsinc/Noora.git",
            branch: "main-wendy"
        ),
        .package(
            url: "https://github.com/swiftlang/swift-subprocess.git",
            exact: "0.2.1",
            traits: []
        ),
        .package(url: "https://github.com/apple/swift-http-types.git", from: "1.4.0"),
        .package(url: "https://github.com/apple/swift-async-dns-resolver.git", from: "0.4.0"),
        .package(url: "https://github.com/apple/swift-system.git", from: "1.4.2"),
        .package(url: "https://github.com/jpsim/Yams.git", from: "6.2.0"),
        .package(url: "https://github.com/wendylabsinc/bluetooth.git", from: "0.1.1"),
        .package(url: "https://github.com/wendylabsinc/dbus.git", from: "0.3.0"),
        .package(url: "https://github.com/wendylabsinc/TOMLKit.git", from: "0.7.0"),
        .package(url: "https://github.com/apple/swift-distributed-tracing.git", from: "1.0.0"),
    ],
    targets: [
        /// The main executable provided by wendy-cli.
        .executableTarget(
            name: "wendy",
            dependencies: [
                .product(name: "ArgumentParser", package: "swift-argument-parser"),
                .product(name: "Logging", package: "swift-log"),
                .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
                .product(name: "SystemPackage", package: "swift-system"),
                .product(name: "NIOFoundationCompat", package: "swift-nio"),
                .product(
                    name: "Hummingbird",
                    package: "hummingbird"
                ),
                .target(name: "CLIOutput"),
                .product(name: "DNSClient", package: "DNSClient"),
                .product(name: "Bluetooth", package: "bluetooth"),
                .target(name: "WendyAgentGRPC"),
                .target(name: "WendyCloudGRPC"),
                .target(name: "WendyShared"),
                .target(name: "Imager"),
                .target(name: "ContainerRegistry"),
                .target(name: "ContainerdGRPC"),
                .target(name: "DownloadSupport"),
                .target(name: "AppConfig"),
                .target(name: "CliXPCProtocol"),
                .target(name: "WendySDK"),
                .target(name: "Analytics"),
                .product(name: "TOMLKit", package: "TOMLKit"),
            ],
            path: "Sources/Wendy",
            resources: [
                .copy("Resources")
            ],
            linkerSettings: [
                .linkedLibrary("zlib", .when(platforms: [.windows])),
                .linkedLibrary("z", .when(platforms: [.windows])),
                .unsafeFlags(
                    ["-LC:/vcpkg/installed/x64-windows/lib"],
                    .when(platforms: [.windows])
                ),
            ]
        ),

        .target(
            name: "WendySDK",
            dependencies: [
                .product(name: "X509", package: "swift-certificates"),
                .product(name: "Crypto", package: "swift-crypto"),
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
                .product(name: "GRPCServiceLifecycle", package: "grpc-swift-extras"),
                .product(name: "DBUS", package: "dbus"),
                .product(name: "Subprocess", package: "swift-subprocess"),
                .product(name: "AsyncHTTPClient", package: "async-http-client"),
                .product(name: "Yams", package: "Yams"),
                .product(name: "Hummingbird", package: "hummingbird"),
                .product(name: "Tracing", package: "swift-distributed-tracing"),
                .target(name: "WendyCloudGRPC"),
                .target(name: "WendyAgentGRPC"),
                .target(name: "ContainerdGRPC"),
                .target(name: "WendyShared"),
                .target(name: "AppConfig"),
                .target(name: "ContainerRegistry"),
                .target(name: "WendySDK"),
                .target(name: "OpenTelemetryGRPC"),
                .target(name: "ALSA"),
                .product(name: "Bluetooth", package: "bluetooth"),
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
                .product(
                    name: "AsyncDNSResolver",
                    package: "swift-async-dns-resolver",
                    condition: .when(platforms: [.macOS])
                ),
                .product(name: "Subprocess", package: "swift-subprocess"),
                .product(name: "DNSClient", package: "DNSClient"),
                .product(name: "Bluetooth", package: "bluetooth"),
                .product(name: "NIOCore", package: "swift-nio"),
                .product(name: "NIOFoundationCompat", package: "swift-nio"),
                .target(name: "WendyAgentGRPC"),
            ]
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
        .target(
            name: "ALSA",
            dependencies: [
                .product(name: "Subprocess", package: "swift-subprocess"),
                .product(name: "_NIOFileSystem", package: "swift-nio"),
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
                .target(name: "DownloadSupport"),
                .target(name: "CLIOutput"),
            ]
        ),
        .target(
            name: "DownloadSupport",
            dependencies: [
                .product(name: "AsyncHTTPClient", package: "async-http-client"),
                .product(name: "NIOFoundationCompat", package: "swift-nio"),
                .product(
                    name: "_NIOFileSystem",
                    package: "swift-nio",
                    condition: .when(platforms: [.macOS, .linux])
                ),
            ]
        ),
        .target(
            name: "AppConfig",
            dependencies: [
                .product(name: "ArgumentParser", package: "swift-argument-parser")
            ]
        ),

        /// CLI output abstraction layer (owns the Noora TUI dependency)
        .target(
            name: "CLIOutput",
            dependencies: [
                .product(name: "Noora", package: "Noora"),
                .product(name: "NIOCore", package: "swift-nio"),
                .product(name: "NIOFoundationCompat", package: "swift-nio"),
            ]
        ),

        /// Analytics module for privacy-first usage tracking
        .target(
            name: "Analytics",
            dependencies: [
                .product(name: "AsyncHTTPClient", package: "async-http-client"),
                .product(name: "Logging", package: "swift-log"),
                .target(name: "WendyShared"),
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

        .testTarget(
            name: "AnalyticsTests",
            dependencies: [
                .target(name: "Analytics")
            ]
        ),

    ]
)
