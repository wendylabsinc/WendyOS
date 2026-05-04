// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "CISwiftResources",
    platforms: [
        .macOS(.v13)
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
