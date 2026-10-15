import Foundation

@MainActor
struct NetworkAccessPreferences {
    private let defaults: UserDefaults
    private static let key = "allowNetworkAccess"

    init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
    }

    var hasChosenNetworkAccess: Bool {
        self.defaults.object(forKey: Self.key) != nil
    }

    /// Resolve migration before the agent creates state on its first startup.
    /// Return whether a fresh install should show the optional access prompt.
    func prepareForStartup(existingInstallation: Bool) -> Bool {
        guard !self.hasChosenNetworkAccess else { return false }
        self.allowsNetworkAccess = existingInstallation
        return !existingInstallation
    }

    static var existingInstallation: Bool {
        guard let identifier = Bundle.main.bundleIdentifier,
            let support = FileManager.default.urls(
                for: .applicationSupportDirectory,
                in: .userDomainMask
            ).first
        else { return false }
        return FileManager.default.fileExists(
            atPath: support.appendingPathComponent(identifier, isDirectory: true).path
        ) || UserDefaults.standard.object(forKey: "launchAtLoginEnabled") != nil
    }

    // Fresh installs stay local until the user explicitly opts in.
    var allowsNetworkAccess: Bool {
        get { self.defaults.bool(forKey: Self.key) }
        nonmutating set { self.defaults.set(newValue, forKey: Self.key) }
    }
}
