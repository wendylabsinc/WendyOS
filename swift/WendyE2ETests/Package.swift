// swift-tools-version: 6.1
import PackageDescription

let swiftSettings: [SwiftSetting] = [
    .enableUpcomingFeature("ExistentialAny"),
    .enableUpcomingFeature("MemberImportVisibility"),
    .enableUpcomingFeature("InternalImportsByDefault"),
]

let package = Package(
    name: "WendyE2ETesting",
    platforms: [
        .macOS(.v15)
    ],
    products: [
        .library(name: "WendyE2ETesting", targets: ["WendyE2ETesting"]),
        .executable(name: "swift-e2e-testing", targets: ["SwiftE2ETestingCLI"]),
    ],
    dependencies: [
        .package(url: "https://github.com/swiftlang/swift-subprocess.git", from: "0.4.0"),
        .package(url: "https://github.com/apple/swift-argument-parser.git", from: "1.6.0"),
        .package(url: "https://github.com/apple/swift-system", from: "1.6.0"),
    ],
    targets: [
        .target(
            name: "WendyE2ETesting",
            dependencies: [
                .product(name: "Subprocess", package: "swift-subprocess"),
                .product(name: "SystemPackage", package: "swift-system"),
            ],
            path: "Sources/WendyE2ETesting",
            swiftSettings: swiftSettings
        ),
        .executableTarget(
            name: "SwiftE2ETestingCLI",
            dependencies: [
                "WendyE2ETesting",
                .product(name: "ArgumentParser", package: "swift-argument-parser"),
            ],
            path: "Sources/SwiftE2ETestingCLI",
            swiftSettings: swiftSettings
        ),
        .testTarget(
            name: "WendyE2ETestingTests",
            dependencies: ["WendyE2ETesting"],
            path: "Tests/WendyE2ETestingTests",
            swiftSettings: swiftSettings
        ),
        .testTarget(
            name: "WendyE2ETests",
            dependencies: ["WendyE2ETesting"],
            path: "Tests/WendyE2ETests",
            swiftSettings: swiftSettings
        ),
    ]
)
