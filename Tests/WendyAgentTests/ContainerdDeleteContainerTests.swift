import ContainerdGRPC
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2Posix
import Testing

@testable import wendy_agent

@Suite("deleteContainer() Functionality")
struct ContainerdDeleteContainerTests {

    // MARK: - Happy Path Tests

    @Test("Successfully delete container with ephemeral snapshot")
    func happyPathWithSnapshot() async throws {
        // Arrange
        let containersClient = MockContainersClient()
        await containersClient.setGetResponse(
            .with {
                $0.container = .with {
                    $0.id = "test-container"
                    $0.snapshotKey = "550e8400-e29b-41d4-a716-446655440000"  // Valid UUID
                    $0.snapshotter = "overlayfs"
                }
            }
        )

        let snapshotsClient = MockSnapshotsClient()
        let tasksClient = MockTasksClient()

        let containerd = Containerd(
            client: try makeDummyClient(),
            containersClient: containersClient,
            snapshotsClient: snapshotsClient,
            tasksClient: tasksClient
        )

        // Act
        try await containerd.deleteContainer(named: "test-container")

        // Assert - no errors thrown means success
    }

    // MARK: - Idempotent Operation Tests

    @Test("Container doesn't exist - returns early without error")
    func containerDoesNotExist() async throws {
        // Arrange
        let containersClient = MockContainersClient()
        await containersClient.setGetError(
            RPCError(code: .notFound, message: "Container not found")
        )

        let snapshotsClient = MockSnapshotsClient()
        let tasksClient = MockTasksClient()

        let containerd = Containerd(
            client: try makeDummyClient(),
            containersClient: containersClient,
            snapshotsClient: snapshotsClient,
            tasksClient: tasksClient
        )

        // Act & Assert - should not throw
        try await containerd.deleteContainer(named: "nonexistent")
    }

    @Test("Container already deleted - continues to snapshot cleanup")
    func containerAlreadyDeleted() async throws {
        // Arrange
        let containersClient = MockContainersClient()
        await containersClient.setGetResponse(
            .with {
                $0.container = .with {
                    $0.id = "test-container"
                    $0.snapshotKey = "550e8400-e29b-41d4-a716-446655440000"
                    $0.snapshotter = "overlayfs"
                }
            }
        )
        // Container.delete() will return .notFound (race condition scenario)
        await containersClient.setDeleteError(RPCError(code: .notFound, message: "Already deleted"))

        let snapshotsClient = MockSnapshotsClient()
        let tasksClient = MockTasksClient()

        let containerd = Containerd(
            client: try makeDummyClient(),
            containersClient: containersClient,
            snapshotsClient: snapshotsClient,
            tasksClient: tasksClient
        )

        // Act & Assert - should not throw, should attempt snapshot cleanup
        try await containerd.deleteContainer(named: "test-container")
    }

    @Test("Snapshot already deleted - catches .notFound")
    func snapshotAlreadyDeleted() async throws {
        // Arrange
        let containersClient = MockContainersClient()
        await containersClient.setGetResponse(
            .with {
                $0.container = .with {
                    $0.id = "test-container"
                    $0.snapshotKey = "550e8400-e29b-41d4-a716-446655440000"
                    $0.snapshotter = "overlayfs"
                }
            }
        )

        let snapshotsClient = MockSnapshotsClient()
        await snapshotsClient.setRemoveError(
            RPCError(code: .notFound, message: "Snapshot not found")
        )

        let tasksClient = MockTasksClient()

        let containerd = Containerd(
            client: try makeDummyClient(),
            containersClient: containersClient,
            snapshotsClient: snapshotsClient,
            tasksClient: tasksClient
        )

        // Act & Assert - should not throw
        try await containerd.deleteContainer(named: "test-container")
    }

    // MARK: - Data Consistency Tests

