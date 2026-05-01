// swift-tools-version: 6.2

import PackageDescription

let package = Package(
    name: "HelloMac",
    platforms: [
        .macOS(.v26)
    ],
    dependencies: [
        .package(url: "https://github.com/apple/swift-container-plugin", from: "1.3.0"),
    ],
    targets: [
        .executableTarget(
            name: "HelloMac"
        )
    ]
)
