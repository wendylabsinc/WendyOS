// swift-tools-version: 6.2

import PackageDescription

let package = Package(
    name: "HelloMac",
    platforms: [
        .macOS(.v26)
    ],
    dependencies: [],
    targets: [
        .executableTarget(
            name: "HelloMac",
            resources: [
                .process("Resources")
            ]
        )
    ]
)
