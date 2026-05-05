// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "CISwiftResources",
    platforms: [
        .macOS(.v13)
    ],
    dependencies: [
        .package(url: "https://github.com/apple/swift-container-plugin", from: "1.3.0"),
    ],
    targets: [
        .executableTarget(
            name: "CISwiftResources",
            resources: [
                .process("Resources")
            ]
        )
    ]
)
