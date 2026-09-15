import Foundation
import GRPCCore
import Testing

@testable import WendyAgentCore

@Suite("Native process configuration")
struct NativeProcessConfigurationTests {
    @Test("relative commands and working directories reject traversal and symlink escapes")
    func paths() throws {
        let base = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: base, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: base) }
        let sub = base.appendingPathComponent("sub")
        try FileManager.default.createDirectory(at: sub, withIntermediateDirectories: true)
        try FileManager.default.createSymbolicLink(
            atPath: base.appendingPathComponent("escape").path,
            withDestinationPath: "/usr/bin"
        )
        #expect(throws: RPCError.self) {
            try NativeProcessConfiguration.executable("escape/true", directory: base.path)
        }
        #expect(throws: RPCError.self) {
            try NativeProcessConfiguration.workingDirectory("escape", directory: base.path)
        }
        #expect(throws: RPCError.self) {
            try NativeProcessConfiguration.workingDirectory("../outside", directory: base.path)
        }
        #expect(
            try NativeProcessConfiguration.workingDirectory("sub", directory: base.path)
                == sub.resolvingSymlinksInPath().path
        )
        #expect(
            try NativeProcessConfiguration.executable("/usr/bin/true", directory: base.path)
                == "/usr/bin/true"
        )
        #expect(throws: RPCError.self) {
            try NativeProcessConfiguration.executable("missing", directory: base.path)
        }
    }

    @Test("request environment wins over the host before agent identity and telemetry")
    func environment() throws {
        let overrides = try NativeProcessConfiguration.environment([
            "MODE=first", "MODE=last", "EMPTY=", "VALUE=a=b", "WENDY_APP_ID=wrong",
            "OTEL_SERVICE_NAME=wrong",
        ])
        let env = ContainerService.nativeAppEnvironment(
            appName: "my.app",
            otelPort: 4321,
            source: ["MODE": "host", "PATH": "/bin"],
            overrides: overrides
        )
        #expect(env["MODE"] == "last")
        #expect(env["EMPTY"] == "")
        #expect(env["VALUE"] == "a=b")
        #expect(env["PATH"] == "/bin")
        #expect(env["WENDY_APP_ID"] == "my.app")
        #expect(env["OTEL_SERVICE_NAME"] == "my.app")
        #expect(env["OTEL_EXPORTER_OTLP_ENDPOINT"] == "http://127.0.0.1:4321")
    }

    @Test("old native launch metadata decodes without the new optional fields")
    func legacy() throws {
        let data = Data(#"{"directory":"/tmp/app","binaryName":"app","args":[]}"#.utf8)
        let native = try JSONDecoder().decode(WendyApp.NativeMetadata.self, from: data)
        #expect(native.environment == nil)
        #expect(native.executablePath == nil)
    }

    @Test("kernel birth identity is stable for a live process")
    func processIdentity() throws {
        let pid = ProcessInfo.processInfo.processIdentifier
        let birth = try #require(NativeProcessConfiguration.birthTime(forPID: pid))
        #expect(birth > 0)
        #expect(NativeProcessConfiguration.birthTime(forPID: pid) == birth)
        #expect(NativeProcessConfiguration.birthTime(forPID: -1) == nil)
    }
}
