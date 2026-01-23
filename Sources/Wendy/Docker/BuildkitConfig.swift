import TOMLKit

/// Helper for managing buildkit TOML configuration using structured parsing
struct BuildkitConfig {
    private var table: TOMLTable

    init() {
        self.table = TOMLTable()
    }

    init(parsing toml: String) throws {
        self.table = try TOMLTable(string: toml)
    }

    /// Check if a registry is already configured
    func hasRegistry(hostname: String, port: Int) -> Bool {
        let key = "\(hostname):\(port)"
        guard let registry = table["registry"] as? TOMLTable else { return false }
        return registry[key] != nil
    }

    /// Add a registry configuration
    mutating func addRegistry(hostname: String, port: Int, http: Bool = true, insecure: Bool = true)
    {
        let key = "\(hostname):\(port)"

        // Get or create registry section
        let registry = (table["registry"] as? TOMLTable) ?? TOMLTable()

        // Create registry entry
        let entry = TOMLTable()
        entry["http"] = http
        entry["insecure"] = insecure

        registry[key] = entry
        table["registry"] = registry
    }

    /// Encode to TOML string with consistent formatting
    func toTOML() -> String {
        return table.convert(to: .toml)
    }

    /// Safely parse config file, falling back to empty config on errors
    static func loadOrCreate(from path: String) -> BuildkitConfig {
        guard let content = try? String(contentsOfFile: path, encoding: .utf8),
            !content.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
        else {
            return BuildkitConfig()
        }

        do {
            return try BuildkitConfig(parsing: content)
        } catch {
            // If existing config is malformed, start fresh
            return BuildkitConfig()
        }
    }
}
