import ArgumentParser
import Foundation
import Logging
import Noora
import WendyShared

struct AgentConnectionOptions: ParsableArguments {
    struct Endpoint: ExpressibleByArgument {
        let host: String
        var port: Int

        init(host: String, port: Int) {
            self.host = host
            self.port = port
        }

        init?(argument: String) {
            // Create a dummy URL to use URLComponents parsing capabilities
            var urlString = argument
            let hasScheme = urlString.contains("://")

            // Only allow wendy:// scheme or no scheme
            if hasScheme {
                if !urlString.starts(with: "wendy://") {
                    return nil
                }
            } else {
                urlString = "wendy://" + urlString
            }

            guard let components = URLComponents(string: urlString),
                let host = components.host, !host.isEmpty
            else {
                return nil
            }

            // Handle IPv6 addresses by removing the brackets if present
            var cleanHost = host
            if cleanHost.first == "[" && cleanHost.last == "]" {
                cleanHost = String(cleanHost.dropFirst().dropLast())
            }

            self.host = cleanHost
            self.port = components.port ?? 50051
        }

        static var defaultValueDescription: String {
            "localhost:50051"
        }

        var description: String {
            "\(host):\(port)"
        }
    }

    @Option(
        name: .shortAndLong,
        help:
            "The host and port of the Wendy Agent to connect to (format: host or host:port). IPv6 addresses must be enclosed in square brackets, e.g. [2001:db8::1] or [2001:db8::1]:8080. Defaults to the `WENDY_AGENT` environment variable."
    )
    var device: Endpoint?

    @Option(
        name: .shortAndLong,
        help:
            """
            Alias for the `--device` option. (Deprecated)
            If both `--device` and `--device` are provided, the `--device` option takes precedence.
            """
    )
    var agent: Endpoint?

    func read(title: TerminalText?) async throws -> Endpoint {
        if let device {
            return device
        }

        if let agent {
            return agent
        }

        let discovery = PlatformDeviceDiscovery(
            logger: Logger(label: "sh.wendy.cli.find-agent")
        )
        let lanDevices = try await Noora().progressStep(
            message: "Searching for WendyOS devices",
            successMessage: nil,
            errorMessage: nil,
            showSpinner: true
        ) { _ in
            try await discovery.findLANDevices()
        }

        if lanDevices.isEmpty {
            throw NoDevicesFound()
        }

        let device = Noora().singleChoicePrompt(
            title: title,
            question: "Select a device",
            options: lanDevices
        )

        return Endpoint(
            host: device.hostname,
            port: device.port
        )
    }

    var endpoint: Endpoint {
        get throws {
            if let device {
                return device
            }

            if let agent {
                return agent
            }

            if let endpoint = ProcessInfo.processInfo.environment["WENDY_AGENT"],
                let endpoint = Endpoint(argument: endpoint)
            {
                return endpoint
            }

            return Endpoint(host: "edgeos-device.local", port: 50051)
        }
    }
}

struct InvalidEndpoint: Error {}
struct NoDevicesFound: Error, CustomStringConvertible, CustomDebugStringConvertible {
    var description: String {
        "No Wendy devices found"
    }

    var debugDescription: String {
        "No Wendy devices found"
    }
}
