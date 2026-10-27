// swift-tools-version: 6.2
import Foundation
import PackageDescription

// The Go engine ships as static libraries: an XCFramework for Apple platforms
// and a SwiftPM artifact bundle for Linux. This nested manifest supports local
// path dependencies using the binaries built by `make swift`. Remote Swift
// distribution from WendyOS is unresolved; publication remains disabled.
// BEGIN BINARY TARGETS
let binaryTargets: [Target] = [
    ("CWendyCoreApple", "CWendyCore.xcframework"),
    ("CWendyCoreLinux", "CWendyCore.artifactbundle"),
].compactMap { name, file in
    let path = "distributions/swift/artifacts/" + file
    let exists = FileManager.default.fileExists(atPath: Context.packageDirectory + "/" + path)
    return exists ? .binaryTarget(name: name, path: path) : nil
}
// END BINARY TARGETS

let binaryPlatforms: [String: [Platform]] = [
    "CWendyCoreApple": [.macOS, .iOS],
    "CWendyCoreLinux": [.linux],
]

let package = Package(
    name: "wendy-core",
    platforms: [.macOS(.v15), .iOS(.v18)],
    products: [.library(name: "WendyCore", targets: ["WendyCore"])],
    targets: binaryTargets + [
        .target(
            name: "WendyCore",
            dependencies: binaryTargets.map {
                .target(name: $0.name, condition: .when(platforms: binaryPlatforms[$0.name, default: []]))
            },
            path: "distributions/swift/Sources/WendyCore",
            swiftSettings: [.enableUpcomingFeature("InternalImportsByDefault")],
            // Go's iOS runtime uses CoreFoundation.
            linkerSettings: [.linkedFramework("CoreFoundation", .when(platforms: [.iOS]))]
        ),
        .testTarget(
            name: "WendyCoreTests",
            dependencies: ["WendyCore"],
            path: "distributions/swift/Tests/WendyCoreTests"
        ),
    ]
)