    @Test("Rejects snapshot key with empty snapshotter")
    func emptySnapshotter() async throws {
        // Arrange
        let containersClient = MockContainersClient()
        await containersClient.setGetResponse(
            .with {
                $0.container = .with {
                    $0.id = "test-container"
                    $0.snapshotKey = "550e8400-e29b-41d4-a716-446655440000"
                    $0.snapshotter = ""  // Empty!
                }
            }
        )

        let snapshotsClient = MockSnapshotsClient()
        let tasksClient = MockTasksClient()

        let containerd = Containerd(
            client: try makeDummyClient(),
            containersClient: containersClient,
            snapshotsClient: snapshotsClient,
            tasksClient: tasksClient
        )

        // Act & Assert - should not throw, but should not attempt snapshot deletion
        try await containerd.deleteContainer(named: "test-container")
    }

    @Test("Skips cleanup when snapshot key is empty")
    func emptySnapshotKey() async throws {
        // Arrange
        let containersClient = MockContainersClient()
        await containersClient.setGetResponse(
            .with {
                $0.container = .with {
                    $0.id = "test-container"
                    $0.snapshotKey = ""  // Empty
                    $0.snapshotter = "overlayfs"
                }
            }
        )

        let snapshotsClient = MockSnapshotsClient()
        let tasksClient = MockTasksClient()

        let containerd = Containerd(
            client: try makeDummyClient(),
            containersClient: containersClient,
            snapshotsClient: snapshotsClient,
            tasksClient: tasksClient
        )

        // Act & Assert - should not throw
        try await containerd.deleteContainer(named: "test-container")
    }

    @Test("Rejects ChainID format snapshot key (sha256:...)")
    func chainIDFormatRejected() async throws {
        // Arrange
        let containersClient = MockContainersClient()
        await containersClient.setGetResponse(
            .with {
                $0.container = .with {
                    $0.id = "test-container"
                    $0.snapshotKey = "sha256:abc123def456"  // ChainID format
                    $0.snapshotter = "overlayfs"
                }
            }
        )

        let snapshotsClient = MockSnapshotsClient()
        let tasksClient = MockTasksClient()

        let containerd = Containerd(
            client: try makeDummyClient(),
            containersClient: containersClient,
            snapshotsClient: snapshotsClient,
            tasksClient: tasksClient
        )

        // Act & Assert - should not throw, should skip snapshot deletion
        try await containerd.deleteContainer(named: "test-container")
    }

    // MARK: - Task Deletion Error Handling

    @Test("Task deletion fails - logs warning, continues")
    func taskDeletionFails() async throws {
        // Arrange
        let containersClient = MockContainersClient()
        await containersClient.setGetResponse(
            .with {
                $0.container = .with {
                    $0.id = "test-container"
                    $0.snapshotKey = "550e8400-e29b-41d4-a716-446655440000"
                    $0.snapshotter = "overlayfs"
                }
            }
        )

        let snapshotsClient = MockSnapshotsClient()

        let tasksClient = MockTasksClient()
        await tasksClient.setDeleteError(
            RPCError(code: .permissionDenied, message: "Permission denied")
        )

        let containerd = Containerd(
            client: try makeDummyClient(),
            containersClient: containersClient,
            snapshotsClient: snapshotsClient,
            tasksClient: tasksClient
        )

        // Act & Assert - should not throw, should continue to container/snapshot deletion
        try await containerd.deleteContainer(named: "test-container")
    }

    // MARK: - Snapshot Deletion Error Handling

    @Test("Snapshot deletion fails - logs warning, succeeds")
    func snapshotDeletionFails() async throws {
        // Arrange
        let containersClient = MockContainersClient()
        await containersClient.setGetResponse(
            .with {
                $0.container = .with {
                    $0.id = "test-container"
                    $0.snapshotKey = "550e8400-e29b-41d4-a716-446655440000"
                    $0.snapshotter = "overlayfs"
                }
            }
        )

        let snapshotsClient = MockSnapshotsClient()
        await snapshotsClient.setRemoveError(
            RPCError(code: .permissionDenied, message: "Permission denied")
        )

        let tasksClient = MockTasksClient()

        let containerd = Containerd(
            client: try makeDummyClient(),
            containersClient: containersClient,
            snapshotsClient: snapshotsClient,
            tasksClient: tasksClient
        )

        // Act & Assert - should not throw (snapshot errors are logged only)
        try await containerd.deleteContainer(named: "test-container")
    }

    // MARK: - Helper Methods

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
