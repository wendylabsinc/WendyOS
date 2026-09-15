import Crypto
import Darwin
import Foundation
import GRPCCore
import OpenTelemetryGRPC
import Testing
import WendyAgentGRPC

@testable import WendyAgentCore

@Suite("ContainerService.startContainer")
struct ContainerServiceTests {
    @Test("telemetry continues after the start stream disconnects", .timeLimit(.minutes(1)))
    func telemetryContinuesAfterStartStreamDisconnects() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.DetachedTelemetry"
        let firstMarker = "triggers-start-stream-disconnect"
        let marker = "live-after-start-disconnect"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        let releaseLater = appDirectory.appendingPathComponent("release-later.fifo")
        let holdOpen = appDirectory.appendingPathComponent("hold-open.fifo")
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try makeFIFO(at: releaseLater)
        try makeFIFO(at: holdOpen)
        try writeDisconnectOutputScript(
            to: appDirectory.appendingPathComponent("output.sh"),
            firstMarker: firstMarker,
            laterMarker: marker,
            releaseFIFOName: releaseLater.lastPathComponent,
            holdFIFOName: holdOpen.lastPathComponent
        )

        let broadcaster = TelemetryBroadcaster()
        let service = ContainerService(
            broadcaster: broadcaster,
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )
        try await registerFileSyncApp(service: service, appID: appID, cmd: "output.sh")

        var request = Wendy_Agent_Services_V1_StartContainerRequest()
        request.appName = appID
        let response = try await service.startContainer(
            request: ServerRequest(metadata: [:], message: request),
            context: makeServerContext(method: "StartContainer")
        )
        let contents = try response.accepted.get()

        do {
            _ = try await contents.producer(
                RPCWriter(
                    wrapping: DisconnectAfterFirstWriteWriter<
                        Wendy_Agent_Services_V1_RunContainerLayersResponse
                    >()
                )
            )
        } catch {
            // This models watch canceling StartContainer after the Started frame.
        }

        let (logSubscriptionID, _, liveLogs) = await broadcaster.subscribeLogs()
        try signalFIFO(at: releaseLater)
        var logIterator = liveLogs.makeAsyncIterator()
        var receivedLaterMarker = false
        while let logs = await logIterator.next() {
            if logRequest(logs, contains: marker) {
                receivedLaterMarker = true
                break
            }
        }
        await broadcaster.unsubscribeLogs(id: logSubscriptionID)
        #expect(receivedLaterMarker)

        // The gRPC runtime invokes a response producer once. Reinvoking it here
        // is a white-box probe of the captured AsyncStream: after the first
        // producer disconnected, it must already be finished rather than hold
        // the later marker in an unbounded response buffer.
        var replayError: String?
        do {
            _ = try await contents.producer(
                RPCWriter(
                    wrapping: DisconnectAfterFirstWriteWriter<
                        Wendy_Agent_Services_V1_RunContainerLayersResponse
                    >()
                )
            )
        } catch {
            replayError = String(describing: error)
        }

