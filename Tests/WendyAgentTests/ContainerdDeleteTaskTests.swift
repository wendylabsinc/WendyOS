import ContainerdGRPC
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2Posix
import SwiftProtobuf
import Testing

@testable import wendy_agent

@Suite("deleteTask() Functionality")
struct ContainerdDeleteTaskTests {

    // MARK: - Happy Path Tests

    @Test("Running task - sends SIGKILL, waits for exit, deletes")
    func runningTask_sendsKillWaitsDeletes() async throws {
        // Arrange
        let tasksClient = MockTasksClient()

        // First call: task is running. Second call: task has exited.
        let runningTask = Containerd_V1_Types_Process.with {
            $0.id = "test-task"
            $0.containerID = "test-container"
            // hasExitedAt is false by default (no exitedAt set)
        }
        let exitedTask = Containerd_V1_Types_Process.with {
            $0.id = "test-task"
            $0.containerID = "test-container"
            $0.exitedAt = .init(date: Date())
        }

        await tasksClient.setListResponseSequence([
            .with { $0.tasks = [runningTask] },  // Initial list
            .with { $0.tasks = [exitedTask] },   // After kill, task has exited
        ])

        let containerd = try makeContainerd(tasksClient: tasksClient)

        // Act
        try await containerd.deleteTask(containerID: "test-container", waitTimeout: .seconds(1))

        // Assert
        let killCount = await tasksClient.killCallCount
        let deleteCount = await tasksClient.deleteCallCount
        let killedIDs = await tasksClient.killedContainerIDs
        let deletedIDs = await tasksClient.deletedContainerIDs

        #expect(killCount == 1, "Should send SIGKILL once")
        #expect(killedIDs == ["test-task"], "Should kill the correct task")
        #expect(deleteCount == 1, "Should delete once")
        #expect(deletedIDs == ["test-task"], "Should delete the correct task")
    }

    @Test("Already exited task - skips SIGKILL, deletes immediately")
    func exitedTask_skipsKillDeletesImmediately() async throws {
        // Arrange
        let tasksClient = MockTasksClient()

        let exitedTask = Containerd_V1_Types_Process.with {
            $0.id = "test-task"
            $0.containerID = "test-container"
            $0.exitedAt = .init(date: Date())
        }

        await tasksClient.setListResponse(.with { $0.tasks = [exitedTask] })

        let containerd = try makeContainerd(tasksClient: tasksClient)

        // Act
        try await containerd.deleteTask(containerID: "test-container", waitTimeout: .seconds(1))

        // Assert
        let killCount = await tasksClient.killCallCount
        let deleteCount = await tasksClient.deleteCallCount

        #expect(killCount == 0, "Should NOT send SIGKILL for already exited task")
        #expect(deleteCount == 1, "Should delete the task")
    }

    @Test("No matching task - no-op, returns without error")
    func noMatchingTask_noop() async throws {
        // Arrange
        let tasksClient = MockTasksClient()

        // Return tasks that don't match our container
        let otherTask = Containerd_V1_Types_Process.with {
            $0.id = "other-task"
            $0.containerID = "other-container"
        }

        await tasksClient.setListResponse(.with { $0.tasks = [otherTask] })

        let containerd = try makeContainerd(tasksClient: tasksClient)

        // Act
        try await containerd.deleteTask(containerID: "nonexistent", waitTimeout: .seconds(1))

        // Assert
        let killCount = await tasksClient.killCallCount
        let deleteCount = await tasksClient.deleteCallCount

        #expect(killCount == 0, "Should not kill any task")
        #expect(deleteCount == 0, "Should not delete any task")
    }

