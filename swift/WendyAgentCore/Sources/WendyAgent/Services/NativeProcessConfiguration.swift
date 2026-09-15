import Darwin
import Foundation
import GRPCCore

typealias PIDBirthTimeLookup = @Sendable (Int32) -> UInt64?

/// Resolution is repeated before launch, so saved configuration cannot bypass
/// containment if a synced directory has since become a symlink.
enum NativeProcessConfiguration {
    static func contained(_ value: String, directory: String) throws -> String {
        guard !value.contains("\0"), !value.contains("\\"),
            !value.split(separator: "/").contains("..")
        else {
            throw RPCError(
                code: .invalidArgument,
                message: "Native path must remain inside the app directory: \(value)"
            )
        }
        let base = URL(fileURLWithPath: directory).resolvingSymlinksInPath().standardizedFileURL
        let candidate =
            (value.hasPrefix("/")
            ? URL(fileURLWithPath: value)
            : base.appendingPathComponent(value.isEmpty ? "." : value))
            .resolvingSymlinksInPath().standardizedFileURL
        guard candidate.path == base.path || candidate.path.hasPrefix(base.path + "/") else {
            throw RPCError(
                code: .invalidArgument,
                message: "Native path escapes the app directory: \(value)"
            )
        }
        return candidate.path
    }

    static func executable(_ command: String, directory: String) throws -> String {
        guard !command.isEmpty, !command.contains("\0") else {
            throw RPCError(
                code: .invalidArgument,
                message: "Native command must name an executable"
            )
        }
        let executable =
            command.hasPrefix("/")
            ? URL(fileURLWithPath: command).resolvingSymlinksInPath().path
            : try contained(command, directory: directory)
        var isDirectory: ObjCBool = false
        guard FileManager.default.fileExists(atPath: executable, isDirectory: &isDirectory),
            !isDirectory.boolValue, FileManager.default.isExecutableFile(atPath: executable)
        else {
            throw RPCError(
                code: .notFound,
                message:
                    "Native executable is unavailable at \(executable) after installing the Brewfile. Check run.command and synced file permissions."
            )
        }
        return executable
    }

    static func workingDirectory(_ value: String, directory: String) throws -> String {
        let resolved = try contained(value, directory: directory)
        var isDirectory: ObjCBool = false
        guard FileManager.default.fileExists(atPath: resolved, isDirectory: &isDirectory),
            isDirectory.boolValue
        else {
            throw RPCError(
                code: .notFound,
                message: "Native working directory does not exist: \(resolved)"
            )
        }
        return resolved
    }

    static func environment(_ entries: [String]) throws -> [String: String] {
        var result: [String: String] = [:]
        for entry in entries {
            guard let separator = entry.firstIndex(of: "="), separator != entry.startIndex,
                !entry.contains("\0")
            else {
                throw RPCError(
                    code: .invalidArgument,
                    message: "Native environment entries must use KEY=VALUE"
                )
            }
            result[String(entry[..<separator])] = String(entry[entry.index(after: separator)...])
        }
        return result
    }

    static func birthTime(forPID pid: Int32) -> UInt64? {
        guard pid > 0 else { return nil }
        var info = proc_bsdinfo()
        let size = Int32(MemoryLayout<proc_bsdinfo>.stride)
        guard proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &info, size) == size else { return nil }
        return info.pbi_start_tvsec * 1_000_000 + info.pbi_start_tvusec
    }
}