        try await deleteApp(service: service, appID: appID)
        #expect(replayError == nil)
    }

    @Test("deleting an app finishes its start log stream", .timeLimit(.minutes(1)))
    func deletingAppFinishesStartLogStream() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.DeleteClosesLogStream"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        let holdOpen = appDirectory.appendingPathComponent("hold-open.fifo")
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try makeFIFO(at: holdOpen)
        try writeBlockingScript(
            to: appDirectory.appendingPathComponent("block.sh"),
            holdFIFOName: holdOpen.lastPathComponent
        )

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )
        try await registerFileSyncApp(service: service, appID: appID, cmd: "block.sh")

        var request = Wendy_Agent_Services_V1_StartContainerRequest()
        request.appName = appID
        let response = try await service.startContainer(
            request: ServerRequest(metadata: [:], message: request),
            context: makeServerContext(method: "StartContainer")
        )
        let contents = try response.accepted.get()
        let writer = SignalingWriter<Wendy_Agent_Services_V1_RunContainerLayersResponse>()
        var messages = writer.events.makeAsyncIterator()
        let producerTask = Task {
            _ = try await contents.producer(RPCWriter(wrapping: writer))
        }
        defer { producerTask.cancel() }

        let started = await messages.next()
        guard case .started? = started?.responseType else {
            Issue.record("start stream did not send Started")
            try? await deleteApp(service: service, appID: appID)
            producerTask.cancel()
            return
        }

        try await deleteApp(service: service, appID: appID)
        try await producerTask.value
    }

    @Test("app updates are published for create, start, stop, and delete")
    func appUpdatesArePublishedForLifecycleChanges() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.Lifecycle"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writeSleepScript(to: appDirectory.appendingPathComponent("sleep.sh"))

        let recorder = AppSnapshotsRecorder()
        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase),
            onAppsChanged: { apps in
                await recorder.record(apps)
            }
        )

        try await registerFileSyncApp(service: service, appID: appID, cmd: "sleep.sh")
        #expect(
            await recorder.last()
                == .some([
                    WendyAppInfo(id: appID, kind: .native, status: .stopped, pid: nil)
                ])
        )

        try await startApp(service: service, appID: appID)
        let runningSnapshot = try #require(await recorder.last())
        #expect(runningSnapshot.count == 1)
        #expect(runningSnapshot[0].id == appID)
        #expect(runningSnapshot[0].kind == .native)
        #expect(runningSnapshot[0].status == .running)
        #expect(runningSnapshot[0].pid != nil)

        try await stopApp(service: service, appID: appID)
        #expect(
            await recorder.last()
                == .some([
                    WendyAppInfo(id: appID, kind: .native, status: .stopped, pid: nil)
                ])
        )

        try await deleteApp(service: service, appID: appID)
        #expect(await recorder.last() == .some([]))
        #expect(!FileManager.default.fileExists(atPath: appDirectory.path))
    }

    @Test("spontaneous native exits publish a stopped app update")
    func spontaneousNativeExitPublishesStoppedAppUpdate() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.ExitOnOwn"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writeExitAfterDelayScript(to: appDirectory.appendingPathComponent("exit.sh"))

        let recorder = AppSnapshotsRecorder()
        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase),
            onAppsChanged: { apps in
                await recorder.record(apps)
            }
        )

        try await registerFileSyncApp(service: service, appID: appID, cmd: "exit.sh")
        try await startApp(service: service, appID: appID)

        try await waitUntil(description: "app starts running") {
            guard let info = await service.appInfo(forAppID: appID) else { return false }
            return info.status == .running && info.pid != nil
        }

        try await waitUntil(description: "app exits and becomes stopped") {
            await service.appInfo(forAppID: appID)
                == WendyAppInfo(id: appID, kind: .native, status: .stopped, pid: nil)
        }

        let snapshots = await recorder.snapshotValues()
        let publishedRunningSnapshot = snapshots.contains { snapshot in
            snapshot.count == 1
                && snapshot[0].id == appID
                && snapshot[0].kind == .native
                && snapshot[0].status == .running
                && snapshot[0].pid != nil
        }
        #expect(publishedRunningSnapshot)
        #expect(
            snapshots.last == [
                WendyAppInfo(id: appID, kind: .native, status: .stopped, pid: nil)
            ]
        )
    }

    @Test("stale termination callbacks do not overwrite a newer launch")
    func staleTerminationCallbacksDoNotOverwriteANewerLaunch() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.StaleTermination"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writeSleepScript(to: appDirectory.appendingPathComponent("sleep.sh"))

        let recorder = AppSnapshotsRecorder()
        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase),
            onAppsChanged: { apps in
                await recorder.record(apps)
            }
        )

        try await registerFileSyncApp(service: service, appID: appID, cmd: "sleep.sh")
        try await startApp(service: service, appID: appID)

        try await waitUntil(description: "first launch token") {
            await service.launchToken(forAppID: appID) != nil
        }
        let firstLaunchToken = try #require(await service.launchToken(forAppID: appID))

        try await startApp(service: service, appID: appID)

        try await waitUntil(description: "second launch replaces the first launch token") {
            guard let info = await service.appInfo(forAppID: appID),
                let token = await service.launchToken(forAppID: appID)
            else {
                return false
            }
            return info.status == .running && info.pid != nil && token != firstLaunchToken
        }

        let snapshotCountBefore = await recorder.count()
        await service.handleAppTermination(id: appID, launchToken: firstLaunchToken)
        let snapshotCountAfter = await recorder.count()
        let currentInfo = try #require(await service.appInfo(forAppID: appID))

        #expect(snapshotCountAfter == snapshotCountBefore)
        #expect(currentInfo.status == .running)
        #expect(currentInfo.pid != nil)

        try await stopApp(service: service, appID: appID)
    }

    @Test("stopApp is a no-op for missing and stopped apps")
    func stopAppIsANoOpForMissingAndStoppedApps() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.StopNoOp"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writeSleepScript(to: appDirectory.appendingPathComponent("sleep.sh"))

        let recorder = AppSnapshotsRecorder()
        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase),
            onAppsChanged: { apps in
                await recorder.record(apps)
            }
        )

        await service.stopApp(id: "missing")
        #expect(await recorder.count() == 0)

        try await registerFileSyncApp(service: service, appID: appID, cmd: "sleep.sh")
        let snapshotCountBefore = await recorder.count()
        await service.stopApp(id: appID)

        #expect(await recorder.count() == snapshotCountBefore)
        #expect(
            await service.appInfo(forAppID: appID)
                == WendyAppInfo(
                    id: appID,
                    kind: .native,
                    status: .stopped,
                    pid: nil
                )
        )
    }

    @Test("stopAllApps stops running apps and keeps them known")
    func stopAllAppsStopsRunningAppsAndKeepsThemKnown() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appIDs = ["sh.wendy.tests.StopAllA", "sh.wendy.tests.StopAllB"]
        let recorder = AppSnapshotsRecorder()
        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase),
            onAppsChanged: { apps in
                await recorder.record(apps)
            }
        )

        for appID in appIDs {
            let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
            try FileManager.default.createDirectory(
                at: appDirectory,
                withIntermediateDirectories: true
            )
            try writeSleepScript(to: appDirectory.appendingPathComponent("sleep.sh"))
            try await registerFileSyncApp(service: service, appID: appID, cmd: "sleep.sh")
            try await startApp(service: service, appID: appID)
        }

        try await waitUntil(description: "apps are running") {
            let infos = await service.currentAppInfosForTesting()
            return infos.count == 2 && infos.allSatisfy { $0.status == .running && $0.pid != nil }
        }

        await service.stopAllApps()

        let stoppedInfos = await service.currentAppInfosForTesting()
        #expect(stoppedInfos.count == 2)
        #expect(stoppedInfos.map(\.id) == appIDs)
        #expect(stoppedInfos.allSatisfy { $0.status == .stopped && $0.pid == nil })
        #expect((await recorder.last()) == stoppedInfos)
    }

    @Test("listContainers returns all known apps with their current status")
    func listContainersReturnsAllKnownAppsWithTheirCurrentStatus() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let runningAppID = "sh.wendy.tests.ListRunning"
        let stoppedAppID = "sh.wendy.tests.ListStopped"

        for appID in [runningAppID, stoppedAppID] {
            let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
            try FileManager.default.createDirectory(
                at: appDirectory,
                withIntermediateDirectories: true
            )
            try writeSleepScript(to: appDirectory.appendingPathComponent("sleep.sh"))
        }

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        try await registerFileSyncApp(service: service, appID: runningAppID, cmd: "sleep.sh")
        try await registerFileSyncApp(service: service, appID: stoppedAppID, cmd: "sleep.sh")
        try await startApp(service: service, appID: runningAppID)

        let containers = try await listContainers(service: service)
        #expect(containers.map(\.appName) == [runningAppID, stoppedAppID])
        #expect(containers[0].runningState == .running)
        #expect(containers[1].runningState == .stopped)

        await service.stopAllApps()
    }

    @Test("persistence stores runtime state and restores apps as stopped")
    func persistenceStoresRuntimeStateAndRestoresAppsAsStopped() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.Persistence"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writeSleepScript(to: appDirectory.appendingPathComponent("sleep.sh"))

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        try await registerFileSyncApp(service: service, appID: appID, cmd: "sleep.sh")
        try await startApp(service: service, appID: appID)

        try await waitUntil(description: "persisted app is running") {
            guard let info = await service.appInfo(forAppID: appID) else { return false }
            return info.status == .running && info.pid != nil
        }

        let persistedApps = try readPersistedApps(at: await service.infoFileURLForTesting())
        let persistedApp = try #require(persistedApps.first { $0.info.id == appID })
        #expect(persistedApp.info.status == .running)
        #expect(persistedApp.info.pid != nil)
        #expect(
            persistedApp.native
                == WendyApp.NativeMetadata(
                    directory: appDirectory.path,
                    binaryName: "sleep.sh",
                    args: [],
                    currentDirectory: appDirectory.path,
                    executablePath: appDirectory.appendingPathComponent("sleep.sh").path,
                    environment: [:]
                )
        )

        let restoredService = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        #expect(
            await restoredService.appInfo(forAppID: appID)
                == WendyAppInfo(
                    id: appID,
                    kind: .native,
                    status: .stopped,
                    pid: nil
                )
        )

        try await startApp(service: restoredService, appID: appID)
        try await waitUntil(description: "restored app runs again") {
            guard let info = await restoredService.appInfo(forAppID: appID) else { return false }
            return info.status == .running && info.pid != nil
        }
        await restoredService.stopApp(id: appID)
    }

    @Test("corrupt persisted app state is ignored on startup")
    func corruptPersistedAppStateIsIgnoredOnStartup() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let infoFileURL = URL(fileURLWithPath: appsBase).appendingPathComponent("info.json")
        try "not valid json".write(to: infoFileURL, atomically: true, encoding: .utf8)

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        #expect(await service.currentAppInfosForTesting().isEmpty)
    }

    @Test("delete removes orphaned native app directories")
    func deleteRemovesOrphanedNativeAppDirectories() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.OrphanedAppDirectory"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try "orphaned".write(
            to: appDirectory.appendingPathComponent("payload.txt"),
            atomically: true,
            encoding: .utf8
        )

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        try await deleteApp(service: service, appID: appID)

        #expect(!FileManager.default.fileExists(atPath: appDirectory.path))
        #expect(await service.currentAppInfosForTesting().isEmpty)
    }

    @Test("delete of a running app publishes stopped before removal")
    func deleteOfARunningAppPublishesStoppedBeforeRemoval() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.DeleteRunning"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writeSleepScript(to: appDirectory.appendingPathComponent("sleep.sh"))

        let recorder = AppSnapshotsRecorder()
        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase),
            onAppsChanged: { apps in
                await recorder.record(apps)
            }
        )

        try await registerFileSyncApp(service: service, appID: appID, cmd: "sleep.sh")
        try await startApp(service: service, appID: appID)
        try await deleteApp(service: service, appID: appID)

        let snapshots = await recorder.snapshotValues()
        let stoppedIndex = snapshots.firstIndex(of: [
            WendyAppInfo(id: appID, kind: .native, status: .stopped, pid: nil)
        ])
        let removedIndex = snapshots.firstIndex(of: [])

        #expect(stoppedIndex != nil)
        #expect(removedIndex != nil)
        #expect(stoppedIndex! < removedIndex!)
    }

    @Test("beginStopping rejects create and start mutations")
    func beginStoppingRejectsCreateAndStartMutations() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.StoppingGate"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writeSleepScript(to: appDirectory.appendingPathComponent("sleep.sh"))

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        await service.beginStopping()

        do {
            try await registerFileSyncApp(service: service, appID: appID, cmd: "sleep.sh")
            Issue.record("Expected createContainer to be rejected while stopping")
        } catch let error as RPCError {
            #expect(error.code == .failedPrecondition)
        }

        do {
            try await startApp(service: service, appID: appID)
            Issue.record("Expected startContainer to be rejected while stopping")
        } catch let error as RPCError {
            #expect(error.code == .failedPrecondition)
        }
    }

    @Test("Linux container create requests fail precondition without a configured runtime")
    func createContainerRejectsLinuxContainers() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.LinuxContainerCreate"
        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        var request = Wendy_Agent_Services_V1_CreateContainerRequest()
        request.appName = appID
        request.imageName = "localhost:5000/sh.wendy.tests.linuxcontainercreate:latest"
        request.appConfig = try JSONEncoder().encode(
            WendyAppConfig(appId: appID, platform: "linux/arm64", entitlements: nil)
        )

        do {
            _ = try await service.createContainer(
                request: ServerRequest(metadata: [:], message: request),
                context: makeServerContext(method: "CreateContainer")
            )
            Issue.record("Expected createContainer to reject Linux containers on Macs")
        } catch let error as RPCError {
            #expect(error.code == .failedPrecondition)
            #expect("\(error)".contains("No Linux container runtime found"))
        }
    }

    @Test("create requests without a platform default to Linux")
    func createContainerRejectsMissingPlatformAsLinux() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.MissingPlatformCreate"
        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        var request = Wendy_Agent_Services_V1_CreateContainerRequest()
        request.appName = appID
        request.imageName = "localhost:5000/sh.wendy.tests.missingplatformcreate:latest"
        request.appConfig = try JSONEncoder().encode(
            WendyAppConfig(appId: appID, platform: nil, entitlements: nil)
        )

        do {
            _ = try await service.createContainer(
                request: ServerRequest(metadata: [:], message: request),
                context: makeServerContext(method: "CreateContainer")
            )
            Issue.record(
                "Expected createContainer to reject missing-platform apps as Linux containers"
            )
        } catch let error as RPCError {
            #expect(error.code == .failedPrecondition)
            #expect("\(error)".contains("No Linux container runtime found"))
        }
    }

    @Test("WendyOS platform create requests are treated as Linux")
    func createContainerRejectsWendyOSPlatformAsLinux() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.WendyOSPlatformCreate"
        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        var request = Wendy_Agent_Services_V1_CreateContainerRequest()
        request.appName = appID
        request.imageName = "localhost:5000/sh.wendy.tests.wendyosplatformcreate:latest"
        request.appConfig = try JSONEncoder().encode(
            WendyAppConfig(appId: appID, platform: "wendyos", entitlements: nil)
        )

        do {
            _ = try await service.createContainer(
                request: ServerRequest(metadata: [:], message: request),
                context: makeServerContext(method: "CreateContainer")
            )
            Issue.record("Expected createContainer to reject wendyos apps as Linux containers")
        } catch let error as RPCError {
            #expect(error.code == .failedPrecondition)
            #expect("\(error)".contains("No Linux container runtime found"))
        }
    }

    @Test("persisted Linux container apps fail gracefully on start")
    func startContainerRejectsPersistedLinuxContainers() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.LinuxContainerStart"
        let persistedApps = [
            WendyApp(
                info: WendyAppInfo(id: appID, kind: .container, status: .stopped, pid: nil),
                native: nil,
                container: WendyApp.ContainerMetadata(
                    imageName: "localhost:5000/sh.wendy.tests.linuxcontainerstart:latest",
                    appConfig: WendyAppConfig(
                        appId: appID,
                        platform: "linux/arm64",
                        entitlements: nil
                    )
                ),
                process: nil,
                launchToken: nil
            )
        ]
        let infoFileURL = URL(fileURLWithPath: appsBase).appendingPathComponent("info.json")
        try JSONEncoder().encode(persistedApps).write(to: infoFileURL)

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        do {
            try await startApp(service: service, appID: appID)
            Issue.record("Expected startContainer to reject persisted Linux containers on Macs")
        } catch let error as RPCError {
            #expect(error.code == .failedPrecondition)
            #expect("\(error)".contains("No Linux container runtime found"))
        }
    }

    @Test("file-sync native launch uses synced app directory as current working directory")
    func fileSyncLaunchUsesSyncedAppDirectoryAsCurrentWorkingDirectory() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.PrintPWD"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)

        try writePrintPWDScript(to: appDirectory.appendingPathComponent("printpwd.sh"))

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        try await registerFileSyncApp(service: service, appID: appID, cmd: "printpwd.sh")
        let stdout = try await startAppAndCollectStdout(service: service, appID: appID)
        let expectedPath = try canonicalPath(appDirectory.path)

        #expect(stdout == expectedPath)
    }

    @Test(
        "sandboxed file-sync native launch uses synced app directory as current working directory"
    )
    func sandboxedFileSyncLaunchUsesSyncedAppDirectoryAsCurrentWorkingDirectory() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.PrintPWDSandboxed"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)

        try writePrintPWDScript(to: appDirectory.appendingPathComponent("printpwd.sh"))
        try writeSandboxProfile(to: appDirectory.appendingPathComponent("sandbox.sb"))

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        try await registerFileSyncApp(service: service, appID: appID, cmd: "printpwd.sh")
        let stdout = try await startAppAndCollectStdout(service: service, appID: appID)
        let expectedPath = try canonicalPath(appDirectory.path)

        #expect(stdout == expectedPath)
    }

    @Test("listContainerStats reports registered apps with zeroed stats")
    func listContainerStatsReportsRegisteredApps() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.Stats"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writePrintPWDScript(to: appDirectory.appendingPathComponent("printpwd.sh"))

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        try await registerFileSyncApp(service: service, appID: appID, cmd: "printpwd.sh")
        let stats = try await listContainerStats(service: service)

        #expect(stats.count == 1)
        #expect(stats.first?.appName == appID)
        #expect(stats.first?.memoryBytes == 0)
        #expect(stats.first?.storageBytes == 0)
    }

    @Test("Brewfile command uses brew bundle with explicit file")
    func brewfileCommandUsesBrewBundleWithExplicitFile() {
        let args = ContainerService.brewBundleArguments(brewfilePath: "/tmp/app/Brewfile")
        #expect(args == ["bundle", "--file", "/tmp/app/Brewfile"])
    }

    @Test("Homebrew lookup uses default install locations only")
    func homebrewLookupUsesDefaultInstallLocationsOnly() {
        let found = ContainerService.findBrewExecutable(
            fileExists: { $0 == "/tmp/fake/brew" || $0 == "/opt/homebrew/bin/brew" }
        )
        #expect(found == "/opt/homebrew/bin/brew")
    }

    @Test("Brewfile failure messages include exit status but not process output")
    func brewfileFailureMessagesIncludeExitStatusButNotProcessOutput() {
        let message = ContainerService.brewBundleFailureMessage(status: 17)
        #expect(!message.contains("ops/Brewfile"))
        #expect(message.contains("exit code 17"))
        #expect(message.contains("wendy device logs"))
        #expect(!message.contains("wendy-e2e-missing-formula"))
        #expect(!message.contains("No available formula"))
        #expect(!message.contains("ghp_secret"))
    }

    @Test("Brewfile command environment omits credentials")
    func brewfileCommandEnvironmentOmitsCredentials() {
        let environment = ContainerService.brewBundleEnvironment(
            source: [
                "HOME": "/Users/wendy",
                "PATH": "/opt/homebrew/bin:/usr/bin:/bin",
                "TMPDIR": "/tmp",
                "USER": "wendy",
                "AWS_SECRET_ACCESS_KEY": "secret",
                "GITHUB_TOKEN": "token",
                "DATABASE_PASSWORD": "secret",
            ],
            realUserName: "wendy"
        )

        #expect(environment["HOME"] == "/Users/wendy")
        #expect(
            environment["PATH"] == "/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin"
        )
        #expect(environment["TMPDIR"] == "/tmp")
        #expect(environment["USER"] == "wendy")
        #expect(environment["HOMEBREW_NO_ANALYTICS"] == "1")
        #expect(environment["HOMEBREW_NO_AUTO_UPDATE"] == nil)
        #expect(environment["AWS_SECRET_ACCESS_KEY"] == nil)
        #expect(environment["GITHUB_TOKEN"] == nil)
        #expect(environment["DATABASE_PASSWORD"] == nil)
    }

    @Test("Brewfile command environment replaces synthetic USER/LOGNAME with the real user")
    func brewfileCommandEnvironmentReplacesSyntheticUserWithRealUser() {
        let environment = ContainerService.brewBundleEnvironment(
            source: [
                "HOME": "/tmp/wendy-e2e/home",
                "USER": "wendy-e2e-agent",
                "LOGNAME": "wendy-e2e-agent",
            ],
            realUserName: "runner"
        )

        #expect(environment["USER"] == "runner")
        #expect(environment["LOGNAME"] == "runner")
        #expect(environment["HOME"] == "/tmp/wendy-e2e/home")
    }

    @Test("Brewfile command environment falls back to source USER when real user is unknown")
    func brewfileCommandEnvironmentFallsBackToSourceUserWhenRealUserIsUnknown() {
        let environment = ContainerService.brewBundleEnvironment(
            source: [
                "HOME": "/Users/wendy",
                "USER": "wendy",
                "LOGNAME": "wendy",
            ],
            realUserName: nil
        )

        #expect(environment["USER"] == "wendy")
        #expect(environment["LOGNAME"] == "wendy")
    }

    @Test("Native app environment routes OTLP to the agent and stamps app identity")
    func nativeAppEnvironmentRoutesOTLPToAgent() {
        let environment = ContainerService.nativeAppEnvironment(
            appName: "camera",
            otelPort: 54321,
            source: [
                "PATH": "/usr/bin:/bin",
                "OTEL_EXPORTER_OTLP_ENDPOINT": "https://inherited.invalid:4317",
                "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
                "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "https://inherited.invalid/v1/logs",
                "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "http/protobuf",
                "OTEL_SERVICE_NAME": "inherited-agent-name",
                "OTEL_RESOURCE_ATTRIBUTES": "deployment.environment.name=test",
            ]
        )

        #expect(environment["PATH"] == "/usr/bin:/bin")
        #expect(environment["NSUnbufferedIO"] == "YES")
        #expect(environment["OTEL_EXPORTER_OTLP_ENDPOINT"] == "http://127.0.0.1:54321")
        #expect(environment["OTEL_EXPORTER_OTLP_PROTOCOL"] == "grpc")
        #expect(environment["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"] == nil)
        #expect(environment["OTEL_EXPORTER_OTLP_LOGS_PROTOCOL"] == nil)
        #expect(environment["OTEL_SERVICE_NAME"] == "camera")
        #expect(
            environment["OTEL_RESOURCE_ATTRIBUTES"]
                == "deployment.environment.name=test,wendy.app.name=camera"
        )
    }

    @Test("Native app environment corrects an inherited Wendy app resource attribute")
    func nativeAppEnvironmentCorrectsInheritedAppAttribute() {
        let environment = ContainerService.nativeAppEnvironment(
            appName: "camera",
            otelPort: 4317,
            source: ["OTEL_RESOURCE_ATTRIBUTES": "wendy.app.name=custom,region=au"]
        )

        #expect(environment["OTEL_RESOURCE_ATTRIBUTES"] == "wendy.app.name=camera,region=au")
    }

    @Test("Adapted native process output uses the canonical container log scope")
    func adaptedNativeOutputUsesContainerScope() throws {
        let request = ContainerService.containerLogRequest(
            appName: "camera",
            text: "hello",
            stream: "stderr",
            severity: .warn,
            timestamp: 123
        )
        let resourceLogs = try #require(request.resourceLogs.first)
        let scopeLogs = try #require(resourceLogs.scopeLogs.first)
        let record = try #require(scopeLogs.logRecords.first)

        #expect(scopeLogs.scope.name == "wendy.container")
        #expect(record.body.stringValue == "hello")
        #expect(record.severityNumber == .warn)
        #expect(record.timeUnixNano == 123)
        #expect(
            record.attributes.contains { attribute in
                attribute.key == "stream" && attribute.value.stringValue == "stderr"
            }
        )
        #expect(
            resourceLogs.resource.attributes.contains { attribute in
                attribute.key == "service.name" && attribute.value.stringValue == "camera"
            }
        )
        #expect(
            resourceLogs.resource.attributes.contains { attribute in
                attribute.key == "wendy.app.name" && attribute.value.stringValue == "camera"
            }
        )
    }

    @Test("Real user name resolves to an existing account")
    func realUserNameResolvesToAnExistingAccount() {
        let name = ContainerService.realUserName()
        #expect(name?.isEmpty == false)
    }

    @Test("Brewfile symlink escapes are rejected before launching Homebrew")
    func brewfileSymlinkEscapesAreRejectedBeforeLaunchingHomebrew() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.SymlinkBrewfile"
        let baseURL = URL(fileURLWithPath: appsBase)
        let appDirectory = baseURL.appendingPathComponent(appID)
        let outsideDirectory = baseURL.appendingPathComponent("outside")
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(
            at: outsideDirectory,
            withIntermediateDirectories: true
        )
        try writePrintPWDScript(to: appDirectory.appendingPathComponent("printpwd.sh"))
        try "brew \"hello\"\n".write(
            to: outsideDirectory.appendingPathComponent("Brewfile"),
            atomically: true,
            encoding: .utf8
        )
        try FileManager.default.createSymbolicLink(
            at: appDirectory.appendingPathComponent("ops"),
            withDestinationURL: outsideDirectory
        )

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: baseURL
        )

        var request = Wendy_Agent_Services_V1_CreateContainerRequest()
        request.appName = appID
        request.cmd = "printpwd.sh"
        request.appConfig = try JSONEncoder().encode(
            WendyAppConfig(
                appId: appID,
                platform: "darwin",
                entitlements: nil,
                brewfile: "ops/Brewfile"
            )
        )

        do {
            _ = try await service.createContainer(
                request: ServerRequest(metadata: [:], message: request),
                context: makeServerContext(method: "CreateContainer")
            )
            Issue.record("Expected createContainer to reject symlink Brewfile escape")
        } catch let error as RPCError {
            #expect(error.code == .invalidArgument)
            #expect("\(error)".contains("brewfile path must stay within the app directory"))
        }
    }

    @Test("invalid Brewfile paths are rejected before launching Homebrew")
    func invalidBrewfilePathsAreRejectedBeforeLaunchingHomebrew() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.BadBrewfile"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writePrintPWDScript(to: appDirectory.appendingPathComponent("printpwd.sh"))

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        var request = Wendy_Agent_Services_V1_CreateContainerRequest()
        request.appName = appID
        request.cmd = "printpwd.sh"
        request.appConfig = try JSONEncoder().encode(
            WendyAppConfig(
                appId: appID,
                platform: "darwin",
                entitlements: nil,
                brewfile: "../Brewfile"
            )
        )

        do {
            _ = try await service.createContainer(
                request: ServerRequest(metadata: [:], message: request),
                context: makeServerContext(method: "CreateContainer")
            )
            Issue.record("Expected createContainer to reject unsafe Brewfile path")
        } catch let error as RPCError {
            #expect(error.code == .invalidArgument)
            #expect("\(error)".contains("brewfile path must be relative"))
        }
    }

    @Test("WriteLayer legacy rejects app name path traversal")
    func writeLayerRejectsAppNameTraversal() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        // A sibling directory of appsBase, inside the writable temp root, so a
        // permission error (rather than the path-validation guard) can't mask
        // whether the traversal actually escaped appsBase.
        let escapeTarget = URL(fileURLWithPath: appsBase)
            .deletingLastPathComponent()
            .appendingPathComponent("wendy-evil-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: escapeTarget) }

        let payload = Data("x".utf8)
        let hash = SHA256.hash(data: payload).map { String(format: "%02x", $0) }.joined()
        // Legacy digest: "<appName>:<filename>:sha256:<hash>". appName escapes appsBase.
        let digest = "../\(escapeTarget.lastPathComponent):pwned.sh:sha256:\(hash)"

        await #expect(throws: (any Error).self) {
            try await driveWriteLayer(service: service, digest: digest, chunks: [payload])
        }
        // Nothing was written outside appsBase.
        #expect(
            !FileManager.default.fileExists(
                atPath: escapeTarget.appendingPathComponent("pwned.sh").path
            )
        )
    }

    @Test("createContainer rejects OCI manifest digest path traversal (readBlob)")
    func createContainerRejectsManifestDigestTraversal() async throws {
        let stateDir = try makeTempDir()
        defer { cleanup(stateDir) }
        let stateURL = URL(fileURLWithPath: stateDir)

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            stateDirectory: stateURL
        )

        // Plant a file outside blobsDirectory (a sibling of stateDir/blobs, inside
        // the writable temp state dir) that the traversal digest would resolve to
        // if the path derivation were unguarded.
        let escapeTarget = stateURL.appendingPathComponent("evil-manifest")
        try "not-a-manifest".write(to: escapeTarget, atomically: true, encoding: .utf8)

        let appID = "sh.wendy.tests.ManifestTraversal"
        var request = Wendy_Agent_Services_V1_CreateContainerRequest()
        request.appName = appID
        request.imageName = "sha256:../../evil-manifest"

        await #expect(throws: PathValidationError.self) {
            _ = try await service.createContainer(
                request: ServerRequest(metadata: [:], message: request),
                context: makeServerContext(method: "CreateContainer")
            )
        }
    }

    @Test("createContainer rejects OCI layer digest path traversal (extractTarGz)")
    func createContainerRejectsLayerDigestTraversal() async throws {
        let stateDir = try makeTempDir()
        defer { cleanup(stateDir) }
        let stateURL = URL(fileURLWithPath: stateDir)
        let blobsShaDir = stateURL.appendingPathComponent("blobs").appendingPathComponent("sha256")

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            stateDirectory: stateURL
        )

        // Legitimate config blob, stored inside blobsDirectory (no traversal).
        let config = OCIImageConfig(
            architecture: "arm64",
            os: "darwin",
            config: OCIContainerConfig(
                Entrypoint: ["./marker.sh"],
                Cmd: nil,
                WorkingDir: nil,
                Env: nil,
                ExposedPorts: nil
            ),
            rootfs: nil
        )
        let configData = try JSONEncoder().encode(config)
        let configHash = SHA256.hash(data: configData).map { String(format: "%02x", $0) }.joined()
        try configData.write(to: blobsShaDir.appendingPathComponent(configHash))

        // Malicious tar.gz planted OUTSIDE blobsDirectory (a sibling of it, inside
        // the writable temp state dir), containing the marker binary.
        let evilLayerURL = stateURL.appendingPathComponent("evil-layer")
        try makeTarGz(containing: [("marker.sh", "#!/bin/sh\necho pwned\n")], at: evilLayerURL)

        // Legitimate manifest, stored inside blobsDirectory, referencing the config
        // normally but pointing its layer digest OUTSIDE blobsDirectory via traversal.
        let manifest = OCIManifest(
            schemaVersion: 2,
            config: OCIDescriptor(
                mediaType: "application/vnd.oci.image.config.v1+json",
                digest: "sha256:\(configHash)",
                size: Int64(configData.count)
            ),
            layers: [
                OCIDescriptor(
                    mediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
                    digest: "sha256:../../evil-layer",
                    size: 1
                )
            ]
        )
        let manifestData = try JSONEncoder().encode(manifest)
        let manifestHash = SHA256.hash(data: manifestData).map { String(format: "%02x", $0) }
            .joined()
        try manifestData.write(to: blobsShaDir.appendingPathComponent(manifestHash))

        let appID = "sh.wendy.tests.LayerTraversal"
        var request = Wendy_Agent_Services_V1_CreateContainerRequest()
        request.appName = appID
        request.imageName = "sha256:\(manifestHash)"

        await #expect(throws: PathValidationError.self) {
            _ = try await service.createContainer(
                request: ServerRequest(metadata: [:], message: request),
                context: makeServerContext(method: "CreateContainer")
            )
        }

        // The malicious layer must never be extracted into the app directory.
        let appDirectory = stateURL.appendingPathComponent("apps").appendingPathComponent(appID)
        #expect(
            !FileManager.default.fileExists(
                atPath: appDirectory.appendingPathComponent("marker.sh").path
            )
        )
    }

    // MARK: - Restart policy & durable user-stop

    @Test("createContainer records the requested restart policy")
    func createContainerRecordsRequestedRestartPolicy() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.CreateRestartPolicy"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writeSleepScript(to: appDirectory.appendingPathComponent("sleep.sh"))

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        var request = Wendy_Agent_Services_V1_CreateContainerRequest()
        request.appName = appID
        request.imageName = ""
        request.cmd = "sleep.sh"
        var policy = RestartPolicy()
        policy.mode = .onFailure
        policy.onFailureMaxRetries = 4
        request.restartPolicy = policy

        _ = try await service.createContainer(
            request: ServerRequest(metadata: [:], message: request),
            context: makeServerContext(method: "CreateContainer")
        )

        let persistedApps = try readPersistedApps(at: await service.infoFileURLForTesting())
        let persistedApp = try #require(persistedApps.first { $0.info.id == appID })
        #expect(
            persistedApp.restartPolicy
                == PersistedRestartPolicy(mode: .onFailure, onFailureMaxRetries: 4)
        )
    }

    @Test("startContainer without a restart policy leaves a previously stored policy intact")
    func startContainerWithoutPolicyLeavesStoredPolicyIntact() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.StartKeepsPolicy"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writeSleepScript(to: appDirectory.appendingPathComponent("sleep.sh"))

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        var createRequest = Wendy_Agent_Services_V1_CreateContainerRequest()
        createRequest.appName = appID
        createRequest.imageName = ""
        createRequest.cmd = "sleep.sh"
        var storedPolicy = RestartPolicy()
        storedPolicy.mode = .onFailure
        storedPolicy.onFailureMaxRetries = 9
        createRequest.restartPolicy = storedPolicy
        _ = try await service.createContainer(
            request: ServerRequest(metadata: [:], message: createRequest),
            context: makeServerContext(method: "CreateContainer")
        )

        // No restartPolicy set on this StartContainerRequest — the stored
        // policy from create must survive untouched.
        try await startApp(service: service, appID: appID)

        let persistedApps = try readPersistedApps(at: await service.infoFileURLForTesting())
        let persistedApp = try #require(persistedApps.first { $0.info.id == appID })
        #expect(
            persistedApp.restartPolicy
                == PersistedRestartPolicy(mode: .onFailure, onFailureMaxRetries: 9)
        )

        await service.stopApp(id: appID)
    }

    @Test("startContainer with a restart policy overwrites the stored one")
    func startContainerWithPolicyOverwritesStoredPolicy() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.StartOverwritesPolicy"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writeSleepScript(to: appDirectory.appendingPathComponent("sleep.sh"))

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        try await registerFileSyncApp(service: service, appID: appID, cmd: "sleep.sh")

        var startRequest = Wendy_Agent_Services_V1_StartContainerRequest()
        startRequest.appName = appID
        var policy = RestartPolicy()
        policy.mode = .no
        startRequest.restartPolicy = policy

        _ = try await service.startContainer(
            request: ServerRequest(metadata: [:], message: startRequest),
            context: makeServerContext(method: "StartContainer")
        )

        let persistedApps = try readPersistedApps(at: await service.infoFileURLForTesting())
        let persistedApp = try #require(persistedApps.first { $0.info.id == appID })
        #expect(
            persistedApp.restartPolicy == PersistedRestartPolicy(mode: .no, onFailureMaxRetries: 0)
        )

        await service.stopApp(id: appID)
    }

    @Test("stopContainer marks stoppedByUser and it survives a save/load cycle")
    func stopContainerMarksStoppedByUserAndPersists() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.StopMarksUser"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writeSleepScript(to: appDirectory.appendingPathComponent("sleep.sh"))

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        try await registerFileSyncApp(service: service, appID: appID, cmd: "sleep.sh")
        try await startApp(service: service, appID: appID)
        try await stopApp(service: service, appID: appID)

        let persistedApps = try readPersistedApps(at: await service.infoFileURLForTesting())
        let persistedApp = try #require(persistedApps.first { $0.info.id == appID })
        #expect(persistedApp.stoppedByUser == true)

        // Reload from disk (a fresh service instance re-reads info.json on
        // init) to prove the flag survives a save/load cycle, not just the
        // in-memory struct.
        let restoredService = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )
        let restoredApps = try readPersistedApps(at: await restoredService.infoFileURLForTesting())
        let restoredApp = try #require(restoredApps.first { $0.info.id == appID })
        #expect(restoredApp.stoppedByUser == true)
    }

    @Test("stopAllApps leaves stoppedByUser false")
    func stopAllAppsLeavesStoppedByUserFalse() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.ShutdownNotUser"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writeSleepScript(to: appDirectory.appendingPathComponent("sleep.sh"))

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        try await registerFileSyncApp(service: service, appID: appID, cmd: "sleep.sh")
        try await startApp(service: service, appID: appID)

        try await waitUntil(description: "app is running before shutdown") {
            await service.appInfo(forAppID: appID)?.status == .running
        }

        await service.stopAllApps()

        let persistedApps = try readPersistedApps(at: await service.infoFileURLForTesting())
        let persistedApp = try #require(persistedApps.first { $0.info.id == appID })
        #expect(persistedApp.stoppedByUser == false)
    }

    @Test("startContainer clears stoppedByUser")
    func startContainerClearsStoppedByUser() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.StartClearsUserStop"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writeSleepScript(to: appDirectory.appendingPathComponent("sleep.sh"))

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        try await registerFileSyncApp(service: service, appID: appID, cmd: "sleep.sh")
        try await startApp(service: service, appID: appID)
        try await stopApp(service: service, appID: appID)

        let stoppedApps = try readPersistedApps(at: await service.infoFileURLForTesting())
        #expect(try #require(stoppedApps.first { $0.info.id == appID }).stoppedByUser == true)

        try await startApp(service: service, appID: appID)

        let restartedApps = try readPersistedApps(at: await service.infoFileURLForTesting())
        #expect(try #require(restartedApps.first { $0.info.id == appID }).stoppedByUser == false)

        await service.stopApp(id: appID)
    }

    @Test("termination records a clean exit code")
    func terminationRecordsCleanExitCode() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.CleanExitCode"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writeExitAfterDelayScript(to: appDirectory.appendingPathComponent("exit.sh"))

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        try await registerFileSyncApp(service: service, appID: appID, cmd: "exit.sh")
        try await startApp(service: service, appID: appID)

        try await waitUntil(description: "app exits on its own") {
            await service.appInfo(forAppID: appID)?.status == .stopped
        }

        #expect(await service.lastExitCode(forAppID: appID) == 0)
    }

    @Test("termination records a non-zero exit code")
    func terminationRecordsCrashExitCode() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.CrashExitCode"
        let appDirectory = URL(fileURLWithPath: appsBase).appendingPathComponent(appID)
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try writeExitWithCodeScript(to: appDirectory.appendingPathComponent("crash.sh"), code: 7)

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase)
        )

        try await registerFileSyncApp(service: service, appID: appID, cmd: "crash.sh")
        try await startApp(service: service, appID: appID)

        try await waitUntil(description: "app crashes on its own") {
            await service.appInfo(forAppID: appID)?.status == .stopped
        }

        #expect(await service.lastExitCode(forAppID: appID) == 7)
    }
}

