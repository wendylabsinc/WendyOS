import Foundation

enum AppDisplayName {
    static let current = resolve(from: .main)

    static func resolve(from bundle: Bundle) -> String {
        if let displayName = bundle.object(forInfoDictionaryKey: "CFBundleDisplayName") as? String,
            !displayName.isEmpty
        {
            return displayName
        }

        if let bundleName = bundle.object(forInfoDictionaryKey: "CFBundleName") as? String,
            !bundleName.isEmpty
        {
            return bundleName
        }

        return ProcessInfo.processInfo.processName
    }
}
