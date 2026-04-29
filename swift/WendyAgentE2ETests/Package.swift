// swift-tools-version: 6.2.0
import PackageDescription

let package = Package(
    name: "WendyAgentE2ETests",
    platforms: [
        .macOS(.v15)
    ],
    products: [
        .library(name: "WendyAgentE2E", targets: ["WendyAgentE2E"]),
    ],
    targets: [
        .target(
            name: "WendyAgentE2E",
            path: "Sources/WendyAgentE2E"
        ),
        .testTarget(
            name: "WendyAgentE2ETests",
            dependencies: ["WendyAgentE2E"],
            path: "Tests/WendyAgentE2ETests"
        ),
    ]
)