// MARK: - Helpers

private actor AppSnapshotsRecorder {
    private var storedSnapshots: [[WendyAppInfo]] = []

    func record(_ apps: [WendyAppInfo]) {
        self.storedSnapshots.append(apps)
    }

    func last() -> [WendyAppInfo]? {
        self.storedSnapshots.last
    }

    func count() -> Int {
        self.storedSnapshots.count
    }

    func snapshotValues() -> [[WendyAppInfo]] {
        self.storedSnapshots
    }
}

private final class CollectingWriter<Element: Sendable>: RPCWriterProtocol, @unchecked Sendable {
    private let queue = DispatchQueue(label: "wendy.tests.collecting-writer")
    private var elements: [Element] = []

    func write(_ element: Element) async throws {
        queue.sync {
            elements.append(element)
        }
    }

    func write(contentsOf elements: some Sequence<Element>) async throws {
        queue.sync {
            self.elements.append(contentsOf: elements)
        }
    }

    func snapshot() -> [Element] {
        queue.sync {
            elements
        }
    }
}

private final class SignalingWriter<Element: Sendable>: RPCWriterProtocol, @unchecked Sendable {
    let events: AsyncStream<Element>
    private let continuation: AsyncStream<Element>.Continuation

    init() {
        (self.events, self.continuation) = AsyncStream.makeStream(
            of: Element.self,
            bufferingPolicy: .bufferingNewest(16)
        )
    }

