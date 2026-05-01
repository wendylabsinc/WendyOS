import Foundation

let name = "Wendy"
let os = ProcessInfo.processInfo.operatingSystemVersionString

#if os(macOS)
let platform = "macOS"
#elseif os(Linux)
let platform = "Linux"
#else
let platform = "Unknown"
#endif

#if arch(arm64)
let arch = "arm64"
#elseif arch(x86_64)
let arch = "x86_64"
#else
let arch = "unknown"
#endif

print("Hello, \(name)! Running on \(platform)/\(arch) (\(os))")
