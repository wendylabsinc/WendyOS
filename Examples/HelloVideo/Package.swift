// swift-tools-version: 6.0
// The swift-tools-version declares the minimum version of Swift required to build this package.

import PackageDescription

let package = Package(
    name: "HelloVideo",
    platforms: [
        .macOS(.v15)
    ],
    dependencies: [
        .package(url: "https://github.com/hummingbird-project/hummingbird.git", from: "2.10.0"),
        .package(url: "https://github.com/tayloraswift/swift-jpeg.git", from: "2.0.0"),
        .package(url: "https://github.com/apple/swift-container-plugin", from: "1.0.0"),
    ],
    targets: [
        // Targets are the basic building blocks of a package, defining a module or a test suite.
        // Targets can depend on other targets in this package and products from dependencies.
        .target(
            name: "LinuxVideo",
            dependencies: [
                .target(name: "CLinuxVideo"),
                .product(name: "JPEG", package: "swift-jpeg"),
            ]
        ),
        .systemLibrary(
            name: "CLinuxVideo"
        ),
        .executableTarget(
            name: "HelloVideo",
            dependencies: [
                .product(name: "Hummingbird", package: "hummingbird"),
                .target(name: "LinuxVideo"),
            ]
        ),
    ]
)