    func write(_ element: Element) async throws {
        self.continuation.yield(element)
    }

    func write(contentsOf elements: some Sequence<Element>) async throws {
        for element in elements {
            self.continuation.yield(element)
        }
    }
}

private final class DisconnectAfterFirstWriteWriter<Element: Sendable>: RPCWriterProtocol,
    @unchecked Sendable
{
    private let queue = DispatchQueue(label: "wendy.tests.disconnecting-writer")
    private var writes = 0

    func write(_ element: Element) async throws {
        let shouldDisconnect = queue.sync {
            writes += 1
            return writes > 1
        }
        if shouldDisconnect {
            throw TestError(description: "simulated client disconnect after Started")
        }
    }

    func write(contentsOf elements: some Sequence<Element>) async throws {
        for element in elements {
            try await self.write(element)
        }
    }
}

private struct TestError: Error, CustomStringConvertible {
    let description: String
}

private func registerFileSyncApp(
    service: ContainerService,
    appID: String,
    cmd: String
) async throws {
    var request = Wendy_Agent_Services_V1_CreateContainerRequest()
    request.appName = appID
    request.imageName = ""
    request.cmd = cmd

    _ = try await service.createContainer(
        request: ServerRequest(metadata: [:], message: request),
        context: makeServerContext(method: "CreateContainer")
    )
}

