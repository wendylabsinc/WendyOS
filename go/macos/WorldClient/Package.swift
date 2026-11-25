// swift-tools-version: 6.3
import PackageDescription

let package = Package(
    name: "WendyWorldClient",
    platforms: [.macOS(.v14), .iOS(.v17), .visionOS(.v1)],
    products: [
        .library(name: "WorldClient", targets: ["WorldClient"]),
        .executable(name: "WorldClientProbe", targets: ["WorldClientProbe"]),
        .executable(name: "ZoneClientProbe", targets: ["ZoneClientProbe"]),
        .library(name: "WorldOperator", targets: ["WorldOperator"]),
        .library(name: "WorldTwin", targets: ["WorldTwin"])
    ],
    targets: [
        .target(name: "WorldClient"),
        .executableTarget(name: "WorldClientProbe", dependencies: ["WorldClient"]),
        .executableTarget(name: "ZoneClientProbe", dependencies: ["WorldClient"]),
        .target(name: "WorldOperator", dependencies: ["WorldClient"]),
        .target(name: "WorldTwin", dependencies: ["WorldClient"]),
        .testTarget(name: "WorldClientTests", dependencies: ["WorldClient"], resources: [.copy("Fixtures")]),
        .testTarget(name: "WorldOperatorTests", dependencies: ["WorldOperator"]),
        .testTarget(name: "WorldTwinTests", dependencies: ["WorldTwin"])
    ]
)