    @Test("Empty task list - no-op, returns without error")
    func emptyTaskList_noop() async throws {
        // Arrange
        let tasksClient = MockTasksClient()
        await tasksClient.setListResponse(.with { $0.tasks = [] })

        let containerd = try makeContainerd(tasksClient: tasksClient)

        // Act
        try await containerd.deleteTask(containerID: "test-container", waitTimeout: .seconds(1))

        // Assert
        let killCount = await tasksClient.killCallCount
        let deleteCount = await tasksClient.deleteCallCount

        #expect(killCount == 0)
        #expect(deleteCount == 0)
    }

    // MARK: - Kill Error Handling

    @Test("Kill returns notFound - continues gracefully to delete")
    func killNotFound_continuesGracefully() async throws {
        // Arrange
        let tasksClient = MockTasksClient()

        let runningTask = Containerd_V1_Types_Process.with {
            $0.id = "test-task"
            $0.containerID = "test-container"
        }

        // Task is running, but kill returns notFound (race condition - task exited between list and kill)
        await tasksClient.setListResponseSequence([
            .with { $0.tasks = [runningTask] },
            .with { $0.tasks = [] },  // Task gone after kill attempt
        ])
        await tasksClient.setKillError(RPCError(code: .notFound, message: "Task not found"))

        let containerd = try makeContainerd(tasksClient: tasksClient)

        // Act - should not throw
        try await containerd.deleteTask(containerID: "test-container", waitTimeout: .seconds(1))

        // Assert
        let killCount = await tasksClient.killCallCount
        let deleteCount = await tasksClient.deleteCallCount

        #expect(killCount == 1, "Should have attempted kill")
        // Delete is still called on the original task reference from the initial list
        #expect(deleteCount == 1, "Should still delete the task")
    }

    // MARK: - Timeout Scenarios

    @Test("Task exits within timeout - succeeds")
    func taskExitsWithinTimeout_succeeds() async throws {
        // Arrange
        let tasksClient = MockTasksClient()

        let runningTask = Containerd_V1_Types_Process.with {
            $0.id = "test-task"
            $0.containerID = "test-container"
        }
        let exitedTask = Containerd_V1_Types_Process.with {
            $0.id = "test-task"
            $0.containerID = "test-container"
            $0.exitedAt = .init(date: Date())
        }

        // Task exits after 3 poll cycles
        await tasksClient.setListResponseSequence([
            .with { $0.tasks = [runningTask] },  // Initial
            .with { $0.tasks = [runningTask] },  // Poll 1 - still running
            .with { $0.tasks = [runningTask] },  // Poll 2 - still running
            .with { $0.tasks = [exitedTask] },   // Poll 3 - exited
        ])

        let containerd = try makeContainerd(tasksClient: tasksClient)

        // Act
        try await containerd.deleteTask(containerID: "test-container", waitTimeout: .seconds(2))

        // Assert
        let deleteCount = await tasksClient.deleteCallCount
        let listCount = await tasksClient.listCallCount

        #expect(deleteCount == 1, "Should delete after task exits")
        #expect(listCount >= 3, "Should have polled multiple times")
    }

    @Test("Task disappears from list during polling - treats as exited")
    func taskDisappearsDuringPolling_treatsAsExited() async throws {
        // Arrange
        let tasksClient = MockTasksClient()

        let runningTask = Containerd_V1_Types_Process.with {
            $0.id = "test-task"
            $0.containerID = "test-container"
        }

        // Task disappears from list (cleaned up externally)
        await tasksClient.setListResponseSequence([
            .with { $0.tasks = [runningTask] },  // Initial - running
            .with { $0.tasks = [] },             // Poll - task gone
        ])

        let containerd = try makeContainerd(tasksClient: tasksClient)

        // Act - should succeed (task considered exited when gone from list)
        try await containerd.deleteTask(containerID: "test-container", waitTimeout: .seconds(1))

        // Assert
        let killCount = await tasksClient.killCallCount
        let deleteCount = await tasksClient.deleteCallCount

        #expect(killCount == 1, "Should have sent SIGKILL")
        // Delete is still called on the original task reference from the initial list
        #expect(deleteCount == 1, "Should still delete the task")
    }