private func startApp(
    service: ContainerService,
    appID: String
) async throws {
    var request = Wendy_Agent_Services_V1_StartContainerRequest()
    request.appName = appID

    _ = try await service.startContainer(
        request: ServerRequest(metadata: [:], message: request),
        context: makeServerContext(method: "StartContainer")
    )
}

private func stopApp(
    service: ContainerService,
    appID: String
) async throws {
    var request = Wendy_Agent_Services_V1_StopContainerRequest()
    request.appName = appID

    _ = try await service.stopContainer(
        request: ServerRequest(metadata: [:], message: request),
        context: makeServerContext(method: "StopContainer")
    )
}

private func deleteApp(
    service: ContainerService,
    appID: String
) async throws {
    var request = Wendy_Agent_Services_V1_DeleteContainerRequest()
    request.appName = appID

    _ = try await service.deleteContainer(
        request: ServerRequest(metadata: [:], message: request),
        context: makeServerContext(method: "DeleteContainer")
    )
}

private func startAppAndCollectStdout(
    service: ContainerService,
    appID: String
) async throws -> String {
    var request = Wendy_Agent_Services_V1_StartContainerRequest()
    request.appName = appID

    let response = try await service.startContainer(
        request: ServerRequest(metadata: [:], message: request),
        context: makeServerContext(method: "StartContainer")
    )

    let contents = try response.accepted.get()
    let writer = CollectingWriter<Wendy_Agent_Services_V1_RunContainerLayersResponse>()
    _ = try await contents.producer(RPCWriter(wrapping: writer))

    let messages = writer.snapshot()
    let stdout = messages.reduce(into: Data()) { data, message in
        guard case .stdoutOutput(let output)? = message.responseType else { return }
        data.append(output.data)
    }
    let stderr = messages.reduce(into: Data()) { data, message in
        guard case .stderrOutput(let output)? = message.responseType else { return }
        data.append(output.data)
    }

    let stdoutText = String(decoding: stdout, as: UTF8.self)
        .trimmingCharacters(in: .whitespacesAndNewlines)
    let stderrText = String(decoding: stderr, as: UTF8.self)
        .trimmingCharacters(in: .whitespacesAndNewlines)

    if stdoutText.isEmpty, !stderrText.isEmpty {
        throw TestError(description: "Process produced no stdout. stderr: \(stderrText)")
    }

    return stdoutText
}

