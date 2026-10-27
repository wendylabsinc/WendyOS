// swift-tools-version: 6.2
import PackageDescription

let package = Package(
    name: "HelloWorld",
    platforms: [.macOS(.v15)],
    dependencies: [.package(path: "../../..")],
    targets: [
        .executableTarget(
            name: "HelloWorld",
            dependencies: [.product(name: "WendyCore", package: "wendy-core")]
        ),
    ]
)