    // MARK: - Delete Error Handling

    @Test("Delete fails - propagates error")
    func deleteFails_propagatesError() async throws {
        // Arrange
        let tasksClient = MockTasksClient()

        let exitedTask = Containerd_V1_Types_Process.with {
            $0.id = "test-task"
            $0.containerID = "test-container"
            $0.exitedAt = .init(date: Date())
        }

        await tasksClient.setListResponse(.with { $0.tasks = [exitedTask] })
        await tasksClient.setDeleteError(RPCError(code: .permissionDenied, message: "Permission denied"))

        let containerd = try makeContainerd(tasksClient: tasksClient)

        // Act & Assert
        await #expect(throws: RPCError.self) {
            try await containerd.deleteTask(containerID: "test-container", waitTimeout: .seconds(1))
        }
    }

    // MARK: - Multiple Tasks

    @Test("Multiple tasks - only kills and deletes matching one")
    func multipleTasks_onlyDeletesMatching() async throws {
        // Arrange
        let tasksClient = MockTasksClient()

        let task1 = Containerd_V1_Types_Process.with {
            $0.id = "task-1"
            $0.containerID = "container-1"
            $0.exitedAt = .init(date: Date())
        }
        let task2 = Containerd_V1_Types_Process.with {
            $0.id = "task-2"
            $0.containerID = "container-2"
            $0.exitedAt = .init(date: Date())
        }
        let task3 = Containerd_V1_Types_Process.with {
            $0.id = "task-3"
            $0.containerID = "container-3"
            $0.exitedAt = .init(date: Date())
        }

        await tasksClient.setListResponse(.with { $0.tasks = [task1, task2, task3] })

        let containerd = try makeContainerd(tasksClient: tasksClient)

        // Act
        try await containerd.deleteTask(containerID: "container-2", waitTimeout: .seconds(1))

        // Assert
        let deletedIDs = await tasksClient.deletedContainerIDs
        let killCount = await tasksClient.killCallCount

        #expect(deletedIDs == ["task-2"], "Should only delete matching task")
        #expect(killCount == 0, "Should not kill already-exited tasks")
    }

    @Test("Matches by task ID when containerID doesn't match")
    func matchesByTaskID() async throws {
        // Arrange
        let tasksClient = MockTasksClient()

        // Task where id matches but containerID is different
        let task = Containerd_V1_Types_Process.with {
            $0.id = "my-app"
            $0.containerID = "different-container-id"
            $0.exitedAt = .init(date: Date())
        }

        await tasksClient.setListResponse(.with { $0.tasks = [task] })

        let containerd = try makeContainerd(tasksClient: tasksClient)

        // Act - search by task ID
        try await containerd.deleteTask(containerID: "my-app", waitTimeout: .seconds(1))

        // Assert
        let deletedIDs = await tasksClient.deletedContainerIDs
        #expect(deletedIDs == ["my-app"], "Should match by task ID")
    }

    // MARK: - Helper Methods

    private func makeContainerd(
        containersClient: MockContainersClient? = nil,
        snapshotsClient: MockSnapshotsClient? = nil,
        tasksClient: MockTasksClient
    ) throws -> Containerd {
        return Containerd(
            client: try makeDummyClient(),
            containersClient: containersClient ?? MockContainersClient(),
            snapshotsClient: snapshotsClient ?? MockSnapshotsClient(),
            tasksClient: tasksClient
        )
    }

    private func makeDummyClient() throws -> GRPCClient<HTTP2ClientTransport.Posix> {
        // Create a dummy client that won't actually be used
        // This is required by the Containerd init, but the protocol mocks will be used instead
        return GRPCClient(
            transport: try HTTP2ClientTransport.Posix(
                target: .unixDomainSocket(path: "/run/containerd/containerd.sock"),
                transportSecurity: .plaintext,
                config: .defaults
            )
        )
    }
}