private func listContainers(
    service: ContainerService
) async throws -> [AppContainer] {
    let response = try await service.listContainers(
        request: ServerRequest(
            metadata: [:],
            message: Wendy_Agent_Services_V1_ListContainersRequest()
        ),
        context: makeServerContext(method: "ListContainers")
    )

    let contents = try response.accepted.get()
    let writer = CollectingWriter<Wendy_Agent_Services_V1_ListContainersResponse>()
    _ = try await contents.producer(RPCWriter(wrapping: writer))

    return writer.snapshot().compactMap(\.container)
}

private func listContainerStats(
    service: ContainerService
) async throws -> [Wendy_Agent_Services_V1_ContainerStats] {
    let response = try await service.listContainerStats(
        request: ServerRequest(
            metadata: [:],
            message: Wendy_Agent_Services_V1_ListContainerStatsRequest()
        ),
        context: makeServerContext(method: "ListContainerStats")
    )
    return try response.message.stats
}

private func makeServerContext(method: String) -> ServerContext {
    ServerContext(
        descriptor: MethodDescriptor(
            fullyQualifiedService: "wendy.agent.services.v1.WendyContainerService",
            method: method
        ),
        remotePeer: "in-process:test",
        localPeer: "in-process:test",
        cancellation: .init()
    )
}

