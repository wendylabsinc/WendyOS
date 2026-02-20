import Foundation
import Testing
import WendyShared

@Suite("LANDevice TXT Record Parsing")
struct LANDeviceTXTParsingTests {

    // MARK: - New OS format (UUID in "id", no "wendyosdevice")

    @Test("New OS: uses UUID from id field")
    func newOSUsesIdAsUUID() {
        let txt: [String: String] = [
            "id": "7ffb6f7d-0883-47d1-a77b-830f68dd0dda",
            "displayname": "Warm Pepper",
            "name": "warm-pepper",
        ]
        let identity = LANDevice.extractIdentity(from: txt)
        #expect(identity.id == "7ffb6f7d-0883-47d1-a77b-830f68dd0dda")
        #expect(identity.displayName == "Warm Pepper")
    }

    @Test("New OS: falls back to name when displayname is missing")
    func newOSFallsBackToName() {
        let txt: [String: String] = [
            "id": "7ffb6f7d-0883-47d1-a77b-830f68dd0dda",
            "name": "warm-pepper",
        ]
        let identity = LANDevice.extractIdentity(from: txt)
        #expect(identity.id == "7ffb6f7d-0883-47d1-a77b-830f68dd0dda")
        #expect(identity.displayName == "warm-pepper")
    }

    // MARK: - Old OS format (UUID in "wendyosdevice", display string in "id")

    @Test("Old OS: uses wendyosdevice UUID, not the id display string")
    func oldOSUsesWendyosdevice() {
        let txt: [String: String] = [
            "wendyosdevice": "7ffb6f7d-0883-47d1-a77b-830f68dd0dda",
            "id": "WendyOS Device warm-pepper",
            "displayname": "Warm Pepper",
            "name": "warm-pepper",
        ]
        let identity = LANDevice.extractIdentity(from: txt)
        #expect(identity.id == "7ffb6f7d-0883-47d1-a77b-830f68dd0dda")
        #expect(identity.displayName == "Warm Pepper")
    }

    @Test("Old OS: id is non-UUID display string, prefers wendyosdevice")
    func oldOSNonUUIDIdFallsToWendyosdevice() {
        let txt: [String: String] = [
            "wendyosdevice": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
            "id": "WendyOS Device spirited-rocket",
            "displayname": "Spirited Rocket",
        ]
        let identity = LANDevice.extractIdentity(from: txt)
        #expect(identity.id == "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
        #expect(identity.displayName == "Spirited Rocket")
    }

    // MARK: - Edge cases

    @Test("Empty TXT records uses fallback")
    func emptyTXTUsesFallback() {
        let identity = LANDevice.extractIdentity(from: [:])
        #expect(identity.id == "WendyOS Device")
        #expect(identity.displayName == "WendyOS Device")
    }

    @Test("Empty TXT records uses custom fallback")
    func emptyTXTUsesCustomFallback() {
        let identity = LANDevice.extractIdentity(
            from: [:],
            fallbackId: "wendyos-warm-pepper.local"
        )
        #expect(identity.id == "wendyos-warm-pepper.local")
        #expect(identity.displayName == "wendyos-warm-pepper.local")
    }

    @Test("Only wendyosdevice present (old OS, no other keys)")
    func onlyWendyosdevice() {
        let txt: [String: String] = [
            "wendyosdevice": "7ffb6f7d-0883-47d1-a77b-830f68dd0dda"
        ]
        let identity = LANDevice.extractIdentity(from: txt)
        #expect(identity.id == "7ffb6f7d-0883-47d1-a77b-830f68dd0dda")
        // displayName falls back to id since no displayname or name key
        #expect(identity.displayName == "7ffb6f7d-0883-47d1-a77b-830f68dd0dda")
    }

    @Test("Only non-UUID id present (no wendyosdevice)")
    func onlyNonUUIDId() {
        let txt: [String: String] = [
            "id": "WendyOS Device warm-pepper"
        ]
        let identity = LANDevice.extractIdentity(from: txt)
        // Non-UUID id fails UUID check, no wendyosdevice, falls back to raw id
        #expect(identity.id == "WendyOS Device warm-pepper")
        #expect(identity.displayName == "WendyOS Device warm-pepper")
    }

    @Test("Case-insensitive key matching")
    func caseInsensitiveKeys() {
        let txt: [String: String] = [
            "ID": "7ffb6f7d-0883-47d1-a77b-830f68dd0dda",
            "DisplayName": "Warm Pepper",
            "Name": "warm-pepper",
        ]
        let identity = LANDevice.extractIdentity(from: txt)
        #expect(identity.id == "7ffb6f7d-0883-47d1-a77b-830f68dd0dda")
        #expect(identity.displayName == "Warm Pepper")
    }

    @Test("Mixed case keys from different OS versions")
    func mixedCaseKeys() {
        let txt: [String: String] = [
            "WendyOSDevice": "7ffb6f7d-0883-47d1-a77b-830f68dd0dda",
            "displayName": "Warm Pepper",
        ]
        let identity = LANDevice.extractIdentity(from: txt)
        // "wendyosdevice" matches case-insensitively
        #expect(identity.id == "7ffb6f7d-0883-47d1-a77b-830f68dd0dda")
        #expect(identity.displayName == "Warm Pepper")
    }

    @Test("UUID in id takes priority over wendyosdevice")
    func uuidIdTakesPriorityOverWendyosdevice() {
        // New OS might still have wendyosdevice during transition
        let txt: [String: String] = [
            "id": "11111111-2222-3333-4444-555555555555",
            "wendyosdevice": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
            "displayname": "Test Device",
        ]
        let identity = LANDevice.extractIdentity(from: txt)
        // UUID in id should win since it's the new format
        #expect(identity.id == "11111111-2222-3333-4444-555555555555")
        #expect(identity.displayName == "Test Device")
    }

    @Test("displayname preferred over name")
    func displaynamePreferredOverName() {
        let txt: [String: String] = [
            "id": "7ffb6f7d-0883-47d1-a77b-830f68dd0dda",
            "displayname": "Warm Pepper",
            "name": "warm-pepper",
        ]
        let identity = LANDevice.extractIdentity(from: txt)
        #expect(identity.displayName == "Warm Pepper")
    }

    @Test("Uppercase UUID is still valid")
    func uppercaseUUID() {
        let txt: [String: String] = [
            "id": "7FFB6F7D-0883-47D1-A77B-830F68DD0DDA",
            "displayname": "Test Device",
        ]
        let identity = LANDevice.extractIdentity(from: txt)
        #expect(identity.id == "7FFB6F7D-0883-47D1-A77B-830F68DD0DDA")
    }

    @Test("Malformed UUID in id falls back to wendyosdevice")
    func malformedUUIDFallsBack() {
        let txt: [String: String] = [
            "id": "not-a-uuid",
            "wendyosdevice": "7ffb6f7d-0883-47d1-a77b-830f68dd0dda",
            "displayname": "Test Device",
        ]
        let identity = LANDevice.extractIdentity(from: txt)
        #expect(identity.id == "7ffb6f7d-0883-47d1-a77b-830f68dd0dda")
    }
}
