import ArgumentParser
import CLIOutput
import Foundation

struct AuthCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "auth",
        abstract: "Managed authentication to cloud services",
        shouldDisplay: false,
        subcommands: [
            LoginCommand.self,
            LogoutCommand.self,
            RefreshCertsCommand.self,
        ]
    )
}

struct LoginCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "login",
        abstract: "Log into cloud services"
    )

    @Option
    var cloudDashboard = "https://cloud.wendy.sh"

    @Option
    var cloudGRPC = "cloud.wendy.sh"

    func run() async throws {
        try await loginFlow(
            cloudDashboard: cloudDashboard,
            cloudGRPC: cloudGRPC
        ) { token in
            cliOutput.success("Logged in")
            #if canImport(Darwin)
                Task {
                    try await Task.sleep(for: .seconds(1))
                    Darwin.exit(0)
                }
            #endif
        }
    }
}

struct RefreshCertsCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "refresh-certs",
        abstract: "Refresh the development certificates for your CLI"
    )

    func run() async throws {
        try await withAuth(title: "Refresh certificates") { auth in
            var auth = auth
            var refreshed = false
            for index in auth.certificates.indices {
                if try await refreshCertificateIfNeeded(
                    auth: &auth,
                    certIndex: index,
                    force: true
                ) {
                    refreshed = true
                }
            }
            if !refreshed {
                cliOutput.success("All certificates are up to date")
            }
        }
    }
}

struct LogoutCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "logout",
        abstract: "Log out of cloud services"
    )

    @Option(help: "Cloud dashboard URL to log out from")
    var cloudDashboard: String?

    func run() async throws {
        var config = getConfig()

        if config.auth.isEmpty {
            if JSONMode.isEnabled {
                JSONErrorResponse(
                    error: "no_accounts",
                    reason: "No accounts found to log out from"
                ).print()
            } else {
                cliOutput.error("No accounts found")
            }
            return
        }

        let logout: Config.Auth
        if let cloudDashboard {
            guard let auth = config.auth.first(where: { $0.cloudDashboard == cloudDashboard })
            else {
                if JSONMode.isEnabled {
                    JSONErrorResponse(
                        error: "account_not_found",
                        reason: "No account found for cloud dashboard: \(cloudDashboard)"
                    ).print()
                } else {
                    cliOutput.error(
                        "No account found for cloud dashboard: \(cloudDashboard)"
                    )
                }
                return
            }
            logout = auth
        } else if JSONMode.isEnabled {
            jsonModeRequiresArgument(
                argument: "cloud-dashboard",
                description: "Provide --cloud-dashboard <url> to specify which account to log out"
            )
        } else {
            let options = config.auth.map(\.description)
            let selected = try await cliOutput.singleChoicePrompt(
                title: "Logout",
                question: "Which account do you want to log out of?",
                options: options
            )
            guard let match = config.auth.first(where: { $0.description == selected }) else {
                cliOutput.error("No matching account found")
                return
            }
            logout = match
        }

        config.auth.removeAll { $0 == logout }

        let data = try JSONEncoder().encode(config)
        try data.write(to: configURL)

        if JSONMode.isEnabled {
            struct SuccessResponse: Codable {
                let success: Bool
                let message: String
            }
            let response = SuccessResponse(success: true, message: "Logged out")
            let responseData = try JSONEncoder().encode(response)
            print(String(data: responseData, encoding: .utf8)!)
        }
    }
}
