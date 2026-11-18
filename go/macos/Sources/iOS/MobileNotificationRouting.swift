import Foundation
import Combine

struct MobilePushReference: Equatable, Sendable {
    let notificationID: String
    let organizationID: String

    init?(userInfo: [AnyHashable: Any]) {
        guard let payload = userInfo["wendy"] as? [String: Any],
              let version = payload["version"] as? Int, version == 2,
              let id = payload["notification_id"] as? String,
              let organization = payload["organization_id"] as? String,
              UUID(uuidString: id)?.uuidString.lowercased() == id,
              UUID(uuidString: organization)?.uuidString.lowercased() == organization else { return nil }
        notificationID = id
        organizationID = organization
    }
}

struct MobileNotificationTap: Identifiable, Equatable, Sendable {
    let id = UUID()
    let reference: MobilePushReference?
    let malformedCloudReference: Bool

    init(userInfo: [AnyHashable: Any]) {
        reference = MobilePushReference(userInfo: userInfo)
        malformedCloudReference = userInfo["wendy"] != nil && reference == nil
    }
}

struct MobileNotificationDestination: Equatable, Sendable {
    let deviceID: String
    let cameraID: String?
    let recordingEventID: String?
    let recordingCampaign: String?

    init(notification: MobileCloudNotification, reference: MobilePushReference) throws {
        guard notification.id == reference.notificationID, notification.organizationID == reference.organizationID else {
            throw WendyError(message: "This notification belongs to another organization.")
        }
        guard let url = URLComponents(string: notification.deepLink), url.scheme == "wendy",
              url.host == "organizations", url.user == nil, url.password == nil, url.port == nil, url.fragment == nil else {
            throw WendyError(message: "This notification does not have a supported device link.")
        }
        let path = url.path.split(separator: "/").map(String.init)
        guard path.count == 4, UUID(uuidString: path[0])?.uuidString.lowercased() == reference.organizationID,
              path[1] == "devices", path[3] == "live", let device = UUID(uuidString: path[2]) else {
            throw WendyError(message: "This notification does not have a supported device link.")
        }
        let query = url.queryItems ?? []
        guard query.count <= 1, query.allSatisfy({ $0.name == "camera" && $0.value?.isEmpty == false }) else {
            throw WendyError(message: "This notification has an invalid camera link.")
        }
        deviceID = device.uuidString.lowercased()
        cameraID = query.first?.value
        if notification.recordingEventID.isEmpty && notification.recordingCampaign.isEmpty {
            recordingEventID = nil
            recordingCampaign = nil
        } else {
            guard UUID(uuidString: notification.recordingEventID)?.uuidString.lowercased() == notification.recordingEventID,
                  notification.recordingEventID == notification.id,
                  !notification.recordingCampaign.isEmpty, notification.recordingCampaign.count <= 128 else {
                throw WendyError(message: "This notification has an invalid recording reference.")
            }
            recordingEventID = notification.recordingEventID
            recordingCampaign = notification.recordingCampaign
        }
    }
}

@MainActor
final class MobileNotificationRouting: ObservableObject {
    static let shared = MobileNotificationRouting()
    @Published private(set) var pending: MobileNotificationTap?

    func receive(_ tap: MobileNotificationTap) { pending = tap }
    func finish(_ tap: MobileNotificationTap) {
        if pending?.id == tap.id { pending = nil }
    }
}

struct MobileNotificationPresentation: Identifiable {
    let id = UUID()
    let device: Device
    var episode: MobileRecordingEpisode?
    var cameraID: String?
}

// Cancellable, bounded polling: an alert can arrive before its clip is sealed.
// Keep playback in our existing viewer, including audio, ROS and mixed data.
enum MobileNotificationRecording {
    static func find(device: Device, campaign: String, eventID: String,
                     attempts: Int = 10,
                     read: @Sendable (Device, String, String) async throws -> MobileRecordingEpisode? = {
                         try await GoClient.shared.notificationRecording(device: $0, campaign: $1, eventID: $2)
                     },
                     pause: @Sendable () async throws -> Void = { try await Task.sleep(for: .seconds(2)) }) async throws -> MobileRecordingEpisode {
        let deadline = Date().addingTimeInterval(30)
        for attempt in 0..<attempts {
            try Task.checkCancellation()
            if let episode = try await read(device, campaign, eventID) {
                try Task.checkCancellation()
                return episode
            }
            if attempt + 1 < attempts && Date() < deadline { try await pause() } else { break }
        }
        throw WendyError(message: "This recording is still being saved or is no longer available. Tap the alert again in a moment, or check Data.")
    }
}

// These messages are authored by the app, rather than returned by a device.
// Preserve their actionable guidance while sanitizing all other failures.
enum MobileNotificationMessage {
    static func display(_ failure: WendyError) -> String {
        let messages: Set<String> = [
            "Switch to this alert’s organization in Settings to open it.",
            "This alert has an invalid reference.",
            "This alert’s device is no longer available in this organization.",
            "This notification is no longer available.",
            "This recording is still being saved or is no longer available. Tap the alert again in a moment, or check Data."
        ]
        return messages.contains(failure.message) ? failure.message : PresentedFailure(failure).message
    }
}
