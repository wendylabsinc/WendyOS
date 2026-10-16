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

    // https://github.com/swiftlang/swift-evolution/blob/main/proposals/0458-strict-memory-safety.md
    // Opt into strict memory safety: uses of memory-unsafe APIs must be spelled
    // `unsafe`, so any new unchecked pointer/buffer usage is surfaced at review.
    .strictMemorySafety(),
]

let package = Package(
    name: "WendyAgentCore",
    platforms: [
        .macOS(.v15)
    ],
    products: [
        .library(name: "WendyAgentCore", targets: ["WendyAgentCore"])
    ],
    dependencies: [
        .package(url: "https://github.com/grpc/grpc-swift-2.git", from: "2.2.1"),
        .package(url: "https://github.com/grpc/grpc-swift-nio-transport.git", from: "2.3.0"),
        .package(url: "https://github.com/grpc/grpc-swift-protobuf.git", from: "2.0.0"),
        .package(url: "https://github.com/grpc/grpc-swift-extras.git", from: "2.1.1"),
        .package(url: "https://github.com/apple/swift-log.git", from: "1.6.3"),
        .package(url: "https://github.com/apple/swift-protobuf.git", from: "1.37.0"),
        .package(url: "https://github.com/swift-server/swift-service-lifecycle.git", from: "2.9.1"),
        .package(url: "https://github.com/apple/swift-crypto.git", from: "4.0.0"),
        // ML-DSA-65/87 certificate parsing lives on apple/swift-certificates PR #312, not yet
        // released. We pin its head commit, which GitHub keeps fetchable from apple's own repo
        // only while that PR stays open and is not force-pushed (wendylabsinc/swift-certificates
        // keeps it at refs/heads/pin-65ecdea6). Do not rebase/force-push that PR without bumping
        // this revision in the same change. Swap to `from: "<version>"` once Apple ships a
        // release carrying it. Same revision as Wendy Cloud and wendy-auth: a cert this parses
        // has to parse there too. On Darwin it verifies ML-DSA only on macOS 26+ (CryptoKit).
        .package(
            url: "https://github.com/apple/swift-certificates.git",
            revision: "65ecdea656b6764c77f49d213aa417f1d2838b52"
        ),
        // swift-nio-ssl comes from the wendylabsinc fork, branch mldsa-on-upstream: apple main
        // 15e0b9e (which carries apple/swift-nio-ssl#604, the fix this package pinned for)
        // revendoring BoringSSL 8eb25be6f with ML-DSA in libssl, plus NIOSSL ML-DSA signature
        // algorithms in the DEFAULT verify list, so an ML-DSA operator's CertificateVerify
        // verifies at the mTLS handshake with no per-config opt-in. Bump it in the same change
        // as any fork update and keep it in step with Wendy Cloud's and wendy-auth's pin. Swap
        // back to apple upstream `from:` once Apple ships a release with ML-DSA in libssl. This
        // root-package revision pin overrides grpc-swift-nio-transport's version requirement on
        // the same package identity.
        .package(
            url: "https://github.com/wendylabsinc/swift-nio-ssl.git",
            revision: "dce9736251c9110b6c249f5ea7fd34199d5dd9bf"
        ),
        .package(url: "https://github.com/hummingbird-project/hummingbird.git", from: "2.0.0"),
    ],
    targets: [
        .target(
            name: "CUVCControl",
            linkerSettings: [.linkedFramework("IOKit"), .linkedFramework("CoreFoundation")]
        ),
        .testTarget(
            name: "WendyAgentCoreTests",
            dependencies: [
                .target(name: "WendyAgentCore"),
                .target(name: "WendyAgentGRPC"),
                .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "Logging", package: "swift-log"),
                .product(name: "NIOSSL", package: "swift-nio-ssl"),
                .product(name: "SwiftProtobuf", package: "swift-protobuf"),
                .product(name: "Hummingbird", package: "hummingbird"),
                .product(name: "HummingbirdTesting", package: "hummingbird"),
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
                .product(name: "NIOSSL", package: "swift-nio-ssl"),
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "SwiftProtobuf", package: "swift-protobuf"),
                .product(name: "GRPCServiceLifecycle", package: "grpc-swift-extras"),
                .product(name: "Crypto", package: "swift-crypto"),
                .product(name: "X509", package: "swift-certificates"),
                .product(name: "Hummingbird", package: "hummingbird"),
                .product(name: "HummingbirdTLS", package: "hummingbird"),
                .target(name: "WendyAgentGRPC"),
                .target(name: "WendyCloudGRPC"),
                .target(name: "CUVCControl"),
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
