import Foundation
import Testing

@testable import wendy_agent

@Suite("Snapshot Key Validation")
struct ContainerdSnapshotKeyValidationTests {

    @Test("Valid UUID formats are accepted (lowercase)")
    func validUUIDLowercase() {
        #expect(Containerd.isEphemeralSnapshotKey("550e8400-e29b-41d4-a716-446655440000"))
        #expect(Containerd.isEphemeralSnapshotKey("a1b2c3d4-e5f6-4789-a012-345678901234"))
    }

    @Test("Valid UUID formats are accepted (uppercase)")
    func validUUIDUppercase() {
        #expect(Containerd.isEphemeralSnapshotKey("550E8400-E29B-41D4-A716-446655440000"))
        #expect(Containerd.isEphemeralSnapshotKey("A1B2C3D4-E5F6-4789-A012-345678901234"))
    }

    @Test("Valid UUID formats are accepted (mixed case)")
    func validUUIDMixedCase() {
        #expect(Containerd.isEphemeralSnapshotKey("550e8400-E29B-41d4-A716-446655440000"))
        #expect(Containerd.isEphemeralSnapshotKey("A1b2C3d4-e5F6-4789-A012-345678901234"))
    }

    @Test("ChainID format (sha256:...) is rejected")
    func chainIDFormatRejected() {
        #expect(!Containerd.isEphemeralSnapshotKey("sha256:abc123def456"))
        #expect(!Containerd.isEphemeralSnapshotKey("sha256:1234567890abcdef"))
        #expect(!Containerd.isEphemeralSnapshotKey("sha256:0000000000000000"))
    }

    @Test("Empty string is rejected")
    func emptyStringRejected() {
        #expect(!Containerd.isEphemeralSnapshotKey(""))
    }

    @Test("Invalid UUID formats are rejected")
    func invalidFormatsRejected() {
        // Missing dashes
        #expect(!Containerd.isEphemeralSnapshotKey("550e8400e29b41d4a716446655440000"))

        // Too short
        #expect(!Containerd.isEphemeralSnapshotKey("550e8400-e29b-41d4-a716"))

        // Too long
        #expect(!Containerd.isEphemeralSnapshotKey("550e8400-e29b-41d4-a716-446655440000-extra"))

        // Wrong character
        #expect(!Containerd.isEphemeralSnapshotKey("550e8400-e29b-41d4-a716-44665544000g"))

        // Random string
        #expect(!Containerd.isEphemeralSnapshotKey("not-a-uuid"))
        #expect(!Containerd.isEphemeralSnapshotKey("12345"))
    }

    @Test("Nil UUID components are rejected")
    func nilComponentsRejected() {
        // Missing sections
        #expect(!Containerd.isEphemeralSnapshotKey("550e8400-e29b-41d4"))
        #expect(!Containerd.isEphemeralSnapshotKey("550e8400-e29b"))

        // Extra dashes
        #expect(!Containerd.isEphemeralSnapshotKey("550e8400--e29b-41d4-a716-446655440000"))
    }
}