private func driveWriteLayer(
    service: ContainerService,
    digest: String,
    chunks: [Data]
) async throws {
    let stream = AsyncThrowingStream<Wendy_Agent_Services_V1_WriteLayerRequest, any Error> {
        continuation in
        var first = true
        for chunk in chunks {
            var message = Wendy_Agent_Services_V1_WriteLayerRequest()
            if first {
                message.digest = digest
                first = false
            }
            message.data = chunk
            continuation.yield(message)
        }
        continuation.finish()
    }
    let request = StreamingServerRequest<Wendy_Agent_Services_V1_WriteLayerRequest>(
        metadata: [:],
        messages: RPCAsyncSequence(wrapping: stream)
    )
    _ = try await service.writeLayer(
        request: request,
        context: makeServerContext(method: "WriteLayer")
    )
}

private func writePrintPWDScript(to url: URL) throws {
    try "#!/bin/sh\n/bin/pwd\n".write(to: url, atomically: true, encoding: .utf8)
    try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: url.path)
}

private func writeSandboxProfile(to url: URL) throws {
    try "(version 1)\n(allow default)\n".write(to: url, atomically: true, encoding: .utf8)
}

private func writeSleepScript(to url: URL) throws {
    try "#!/bin/sh\nwhile true; do\n  sleep 1\ndone\n".write(
        to: url,
        atomically: true,
        encoding: .utf8
    )
    try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: url.path)
}

