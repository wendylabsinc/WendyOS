import AVKit
import SwiftTerm
import SwiftUI
import WebKit
import XCTest

@testable import WendyDesktop

final class InstallTests: XCTestCase {
    func testRecoveryAndMediaRoutesStaySeparate() {
        let recovery = InstallGuide(board: .orinNano, method: .recoveryNVMe)
        XCTAssertEqual(
            recovery.arguments,
            [
                "install", "--device-type", "jetson-orin-nano", "--storage", "nvme",
                "--rootfs-only=false",
            ]
        )
        XCTAssertNil(
            recovery.preparationVideo,
            "Recovery must not show an external NVMe enclosure video"
        )
        let rootfs = InstallGuide(board: .orinNano, method: .rootfsNVMe)
        XCTAssertTrue(rootfs.arguments.contains("--rootfs-only"))
        XCTAssertEqual(rootfs.preparationVideo, "jetson-nvme-mac-adapter")
        XCTAssertTrue(rootfs.eraseScope.contains("unchanged"))
        XCTAssertFalse(InstallBoard.agxOrin.methods.contains(.rootfsSD))
        XCTAssertFalse(InstallBoard.orinNano.methods.contains(.recoveryEMMC))
        XCTAssertEqual(
            InstallGuide(board: .iq9075, method: .edl).arguments,
            ["install", "--device-type", "dragonwing-iq-9075"]
        )
        for board in InstallBoard.allCases {
            for method in board.methods {
                let args = InstallGuide(board: board, method: method).arguments
                XCTAssertFalse(args.contains("--force"))
                XCTAssertFalse(args.contains("--yes-overwrite-internal"))
                XCTAssertFalse(args.contains("--drive"))
            }
        }
    }
    func testPhysicalGuidesAndBundledMedia() {
        for board in InstallBoard.allCases {
            for method in board.methods {
                let guide = InstallGuide(board: board, method: method)
                XCTAssertFalse(guide.preparation.isEmpty)
                XCTAssertFalse(guide.afterInstall.isEmpty)
                for name in [guide.preparationVideo, guide.bootVideo].compactMap({ $0 }) {
                    XCTAssertNotNil(
                        Bundle.main.url(
                            forResource: name,
                            withExtension: "mp4",
                            subdirectory: "installation"
                        ),
                        name
                    )
                }
            }
        }
        let sd = InstallGuide(board: .pi5, method: .sd)
        let nvme = InstallGuide(board: .pi5, method: .nvme)
        XCTAssertNotEqual(sd.preparationVideo, nvme.preparationVideo)
        XCTAssertTrue(nvme.afterInstall.joined().contains("M.2 HAT"))
        let dragon = InstallGuide(board: .iq8275, method: .edl)
        XCTAssertTrue(dragon.preparation.joined().contains("S2-8"))
        XCTAssertTrue(dragon.afterInstall.joined().contains("OFF"))
        let nano = InstallGuide(board: .orinNano, method: .recoveryNVMe)
        XCTAssertTrue(nano.preparation.joined().contains("pin 9 (GND) and pin 10 (FC REC)"))
    }
    @MainActor func testInstallerEnvironmentDoesNotInheritRemoteTargetOrAutomation() {
        let env = InstallSession.environment(source: [
            "HOME": "/tmp/test", "WENDY_AGENT_SOCKET": "/tmp/remote", "CI": "true",
            "BASH_ENV": "/tmp/startup", "USER": "test",
        ])
        XCTAssertTrue(env.contains("HOME=/tmp/test"))
        XCTAssertTrue(env.contains("TERM=xterm-256color"))
        XCTAssertFalse(
            env.contains(where: {
                $0.hasPrefix("WENDY_AGENT_SOCKET=") || $0.hasPrefix("CI=")
                    || $0.hasPrefix("BASH_ENV=")
            })
        )
    }
    @MainActor func testPTYReportsSuccessAndFailureAndPreventsParallelLaunch() async throws {
        let session = InstallSession()
        session.launch(
            executable: "/bin/sh",
            arguments: ["-c", "test -t 0 && test -t 1"],
            operation: .install
        )
        session.launch(executable: "/usr/bin/false", arguments: [], operation: .install)
        for _ in 0..<100 where session.running { try await Task.sleep(for: .milliseconds(50)) }
        XCTAssertFalse(session.running)
        XCTAssertTrue(session.installed, session.result ?? session.error ?? "no result")
        XCTAssertFalse(session.verified)
        session.launch(executable: "/usr/bin/false", arguments: [], operation: .verify)
        for _ in 0..<100 where session.running { try await Task.sleep(for: .milliseconds(50)) }
        XCTAssertFalse(session.running)
        XCTAssertFalse(session.verified)
        XCTAssertTrue(session.installed)
        XCTAssertTrue(session.result?.contains("not verified") == true)
    }
    @MainActor func testCompletedInstallerCannotBeChangedByDuplicateExitCallbacks() {
        for firstExit: Int32? in [256, nil] {
            let session = InstallSession()
            let terminal = LocalProcessTerminalView(frame: .zero)
            session.terminal = terminal
            session.running = true
            session.processTerminated(source: terminal, exitCode: firstExit)
            let failure = session.result
            XCTAssertFalse(session.installed)
            XCTAssertFalse(session.running)

            // A second notification after the child has already been reaped
            // must not turn a failed or unknown exit into success.
            session.processTerminated(source: terminal, exitCode: 0)
            XCTAssertFalse(session.installed)
            XCTAssertFalse(session.verified)
            XCTAssertEqual(session.result, failure)
        }
    }
    @MainActor func testFabricatedZeroCannotOverrideActualFailedCommandReceipt() async throws {
        let receipt = try InstallerExitReceipt()
        defer { receipt.remove() }
        let child = Process()
        child.executableURL = URL(fileURLWithPath: "/bin/sh")
        child.arguments = receipt.arguments(executable: "/usr/bin/false", arguments: [])
        try child.run()
        for _ in 0..<100 where child.isRunning { try await Task.sleep(for: .milliseconds(10)) }
        XCTAssertFalse(child.isRunning)
        XCTAssertEqual(receipt.exitStatus(), 1)
        let session = InstallSession()
        let terminal = LocalProcessTerminalView(frame: .zero)
        session.terminal = terminal
        session.exitReceipt = receipt
        session.running = true
        // A stale terminal cannot consume the active command's receipt.
        session.processTerminated(source: LocalProcessTerminalView(frame: .zero), exitCode: 0)
        XCTAssertTrue(session.running)
        // SwiftTerm 1.18 can fabricate this FIRST zero when waitpid returns no
        // status. The real shell has independently waited for /usr/bin/false.
        session.processTerminated(source: terminal, exitCode: 0)
        XCTAssertFalse(session.running)
        XCTAssertFalse(session.installed)
        let failure = session.result
        session.processTerminated(source: terminal, exitCode: 0)
        XCTAssertFalse(session.installed)
        XCTAssertEqual(session.result, failure)
        XCTAssertFalse(FileManager.default.fileExists(atPath: receipt.directory.path))
    }
    @MainActor func testInstallerExitReceiptRejectsUnknownAndUnownedStatus() throws {
        let receipt = try InstallerExitReceipt()
        defer { receipt.remove() }
        let status = receipt.directory.appendingPathComponent("status")
        XCTAssertNil(receipt.exitStatus())
        for text in [
            "", "0\n", "foreign:0\n", "\(receipt.nonce):0", "\(receipt.nonce):00\n",
            "\(receipt.nonce):-1\n", "\(receipt.nonce):256\n", "\(receipt.nonce):0\nextra",
            String(repeating: "x", count: 65),
        ] {
            try Data(text.utf8).write(to: status)
            try FileManager.default.setAttributes(
                [.posixPermissions: 0o600],
                ofItemAtPath: status.path
            )
            XCTAssertNil(receipt.exitStatus(), text)
        }
        try Data("\(receipt.nonce):0\n".utf8).write(to: status)
        try FileManager.default.setAttributes([.posixPermissions: 0o644], ofItemAtPath: status.path)
        XCTAssertNil(receipt.exitStatus())
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: status.path)
        XCTAssertEqual(receipt.exitStatus(), 0)
        let foreign = receipt.directory.appendingPathComponent("foreign")
        try FileManager.default.moveItem(at: status, to: foreign)
        try FileManager.default.createSymbolicLink(at: status, withDestinationURL: foreign)
        XCTAssertNil(receipt.exitStatus())
        receipt.remove()
        XCTAssertTrue(FileManager.default.fileExists(atPath: foreign.path))
        try FileManager.default.removeItem(at: foreign)
        receipt.remove()
    }
    @MainActor func testPTYReceiptPreservesLiteralArgumentsAndFailureAcrossSessions() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(
            UUID().uuidString
        )
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false)
        defer { try? FileManager.default.removeItem(at: directory) }
        let injected = directory.appendingPathComponent("must-not-exist")
        let literal = "$(/usr/bin/touch '\(injected.path)'); `touch '\(injected.path)'`"
        for iteration in 0..<20 {
            let session = InstallSession()
            session.launch(
                executable: "/bin/sh",
                arguments: [
                    "-c", "test -t 0 && test -t 1 && test \"$1\" = \"$2\"", "literal", literal,
                    literal,
                ],
                operation: .install
            )
            for _ in 0..<100 where session.running { try await Task.sleep(for: .milliseconds(10)) }
            XCTAssertFalse(session.running, "iteration \(iteration)")
            XCTAssertTrue(
                session.installed,
                session.result ?? session.error ?? "missing install result"
            )
            session.launch(executable: "/usr/bin/false", arguments: [literal], operation: .verify)
            for _ in 0..<100 where session.running { try await Task.sleep(for: .milliseconds(10)) }
            XCTAssertFalse(session.running)
            XCTAssertFalse(session.verified)
            XCTAssertTrue(session.installed)
            XCTAssertTrue(session.result?.contains("not verified") == true)
            XCTAssertNil(session.exitReceipt)
        }
        XCTAssertFalse(FileManager.default.fileExists(atPath: injected.path))
    }
    @MainActor func testReceiptWrapperRefusesAnExistingStaleRecord() async throws {
        let receipt = try InstallerExitReceipt()
        defer { receipt.remove() }
        let status = receipt.directory.appendingPathComponent("status")
        try Data("stale-invocation:0\n".utf8).write(to: status)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: status.path)
        let child = Process()
        child.executableURL = URL(fileURLWithPath: "/bin/sh")
        child.arguments = receipt.arguments(executable: "/usr/bin/true", arguments: [])
        child.standardError = FileHandle.nullDevice
        try child.run()
        for _ in 0..<100 where child.isRunning { try await Task.sleep(for: .milliseconds(10)) }
        XCTAssertFalse(child.isRunning)
        XCTAssertEqual(child.terminationStatus, 125)
        XCTAssertEqual(try String(contentsOf: status, encoding: .utf8), "stale-invocation:0\n")
        XCTAssertNil(receipt.exitStatus())
    }
    @MainActor func testPTYInterruptCannotBecomeVerified() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(
            UUID().uuidString
        )
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false)
        defer { try? FileManager.default.removeItem(at: directory) }
        let childPID = directory.appendingPathComponent("child.pid")
        let session = InstallSession()
        session.installed = true
        session.launch(
            executable: "/bin/sh",
            arguments: [
                "-c",
                "printf '%s\\n' \"$$\" > \"$1\"; exec /bin/sleep 30", "owned-interrupt",
                childPID.path,
            ],
            operation: .verify
        )
        for _ in 0..<100 where !FileManager.default.fileExists(atPath: childPID.path) {
            try await Task.sleep(for: .milliseconds(10))
        }
        let pid = try XCTUnwrap(
            Int32(
                try String(contentsOf: childPID, encoding: .utf8).trimmingCharacters(
                    in: .whitespacesAndNewlines
                )
            )
        )
        XCTAssertTrue(session.running)
        session.interrupt()
        for _ in 0..<200 where session.running { try await Task.sleep(for: .milliseconds(10)) }
        XCTAssertFalse(session.running)
        XCTAssertFalse(session.verified)
        XCTAssertTrue(session.installed)
        XCTAssertNil(session.exitReceipt)
        XCTAssertEqual(kill(pid, 0), -1, "owned interrupted command was not reaped by its wrapper")
        XCTAssertEqual(errno, ESRCH)
    }
    @MainActor func testBundled3DGuidesLoadOfflineInWebKit() async throws {
        for board in ["jetson-orin-nano", "dragonwing-iq-9075", "raspberry-pi-5"] {
            let config = WKWebViewConfiguration()
            let root = try XCTUnwrap(Bundle.main.resourceURL?.appendingPathComponent("Install3D"))
            config.setURLSchemeHandler(
                InstallAssetScheme(root: root),
                forURLScheme: "wendy-install"
            )
            let web = WKWebView(
                frame: NSRect(x: 0, y: 0, width: 900, height: 490),
                configuration: config
            )
            let window = NSWindow(
                contentRect: web.frame,
                styleMask: [.titled],
                backing: .buffered,
                defer: false
            )
            window.contentView = web
            window.orderFront(nil)
            defer {
                window.orderOut(nil)
                window.contentView = nil
                web.stopLoading()
            }
            web.load(
                URLRequest(url: URL(string: "wendy-install://bundle/index.html?board=\(board)")!)
            )
            var ready = false
            var failure: String?
            for _ in 0..<150 {
                ready =
                    (try? await web.evaluateJavaScript("window.guideReady === true")) as? Bool
                    == true
                failure =
                    (try? await web.evaluateJavaScript("window.guideError || null")) as? String
                if ready || failure != nil { break }
                try await Task.sleep(for: .milliseconds(200))
            }
            XCTAssertTrue(ready, "\(board): \(failure ?? "3D guide timed out")")
            if ready {
                let step = board == "jetson-orin-nano" ? 2 : (board == "dragonwing-iq-9075" ? 1 : 0)
                _ = try await web.evaluateJavaScript("window.installGuide.select(\(step))")
                let screenshot = try await web.takeSnapshot(configuration: nil)
                let attachment = XCTAttachment(image: screenshot)
                attachment.name = "3D setup \(board)"
                attachment.lifetime = .keepAlways
                add(attachment)
            }
        }
    }

    @MainActor func testThorGuideMountsNativeVideoPlayer() async throws {
        let guide = InstallGuidanceView(guide: InstallGuide(board: .thor, method: .thorRecovery))
        let host = NSHostingView(rootView: guide.frame(width: 700, height: 600))
        let window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 700, height: 600),
            styleMask: [.titled],
            backing: .buffered,
            defer: false
        )
        window.contentView = host
        window.orderFront(nil)
        defer {
            window.orderOut(nil)
            window.contentView = nil
        }
        try await Task.sleep(for: .milliseconds(500))
        host.layoutSubtreeIfNeeded()
        func players(_ view: NSView) -> [AVPlayerView] {
            (view as? AVPlayerView).map { [$0] } ?? view.subviews.flatMap(players)
        }
        let video = try XCTUnwrap(players(host).first)
        XCTAssertNotNil(video.player?.currentItem)
    }

    @MainActor func testBundledInstallerStartsInPTY() async throws {
        let cli = try XCTUnwrap(Bundle.main.url(forAuxiliaryExecutable: "wendy"))
        let session = InstallSession()
        session.launch(executable: cli.path, arguments: ["install", "--help"], operation: .install)
        for _ in 0..<200 where session.running { try await Task.sleep(for: .milliseconds(50)) }
        XCTAssertFalse(session.running)
        XCTAssertTrue(session.installed, session.result ?? session.error ?? "no result")
    }

    @MainActor func testGuidanceRendersAtSidebarWidth() throws {
        let content = VStack(spacing: 20) {
            OrinRecoveryDiagram()
            DragonwingSwitchDiagram()
            DragonwingSwitchDiagram(boot: true)
            MediaInstallDiagram(sd: true)
            MediaInstallDiagram(sd: false)
        }.padding(24).frame(width: 660).background(Color(nsColor: .windowBackgroundColor))
        let renderer = ImageRenderer(content: content)
        renderer.scale = 2
        let image = try XCTUnwrap(renderer.nsImage)
        let attachment = XCTAttachment(image: image)
        attachment.name = "Installation guidance"
        attachment.lifetime = .keepAlways
        add(attachment)
        // A local artifact also makes the hardware diagrams easy to inspect.
        let data = try XCTUnwrap(image.tiffRepresentation)
        let bitmap = try XCTUnwrap(NSBitmapImageRep(data: data))
        try bitmap.representation(using: .png, properties: [:])?.write(
            to: URL(fileURLWithPath: NSTemporaryDirectory()).appendingPathComponent(
                "wendy-install-guidance.png"
            )
        )
    }
}
