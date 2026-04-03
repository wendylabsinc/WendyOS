// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "CISwiftBluetooth",
    dependencies: [
        .package(url: "https://github.com/apple/swift-container-plugin", from: "1.0.0")
    ],
    targets: [
        .executableTarget(name: "CISwiftBluetooth")
    ]
)
