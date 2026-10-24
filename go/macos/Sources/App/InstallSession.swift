#if os(macOS)
    import AppKit
    import SwiftUI
    import SwiftTerm

    @MainActor
    final class InstallSession: NSObject, ObservableObject, LocalProcessTerminalViewDelegate {
        static let shared = InstallSession()
        enum Operation { case install, verify }
        @Published var board: InstallBoard = .pi5
        @Published var method: InstallMethod = .sd
        @Published var page = 0
        @Published var running = false
        @Published var result: String?
        @Published var installed = false
        @Published var verified = false
        @Published var terminal: LocalProcessTerminalView?
        @Published var error: String?
        @Published var address = ""
        @Published var version = ""
        @Published var requireEnrollment = false
        private(set) var operation = Operation.install
        var guide: InstallGuide { InstallGuide(board: board, method: method) }

        func select(_ board: InstallBoard) {
            guard !running else { return }
            self.board = board
            method = board.methods[0]
        }
        func reset() {
            guard !running else { return }
            terminal = nil
            installed = false
            verified = false
            result = nil
            error = nil
            page = 0
            address = ""
            version = ""
            requireEnrollment = false
        }
        func install() {
            guard !running, board.methods.contains(method) else { return }
            launchBundled(arguments: guide.arguments, operation: .install)
        }
        func verify() {
            guard !running, installed else { return }
            let host = address.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !host.isEmpty else { return }
            var arguments = ["install", "verify", "--address", host]
            if board != .other {
                let release = version.trimmingCharacters(in: .whitespacesAndNewlines)
                guard !release.isEmpty else { return }
                arguments += [
                    "--expected-device-type", board.rawValue, "--expected-os-version", release,
                ]
            }
            if requireEnrollment { arguments += ["--require-enrollment"] }
            launchBundled(arguments: arguments, operation: .verify)
        }
        private func launchBundled(arguments: [String], operation: Operation) {
            guard
                let executable = Bundle.main.executableURL?.deletingLastPathComponent()
                    .appendingPathComponent("wendy"),
                FileManager.default.isExecutableFile(atPath: executable.path)
            else {
                error =
                    "The bundled Wendy installer is missing. Rebuild or reinstall the macOS app."
                return
            }
            launch(executable: executable.path, arguments: arguments, operation: operation)
        }

        // No shell interpolation, no password capture, and no automatic responses to
        // target/erase prompts. The existing CLI owns those interactions in its PTY.
        func launch(executable: String, arguments: [String], operation: Operation) {
            guard !running else { return }
            self.operation = operation
            error = nil
            result = nil
            verified = false
            if operation == .install { installed = false }
            let view = LocalProcessTerminalView(frame: NSRect(x: 0, y: 0, width: 780, height: 420))
            view.font = .monospacedSystemFont(ofSize: 12, weight: .regular)
            view.processDelegate = self
            terminal = view
            running = true
            page = 2
            // The terminal starts with an explicit frame and forwards later
            // layout changes to the PTY through SwiftTerm.
            view.startProcess(
                executable: executable,
                args: arguments,
                environment: Self.environment(),
                currentDirectory: NSHomeDirectory()
            )
            if !view.process.running {
                running = false
                error = "The installer could not start. No write was started."
            }
            view.window?.makeFirstResponder(view)
        }

        static func environment(
            source: [String: String] = ProcessInfo.processInfo.environment
        ) -> [String] {
            // Do not inherit an agent target, CI settings, or shell startup scripts.
            var env = source.filter {
                [
                    "HOME", "USER", "LOGNAME", "TMPDIR", "LANG", "LC_CTYPE", "SSH_AUTH_SOCK",
                    "XDG_CACHE_HOME", "WENDY_CONFIG_DIR", "WENDY_ANALYTICS", "HTTP_PROXY",
                    "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
                ].contains(
                    $0.key
                )
            }
            env["PATH"] = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
            env["WENDY_DESKTOP_INSTALLER"] = "1"
            env["TERM"] = "xterm-256color"
            env["COLORTERM"] = "truecolor"
            env["LANG"] = env["LANG"] ?? "en_US.UTF-8"
            return env.sorted { $0.key < $1.key }.map { "\($0.key)=\($0.value)" }
        }
        func interrupt() {
            guard running else { return }
            guard let terminal else { return }
            terminal.send(source: terminal, data: [3][...])
        }
        nonisolated func sizeChanged(source: LocalProcessTerminalView, newCols: Int, newRows: Int) {
        }
        nonisolated func setTerminalTitle(source: LocalProcessTerminalView, title: String) {}
        nonisolated func hostCurrentDirectoryUpdate(source: TerminalView, directory: String?) {}
        nonisolated func processTerminated(source: TerminalView, exitCode: Int32?) {
            // LocalProcessTerminalView uses SwiftTerm's default main queue.
            let sourceID = ObjectIdentifier(source)
            MainActor.assumeIsolated {
                guard self.terminal.map(ObjectIdentifier.init) == sourceID else { return }
                self.running = false
                if exitCode == 0 {
                    if self.operation == .install {
                        self.installed = true
                        self.result =
                            "Installer finished. Follow the boot steps, then verify the device."
                    } else {
                        self.verified = true
                        self.result =
                            "First boot verified at \(self.address). Application readiness has not been tested."
                    }
                } else {
                    self.result =
                        self.operation == .install
                        ? "Installation did not finish successfully. Review the output before retrying; the target may have been partially written."
                        : "First boot was not verified. Check the address, installed version and output, then try again."
                }
            }
        }
    }

    struct InstallTerminal: NSViewRepresentable {
        let terminal: LocalProcessTerminalView
        func makeNSView(context: Context) -> LocalProcessTerminalView { terminal }
        func updateNSView(_ nsView: LocalProcessTerminalView, context: Context) {}
    }
#endif
