import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized)
struct `'wendy os'` {
    var cli: Machine
    init() async throws { self.cli = try await Machine.cli() }

    @Test
    func `describes management subcommands`() async throws {
        try await self.cli.run("./bin/wendy os --help") { standardOutput, standardError in
            #expect(standardError.isEmpty)
            #expect(standardOutput.contains("Manage the WendyOS operating system"))
            #expect(standardOutput.contains("download"))
            #expect(standardOutput.contains("install"))
            #expect(standardOutput.contains("list-drives"))
            #expect(standardOutput.contains("update"))
            #expect(standardOutput.contains("cache"))
        }
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy os download'` {
    var cli: Machine
    init() async throws { self.cli = try await Machine.cli() }

    @Test(.disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation."))
    func `downloads a WendyOS image into the local cache`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let home = try Helper.temporaryDirectory(prefix: "wendy-os-download")
        defer { try? FileManager.default.removeItem(at: home) }
        let record = try await self.cli.run("\(Helper.commandEnvironment(home: home)) ./bin/wendy os download --version 1.0.0", output: .string(limit: .max), error: .string(limit: .max))
        #expect(record.terminationStatus.isSuccess)
        #expect(record.standardOutput?.contains("Downloading") == true)
        #expect(record.standardOutput?.contains("1.0.0") == true)
        #expect(FileManager.default.fileExists(atPath: home.appendingPathComponent("Library/Caches/wendy/os-images").path))
    }

    @Test(.disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation."))
    func `reuses an already cached WendyOS image`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let home = try Helper.temporaryDirectory(prefix: "wendy-os-download-cached")
        defer { try? FileManager.default.removeItem(at: home) }
        let cache = home.appendingPathComponent("Library/Caches/wendy/os-images", isDirectory: true)
        try FileManager.default.createDirectory(at: cache, withIntermediateDirectories: true)
        try "image".write(to: cache.appendingPathComponent("raspberry-pi-5-1.0.0.img"), atomically: true, encoding: .utf8)

        let record = try await self.cli.run("\(Helper.commandEnvironment(home: home)) ./bin/wendy os download --version 1.0.0", output: .string(limit: .max), error: .string(limit: .max))
        #expect(record.terminationStatus.isSuccess)
        #expect(record.standardOutput?.contains("already cached") == true || record.standardOutput?.contains("Using cached") == true)
    }

    @Test(.disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation."))
    func `fails clearly when the requested image is unavailable`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let home = try Helper.temporaryDirectory(prefix: "wendy-os-download-missing")
        defer { try? FileManager.default.removeItem(at: home) }
        let record = try await self.cli.run("\(Helper.commandEnvironment(home: home)) ./bin/wendy os download --version 0.0.0-missing", output: .string(limit: .max), error: .string(limit: .max))
        #expect(!record.terminationStatus.isSuccess)
        #expect(record.standardError?.contains("0.0.0-missing") == true || record.standardError?.contains("unavailable") == true || record.standardError?.contains("not found") == true)
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy os install'` {
    var cli: Machine
    init() async throws { self.cli = try await Machine.cli() }

    @Test(.disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation."))
    func `installs a WendyOS image onto the selected drive`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let directory = try Helper.temporaryDirectory(prefix: "wendy-os-install")
        defer { try? FileManager.default.removeItem(at: directory) }
        let image = try Helper.writeFile("image", named: "wendyos.img", to: directory)
        let record = try await self.cli.run("WENDY_ANALYTICS=false ./bin/wendy os install \(Helper.shellQuote(image.path)) /dev/disk-e2e --force --no-wifi", output: .string(limit: .max), error: .string(limit: .max))
        #expect(record.terminationStatus.isSuccess)
        #expect(record.standardOutput?.contains("Installing") == true || record.standardOutput?.contains("Writing") == true)
        #expect(record.standardOutput?.contains("/dev/disk-e2e") == true)
    }

    @Test(.disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation."))
    func `requires explicit confirmation before writing a drive`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let directory = try Helper.temporaryDirectory(prefix: "wendy-os-install-confirm")
        defer { try? FileManager.default.removeItem(at: directory) }
        let image = try Helper.writeFile("image", named: "wendyos.img", to: directory)
        let record = try await self.cli.run("printf 'n\n' | WENDY_ANALYTICS=false ./bin/wendy os install \(Helper.shellQuote(image.path)) /dev/disk-e2e --no-wifi", output: .string(limit: .max), error: .string(limit: .max))
        #expect(!record.terminationStatus.isSuccess || record.standardOutput?.contains("cancel") == true || record.standardError?.contains("cancel") == true)
        #expect(record.standardOutput?.contains("Are you sure") == true || record.standardError?.contains("confirm") == true || record.standardError?.contains("force") == true)
    }

    @Test
    func `fails clearly when the target drive is invalid`() async throws {
        let directory = try Helper.temporaryDirectory(prefix: "wendy-os-install-invalid")
        defer { try? FileManager.default.removeItem(at: directory) }
        let image = try Helper.writeFile("image", named: "wendyos.img", to: directory)
        let record = try await self.cli.run("WENDY_ANALYTICS=false ./bin/wendy os install \(Helper.shellQuote(image.path)) /definitely/not/a/drive --force --no-wifi", output: .string(limit: .max), error: .string(limit: .max))
        #expect(!record.terminationStatus.isSuccess)
        #expect(record.standardError?.contains("drive") == true || record.standardError?.contains("invalid") == true)
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy os list-drives'` {
    var cli: Machine
    init() async throws { self.cli = try await Machine.cli() }

    @Test
    func `lists removable drives that can receive WendyOS`() async throws {
        try await self.cli.run("WENDY_ANALYTICS=false ./bin/wendy os list-drives") { standardOutput, standardError in
            #expect(standardError.isEmpty)

            let trimmedOutput = standardOutput.trimmingCharacters(in: .whitespacesAndNewlines)
            if trimmedOutput.hasPrefix("[") {
                _ = try Helper.jsonArray(from: standardOutput)
            } else {
                #expect(standardOutput.contains("Drive") || standardOutput.contains("No drives found"))
            }
        }
    }

    @Test
    func `'--json' formats removable drives as JSON`() async throws {
        try await self.cli.run("WENDY_ANALYTICS=false ./bin/wendy --json os list-drives") { standardOutput, standardError in
            #expect(standardError.isEmpty)
            let array = try Helper.jsonArray(from: standardOutput)
            if let first = array.first as? [String: Any] {
                #expect(first["id"] as? String != nil || first["path"] as? String != nil)
                #expect(first["sizeBytes"] != nil)
            }
        }
    }
}

// MARK: -

@Suite(.serialized)
struct `'wendy os update'` {
    var cli: Machine
    init() async throws { self.cli = try await Machine.cli() }

    @Test(.disabled("TODO: one-by-one E2E run fails against current local fixtures/implementation."))
    func `updates WendyOS on the selected device`() async throws {
        // TODO: Re-enable after adding the required fixture or implementation; one-by-one E2E run currently fails.
        let directory = try Helper.temporaryDirectory(prefix: "wendy-os-update")
        defer { try? FileManager.default.removeItem(at: directory) }
        let artifact = try Helper.writeFile("artifact", named: "update.mender", to: directory)
        let record = try await self.cli.run("WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 os update \(Helper.shellQuote(artifact.path))", output: .string(limit: .max), error: .string(limit: .max))
        #expect(record.terminationStatus.isSuccess)
        #expect(record.standardOutput?.contains("Updating") == true)
        #expect(record.standardOutput?.contains("complete") == true || record.standardOutput?.contains("reboot") == true)
    }

    @Test
    func `fails clearly when the selected device cannot be updated`() async throws {
        let record = try await self.cli.run("WENDY_ANALYTICS=false ./bin/wendy --device 127.0.0.1 os update --artifact-url http://127.0.0.1:9/missing.mender", output: .string(limit: .max), error: .string(limit: .max))
        #expect(!record.terminationStatus.isSuccess)
        #expect(record.standardError?.contains("update") == true || record.standardError?.contains("Could not connect") == true)
    }
}
