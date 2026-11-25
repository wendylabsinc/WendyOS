import Foundation
import Synchronization
import Testing

@testable import WendyAgentCore

private final class DeliveryTestClock: Sendable {
    private let time: Mutex<Int64>
    init(_ value: Int64) { time = Mutex(value) }
    func now() -> Int64 { time.withLock { $0 } }
    func advance(_ seconds: Int64) { time.withLock { $0 += seconds } }
}

@Suite("Durable native delivery authority")
struct BuildDeliveryAuthorityTests {
    @Test("builder key, upload ownership, byte budget and publication CAS survive restart")
    func durableAdmission() async throws {
        guard #available(macOS 26.0, *) else { return }
        let fixture = try BuildDeliveryFixture(operatorLifetime: 3)
        let clock = DeliveryTestClock(fixture.time)
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(
            "delivery-state-\(UUID().uuidString)"
        )
        defer { try? FileManager.default.removeItem(at: root) }
        let images = BlobStore(root: root)
        let path = root.appendingPathComponent("authorizations")
        let original = try await fixture.authority(root: path, images: images, now: { clock.now() })
        let claims = try fixture.claims(budget: 100)
        let compact = try fixture.sign(claims)
        let session = try await original.admit(compact, operator: fixture.primary.certificate)
        #expect(session == BuildDeliveryContract.sessionID(compact))
        await #expect(throws: (any Error).self) {
            try await original.policy(
                session,
                leaf: fixture.otherBuilder.certificate,
                repository: "app"
            )
        }
        await #expect(throws: (any Error).self) {
            try await original.policy(
                session,
                leaf: fixture.builder.certificate,
                repository: "other"
            )
        }
        let upload = try await original.newUpload(
            session,
            leaf: fixture.builder.certificate,
            repository: "app"
        )
        try await original.checkUpload(
            upload,
            session: session,
            leaf: fixture.builder.certificate,
            repository: "app"
        )
        await #expect(throws: (any Error).self) {
            try await original.checkUpload(
                upload + "x",
                session: session,
                leaf: fixture.builder.certificate,
                repository: "app"
            )
        }
        try await original.reserve(session, leaf: fixture.builder.certificate, bytes: 60)
        let competing = BuildDeliveryAuthority(directory: path, images: images)
        await #expect(throws: (any Error).self) {
            try await competing.configure(
                certPEM: fixture.target.certPEM,
                chainPEM: fixture.root.pem
            )
        }
        await competing.close()
        await original.close()
        clock.advance(20)
        let recovered = try await fixture.authority(
            root: path,
            images: images,
            now: { clock.now() }
        )
        // The signing leaf has expired, but the recorded admission was timely.
        _ = try await recovered.policy(
            session,
            leaf: fixture.builder.certificate,
            repository: "app"
        )
        try await recovered.checkUpload(
            upload,
            session: session,
            leaf: fixture.builder.certificate,
            repository: "app"
        )
        await #expect(throws: (any Error).self) {
            try await recovered.reserve(session, leaf: fixture.builder.certificate, bytes: 41)
        }
        await #expect(throws: (any Error).self) {
            try await recovered.admit(
                fixture.sign(fixture.claims()),
                operator: fixture.primary.certificate
            )
        }
        let data = Data(#"{"schemaVersion":2}"#.utf8)
        try await recovered.publish(
            data,
            repository: "app",
            reference: "latest",
            session: session,
            leaf: fixture.builder.certificate
        )
        let status = try await recovered.status(session, leaf: fixture.builder.certificate)
        #expect(status.completed && status.output == BlobStore.contentDigest(data))
        let savedRecord = path.appendingPathComponent(session + ".json")
        let beforeReplay = try Data(contentsOf: savedRecord)
        try await recovered.reserveManifest(session, leaf: fixture.builder.certificate, bytes: 1000)
        #expect(try Data(contentsOf: savedRecord) == beforeReplay)
        await #expect(throws: (any Error).self) {
            try await recovered.reserve(session, leaf: fixture.builder.certificate, bytes: 1)
        }
        // Replay is read-only, including after an ambient operator changes the tag.
        try await recovered.publish(
            data,
            repository: "app",
            reference: "latest",
            session: session,
            leaf: fixture.builder.certificate
        )
        let newer = Data(#"{"schemaVersion":2,"newer":true}"#.utf8)
        try await recovered.publish(
            newer,
            repository: "app",
            reference: "latest",
            session: nil,
            leaf: fixture.primary.certificate
        )
        await #expect(throws: (any Error).self) {
            try await recovered.publish(
                data,
                repository: "app",
                reference: "latest",
                session: session,
                leaf: fixture.builder.certificate
            )
        }
        #expect(
            try images.manifestDigest(repository: "app", reference: "latest")
                == BlobStore.contentDigest(newer)
        )
        await #expect(throws: (any Error).self) {
            try await recovered.status(session, leaf: fixture.builder.certificate)
        }
        try await recovered.closeAndRemove()
    }
    @Test("an intervening operator write cannot be overwritten by an admitted build")
    func publicationConflict() async throws {
        guard #available(macOS 26.0, *) else { return }
        let fixture = try BuildDeliveryFixture()
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(
            "delivery-cas-\(UUID().uuidString)"
        )
        defer { try? FileManager.default.removeItem(at: root) }
        let images = BlobStore(root: root)
        let authority = try await fixture.authority(
            root: root.appendingPathComponent("authorizations"),
            images: images
        )
        let compact = try fixture.sign(fixture.claims())
        let session = try await authority.admit(compact, operator: fixture.primary.certificate)
        let ambient = Data("operator publication".utf8)
        try await authority.publish(
            ambient,
            repository: "app",
            reference: "latest",
            session: nil,
            leaf: fixture.primary.certificate
        )
        await #expect(throws: (any Error).self) {
            try await authority.publish(
                Data("build publication".utf8),
                repository: "app",
                reference: "latest",
                session: session,
                leaf: fixture.builder.certificate
            )
        }
        #expect(
            try images.manifestDigest(repository: "app", reference: "latest")
                == BlobStore.contentDigest(ambient)
        )
        try await authority.closeAndRemove()
    }
}