private func writeExitAfterDelayScript(to url: URL) throws {
    try "#!/bin/sh\nsleep 0.2\n".write(to: url, atomically: true, encoding: .utf8)
    try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: url.path)
}

private func writeExitWithCodeScript(to url: URL, code: Int) throws {
    try "#!/bin/sh\nsleep 0.2\nexit \(code)\n".write(to: url, atomically: true, encoding: .utf8)
    try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: url.path)
}

private func writeDisconnectOutputScript(
    to url: URL,
    firstMarker: String,
    laterMarker: String,
    releaseFIFOName: String,
    holdFIFOName: String
) throws {
    try
        "#!/bin/sh\nprintf '%s\\n' '\(firstMarker)'\nIFS= read -r _ < '\(releaseFIFOName)'\nprintf '%s\\n' '\(laterMarker)'\nIFS= read -r _ < '\(holdFIFOName)'\n"
        .write(
            to: url,
            atomically: true,
            encoding: .utf8
        )
    try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: url.path)
}

private func writeBlockingScript(to url: URL, holdFIFOName: String) throws {
    try "#!/bin/sh\nIFS= read -r _ < '\(holdFIFOName)'\n".write(
        to: url,
        atomically: true,
        encoding: .utf8
    )
    try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: url.path)
}

private func makeFIFO(at url: URL) throws {
    guard Darwin.mkfifo(url.path, S_IRUSR | S_IWUSR) == 0 else {
        throw TestError(description: "Failed to create FIFO at \(url.path): errno \(errno)")
    }
}

private func signalFIFO(at url: URL) throws {
    let handle = try FileHandle(forWritingTo: url)
    try handle.write(contentsOf: Data("continue\n".utf8))
    try handle.close()
}

private func logRequest(
    _ request: Opentelemetry_Proto_Collector_Logs_V1_ExportLogsServiceRequest?,
    contains marker: String
) -> Bool {
    request?.resourceLogs.contains { resourceLogs in
        resourceLogs.scopeLogs.contains { scopeLogs in
            scopeLogs.logRecords.contains { $0.body.stringValue.contains(marker) }
        }
    } ?? false
}

private func makeTempDir() throws -> String {
    let path =
        FileManager.default.temporaryDirectory
        .appendingPathComponent("wendy-test-\(UUID().uuidString)").path
    try FileManager.default.createDirectory(atPath: path, withIntermediateDirectories: true)
    return path
}

/// Builds a real gzipped tarball at `destination` containing the given files,
/// mirroring the `/usr/bin/tar -xzf` extraction ContainerService performs.
private func makeTarGz(
    containing files: [(name: String, content: String)],
    at destination: URL
)
    throws
{
    let stagingDir = destination.deletingLastPathComponent()
        .appendingPathComponent("tar-staging-\(UUID().uuidString)")
    try FileManager.default.createDirectory(at: stagingDir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: stagingDir) }

    for file in files {
        try file.content.write(
            to: stagingDir.appendingPathComponent(file.name),
            atomically: true,
            encoding: .utf8
        )
    }

    let process = Foundation.Process()
    process.executableURL = URL(fileURLWithPath: "/usr/bin/tar")
    process.arguments = ["-czf", destination.path, "-C", stagingDir.path] + files.map(\.name)
    try process.run()
    process.waitUntilExit()
    guard process.terminationStatus == 0 else {
        throw TestError(description: "Failed to build test tar.gz at \(destination.path)")
    }
}

private func canonicalPath(_ path: String) throws -> String {
    var resolved = [CChar](repeating: 0, count: Int(PATH_MAX))
    guard path.withCString({ realpath($0, &resolved) }) != nil else {
        throw TestError(description: "Failed to resolve canonical path for \(path)")
    }
    let count = resolved.firstIndex(of: 0) ?? resolved.count
    return String(decoding: resolved.prefix(count).map(UInt8.init(bitPattern:)), as: UTF8.self)
}

private func cleanup(_ path: String) {
    try? FileManager.default.removeItem(atPath: path)
}

private func readPersistedApps(at url: URL) throws -> [WendyApp] {
    let data = try Data(contentsOf: url)
    return try JSONDecoder().decode([WendyApp].self, from: data)
}

private func waitUntil(
    description: String,
    // Generous on purpose: these waits are for a real child process to exit,
    // and the whole package's tests run in parallel, so a 2 s budget goes
    // flaky as soon as the machine is loaded.
    timeout: Duration = .seconds(10),
    pollInterval: Duration = .milliseconds(20),
    condition: @escaping @Sendable () async -> Bool
) async throws {
    let clock = ContinuousClock()
    let deadline = clock.now + timeout

    while clock.now < deadline {
        if await condition() {
            return
        }
        try await Task.sleep(for: pollInterval)
    }

    throw TestError(description: "Timed out waiting for \(description)")
}
