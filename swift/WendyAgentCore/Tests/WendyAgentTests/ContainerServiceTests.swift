import Darwin
import Foundation
import GRPCCore
import Testing
import WendyAgentGRPC

@testable import WendyAgentCore

@Suite("ContainerService.startContainer")
struct ContainerServiceTests {
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

        try await waitUntil(
            description: "app exits and becomes stopped",
            timeout: .seconds(5)
        ) {
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

    @Test("docker-backed start marks the app running with a nil pid")
    func dockerStartMarksAppRunningWithNilPID() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.DockerStartNilPID"
        let fakeDocker = try FakeDockerEnvironment()
        defer { fakeDocker.cleanup() }

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase),
            dockerExecutable: fakeDocker.scriptURL.path
        )

        await service.setDockerPresentForTesting(true)
        try await registerLinuxContainerApp(
            service: service,
            appID: appID,
            imageName: "localhost:5555/\(appID):latest"
        )
        try await startApp(service: service, appID: appID)

        #expect(
            await service.appInfo(forAppID: appID)
                == WendyAppInfo(id: appID, kind: .container, status: .running, pid: nil)
        )
        #expect(fakeDocker.containerIsRunning(appID: appID))

        let persistedApps = try readPersistedApps(at: await service.infoFileURLForTesting())
        let persistedApp = try #require(persistedApps.first { $0.info.id == appID })
        #expect(persistedApp.info.status == .running)
        #expect(persistedApp.info.pid == nil)
    }

    @Test("docker-backed start stream disconnect does not stop the detached container")
    func dockerStartStreamDisconnectDoesNotStopTheDetachedContainer() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.DockerDisconnect"
        let fakeDocker = try FakeDockerEnvironment()
        defer { fakeDocker.cleanup() }

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase),
            dockerExecutable: fakeDocker.scriptURL.path
        )

        await service.setDockerPresentForTesting(true)
        try await registerLinuxContainerApp(
            service: service,
            appID: appID,
            imageName: "localhost:5555/\(appID):latest"
        )

        let response = try await startAppResponse(service: service, appID: appID)
        let contents = try response.accepted.get()

        do {
            _ = try await contents.producer(
                RPCWriter(wrapping: DisconnectAfterStartedWriter())
            )
            Issue.record("Expected the simulated client disconnect to abort the stream")
        } catch is SimulatedClientDisconnect {
            // Expected.
        }

        #expect(fakeDocker.containerIsRunning(appID: appID))
        #expect(
            await service.appInfo(forAppID: appID)
                == WendyAppInfo(id: appID, kind: .container, status: .running, pid: nil)
        )

        try await stopApp(service: service, appID: appID)
        #expect(!fakeDocker.containerExists(appID: appID))
        #expect(
            await service.appInfo(forAppID: appID)
                == WendyAppInfo(id: appID, kind: .container, status: .stopped, pid: nil)
        )
    }

    @Test("docker attach disconnect does not stop the detached container")
    func dockerAttachDisconnectDoesNotStopTheDetachedContainer() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.DockerAttachDisconnect"
        let fakeDocker = try FakeDockerEnvironment()
        defer { fakeDocker.cleanup() }

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase),
            dockerExecutable: fakeDocker.scriptURL.path
        )

        await service.setDockerPresentForTesting(true)
        try await registerLinuxContainerApp(
            service: service,
            appID: appID,
            imageName: "localhost:5555/\(appID):latest"
        )
        try await startApp(service: service, appID: appID)

        let response = try await attachAppResponse(service: service, appID: appID)
        let contents = try response.accepted.get()

        do {
            _ = try await contents.producer(
                RPCWriter(wrapping: DisconnectAfterFirstOutputWriter())
            )
            Issue.record("Expected the simulated client disconnect to abort the attach stream")
        } catch is SimulatedClientDisconnect {
            // Expected.
        }

        try await waitUntil(description: "docker attach session starts") {
            fakeDocker.attachSessionStarted(appID: appID)
        }
        try await waitUntil(description: "docker attach session ends") {
            !fakeDocker.attachSessionIsActive(appID: appID)
        }

        #expect(fakeDocker.containerIsRunning(appID: appID))
        #expect(
            await service.appInfo(forAppID: appID)
                == WendyAppInfo(id: appID, kind: .container, status: .running, pid: nil)
        )

        try await stopApp(service: service, appID: appID)
        #expect(!fakeDocker.containerExists(appID: appID))
    }

    @Test("docker-backed delete removes the detached container without a stored runtime handle")
    func dockerDeleteRemovesDetachedContainerWithoutAStoredRuntimeHandle() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.DockerDelete"
        let fakeDocker = try FakeDockerEnvironment()
        defer { fakeDocker.cleanup() }

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase),
            dockerExecutable: fakeDocker.scriptURL.path
        )

        await service.setDockerPresentForTesting(true)
        try await registerLinuxContainerApp(
            service: service,
            appID: appID,
            imageName: "localhost:5555/\(appID):latest"
        )
        try await startApp(service: service, appID: appID)

        #expect(fakeDocker.containerIsRunning(appID: appID))
        #expect(
            await service.appInfo(forAppID: appID)
                == WendyAppInfo(id: appID, kind: .container, status: .running, pid: nil)
        )

        try await deleteApp(service: service, appID: appID)

        #expect(!fakeDocker.containerExists(appID: appID))
        #expect(await service.appInfo(forAppID: appID) == nil)
    }

    @Test("listContainers reconciles disappeared docker apps as stopped")
    func listContainersReconcilesDisappearedDockerAppsAsStopped() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.DockerReconcile"
        let fakeDocker = try FakeDockerEnvironment()
        defer { fakeDocker.cleanup() }

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase),
            dockerExecutable: fakeDocker.scriptURL.path
        )

        await service.setDockerPresentForTesting(true)
        try await registerLinuxContainerApp(
            service: service,
            appID: appID,
            imageName: "localhost:5555/\(appID):latest"
        )
        try await startApp(service: service, appID: appID)

        #expect(fakeDocker.containerIsRunning(appID: appID))
        fakeDocker.removeContainer(appID: appID)

        let containers = try await listContainers(service: service)
        let container = try #require(containers.first { $0.appName == appID })

        #expect(container.runningState == .stopped)
        #expect(
            await service.appInfo(forAppID: appID)
                == WendyAppInfo(id: appID, kind: .container, status: .stopped, pid: nil)
        )
    }

    @Test("docker monitor updates app state after an external container exit")
    func dockerMonitorUpdatesAppStateAfterExternalContainerExit() async throws {
        let appsBase = try makeTempDir()
        defer { cleanup(appsBase) }

        let appID = "sh.wendy.tests.DockerMonitorExit"
        let fakeDocker = try FakeDockerEnvironment()
        defer { fakeDocker.cleanup() }

        let service = ContainerService(
            broadcaster: TelemetryBroadcaster(),
            executablePath: "/usr/bin/false",
            appsBase: URL(fileURLWithPath: appsBase),
            dockerExecutable: fakeDocker.scriptURL.path
        )

        await service.setDockerPresentForTesting(true)
        try await registerLinuxContainerApp(
            service: service,
            appID: appID,
            imageName: "localhost:5555/\(appID):latest"
        )
        try await startApp(service: service, appID: appID)
        await service.startDockerMonitorForTesting()

        try await waitUntil(description: "docker monitor starts") {
            fakeDocker.monitorStarted()
        }

        fakeDocker.simulateContainerExit(appID: appID, event: "die")

        try await waitUntil(description: "monitor marks docker app stopped") {
            await service.appInfo(forAppID: appID)
                == WendyAppInfo(id: appID, kind: .container, status: .stopped, pid: nil)
        }

        await service.stopDockerMonitorForTesting()
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
                    currentDirectory: appDirectory.path
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

private struct TestError: Error, CustomStringConvertible {
    let description: String
}

private struct SimulatedClientDisconnect: Error {}

private final class DisconnectAfterStartedWriter: RPCWriterProtocol, @unchecked Sendable {
    func write(_ element: Wendy_Agent_Services_V1_RunContainerLayersResponse) async throws {
        if case .started? = element.responseType {
            throw SimulatedClientDisconnect()
        }
    }

    func write(
        contentsOf elements: some Sequence<Wendy_Agent_Services_V1_RunContainerLayersResponse>
    ) async throws {
        for element in elements {
            try await self.write(element)
        }
    }
}

private final class DisconnectAfterFirstOutputWriter: RPCWriterProtocol, @unchecked Sendable {
    func write(_ element: Wendy_Agent_Services_V1_RunContainerLayersResponse) async throws {
        switch element.responseType {
        case .stdoutOutput?, .stderrOutput?:
            throw SimulatedClientDisconnect()
        default:
            return
        }
    }

    func write(
        contentsOf elements: some Sequence<Wendy_Agent_Services_V1_RunContainerLayersResponse>
    ) async throws {
        for element in elements {
            try await self.write(element)
        }
    }
}

private struct FakeDockerEnvironment {
    let directoryURL: URL
    let scriptURL: URL

    init() throws {
        self.directoryURL = FileManager.default.temporaryDirectory
            .appendingPathComponent("wendy-fake-docker-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(
            at: self.directoryURL,
            withIntermediateDirectories: true
        )

        let stateDirectory = self.directoryURL.appendingPathComponent("state", isDirectory: true)
        try FileManager.default.createDirectory(at: stateDirectory, withIntermediateDirectories: true)
        let eventsPipePath = stateDirectory.appendingPathComponent("events.pipe").path
        guard mkfifo(eventsPipePath, 0o644) == 0 else {
            throw TestError(description: "Failed to create fake docker events pipe at \(eventsPipePath)")
        }

        self.scriptURL = self.directoryURL.appendingPathComponent("docker")
        try Self.writeExecutableScript(
            to: self.scriptURL,
            stateDirectory: stateDirectory.path
        )
    }

    func cleanup() {
        try? FileManager.default.removeItem(at: self.directoryURL)
    }

    func containerExists(appID: String) -> Bool {
        FileManager.default.fileExists(atPath: self.containerDirectory(appID: appID).path)
    }

    func containerIsRunning(appID: String) -> Bool {
        FileManager.default.fileExists(
            atPath: self.containerDirectory(appID: appID)
                .appendingPathComponent("running").path
        )
    }

    func monitorStarted() -> Bool {
        FileManager.default.fileExists(
            atPath: self.directoryURL.appendingPathComponent("state/events-monitor-started").path
        )
    }

    func attachSessionStarted(appID: String) -> Bool {
        FileManager.default.fileExists(
            atPath: self.directoryURL.appendingPathComponent(
                "state/attach-session-started/\(self.containerName(appID: appID))"
            ).path
        )
    }

    func attachSessionIsActive(appID: String) -> Bool {
        FileManager.default.fileExists(
            atPath: self.directoryURL.appendingPathComponent(
                "state/attach-sessions/\(self.containerName(appID: appID))"
            ).path
        )
    }

    func removeContainer(appID: String) {
        try? FileManager.default.removeItem(at: self.containerDirectory(appID: appID))
    }

    func simulateContainerExit(appID: String, event: String) {
        self.removeContainer(appID: appID)
        let eventLine = "\(event)\t\(appID)\n"
        let eventsPipeURL = self.directoryURL.appendingPathComponent("state/events.pipe")
        if let handle = try? FileHandle(forWritingTo: eventsPipeURL) {
            defer { try? handle.close() }
            try? handle.write(contentsOf: Data(eventLine.utf8))
        }
    }

    private func containerDirectory(appID: String) -> URL {
        self.directoryURL.appendingPathComponent(
            "state/containers/\(self.containerName(appID: appID))",
            isDirectory: true
        )
    }

    private func containerName(appID: String) -> String {
        "wendy-\(appID)"
    }

    private static func writeExecutableScript(to url: URL, stateDirectory: String) throws {
        let script = """
            #!/bin/sh
            STATE_DIR="\(stateDirectory)"
            mkdir -p "$STATE_DIR/containers" "$STATE_DIR/log-sessions" "$STATE_DIR/log-session-started" "$STATE_DIR/attach-sessions" "$STATE_DIR/attach-session-started"

            container_dir() {
              echo "$STATE_DIR/containers/$1"
            }

            cmd="$1"
            shift

            case "$cmd" in
              version)
                echo "27.0.1"
                exit 0
                ;;
              pull)
                exit 0
                ;;
              ps)
                include_all=0
                filter_name=""
                while [ $# -gt 0 ]; do
                  case "$1" in
                    -a)
                      include_all=1
                      ;;
                    --filter)
                      shift
                      filter_name="$1"
                      ;;
                    --format)
                      shift
                      ;;
                  esac
                  shift
                done

                if [ "$filter_name" = "name=wendy-registry" ]; then
                  if [ -f "$STATE_DIR/registry-running" ]; then
                    echo "Up 1 second"
                  fi
                  exit 0
                fi

                name="${filter_name#name=^/}"
                name="${name%?}"
                name="${name#name=}"
                dir="$(container_dir "$name")"
                if [ "$include_all" -eq 1 ]; then
                  [ -d "$dir" ] && echo "$name"
                else
                  [ -f "$dir/running" ] && echo "$name"
                fi
                exit 0
                ;;
              rm)
                if [ "$1" = "-f" ]; then
                  shift
                fi
                name="$1"
                if [ "$name" = "wendy-registry" ]; then
                  rm -f "$STATE_DIR/registry-running"
                  exit 0
                fi
                rm -rf "$(container_dir "$name")"
                rm -f "$STATE_DIR/log-sessions/$name"
                exit 0
                ;;
              run)
                name=""
                while [ $# -gt 0 ]; do
                  case "$1" in
                    --name)
                      shift
                      name="$1"
                      ;;
                    -p|--label|--network|-v|--restart)
                      shift
                      ;;
                  esac
                  shift
                done

                if [ "$name" = "wendy-registry" ]; then
                  touch "$STATE_DIR/registry-running"
                  echo "registry-id"
                  exit 0
                fi

                dir="$(container_dir "$name")"
                rm -rf "$dir"
                mkdir -p "$dir"
                touch "$dir/running"
                printf 'hello from %s\n' "$name" > "$dir/log.txt"
                echo "cid-$name"
                exit 0
                ;;
              logs)
                if [ "$1" = "-f" ]; then
                  shift
                fi
                name="$1"
                dir="$(container_dir "$name")"
                if [ ! -d "$dir" ]; then
                  echo "No such container: $name" 1>&2
                  exit 1
                fi
                touch "$STATE_DIR/log-session-started/$name"
                touch "$STATE_DIR/log-sessions/$name"
                cleanup() {
                  rm -f "$STATE_DIR/log-sessions/$name"
                }
                terminate() {
                  cleanup
                  exit 0
                }
                trap cleanup EXIT
                trap terminate INT TERM
                cat "$dir/log.txt"
                while [ -f "$dir/running" ]; do
                  sleep 0.1
                done
                exit 0
                ;;
              events)
                touch "$STATE_DIR/events-monitor-started"
                exec cat "$STATE_DIR/events.pipe"
                ;;
              attach)
                name=""
                while [ $# -gt 0 ]; do
                  case "$1" in
                    --no-stdin|--sig-proxy=false)
                      ;;
                    *)
                      name="$1"
                      ;;
                  esac
                  shift
                done
                dir="$(container_dir "$name")"
                if [ ! -d "$dir" ]; then
                  echo "No such container: $name" 1>&2
                  exit 1
                fi
                touch "$STATE_DIR/attach-session-started/$name"
                touch "$STATE_DIR/attach-sessions/$name"
                cleanup() {
                  rm -f "$STATE_DIR/attach-sessions/$name"
                }
                terminate() {
                  cleanup
                  exit 0
                }
                trap cleanup EXIT
                trap terminate INT TERM
                cat "$dir/log.txt"
                cat >/dev/null
                exit 0
                ;;
              stop)
                if [ "$1" = "--time" ]; then
                  shift
                  shift
                fi
                name="$1"
                dir="$(container_dir "$name")"
                if [ ! -d "$dir" ]; then
                  echo "No such container: $name" 1>&2
                  exit 1
                fi
                rm -rf "$dir"
                echo "$name"
                exit 0
                ;;
              kill)
                name="$1"
                dir="$(container_dir "$name")"
                if [ ! -d "$dir" ]; then
                  echo "No such container: $name" 1>&2
                  exit 1
                fi
                rm -rf "$dir"
                echo "$name"
                exit 0
                ;;
            esac

            echo "unsupported fake docker command: $cmd" 1>&2
            exit 1
            """

        try script.write(to: url, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: url.path)
    }
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

private func registerLinuxContainerApp(
    service: ContainerService,
    appID: String,
    imageName: String
) async throws {
    var request = Wendy_Agent_Services_V1_CreateContainerRequest()
    request.appName = appID
    request.imageName = imageName
    request.appConfig = try JSONEncoder().encode(
        WendyAppConfig(appId: appID, platform: "linux/arm64", entitlements: nil)
    )

    _ = try await service.createContainer(
        request: ServerRequest(metadata: [:], message: request),
        context: makeServerContext(method: "CreateContainer")
    )
}

private func startAppResponse(
    service: ContainerService,
    appID: String
) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_RunContainerLayersResponse> {
    var request = Wendy_Agent_Services_V1_StartContainerRequest()
    request.appName = appID

    return try await service.startContainer(
        request: ServerRequest(metadata: [:], message: request),
        context: makeServerContext(method: "StartContainer")
    )
}

private func attachAppResponse(
    service: ContainerService,
    appID: String
) async throws -> StreamingServerResponse<Wendy_Agent_Services_V1_RunContainerLayersResponse> {
    var firstMessage = Wendy_Agent_Services_V1_AttachContainerRequest()
    firstMessage.requestType = .appName(appID)

    return try await service.attachContainer(
        request: StreamingServerRequest(
            metadata: [:],
            messages: RPCAsyncSequence(wrapping: AsyncThrowingStream { continuation in
                continuation.yield(firstMessage)
                continuation.finish()
            })
        ),
        context: makeServerContext(method: "AttachContainer")
    )
}

private func startApp(
    service: ContainerService,
    appID: String
) async throws {
    _ = try await startAppResponse(service: service, appID: appID)
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

private func makeTempDir() throws -> String {
    let path =
        FileManager.default.temporaryDirectory
        .appendingPathComponent("wendy-test-\(UUID().uuidString)").path
    try FileManager.default.createDirectory(atPath: path, withIntermediateDirectories: true)
    return path
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
    timeout: Duration = .seconds(2),
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
