import Foundation

struct MobileCloudNotification: Identifiable, Sendable {
    let id: String
    let date: Date
    let title: String
    let body: String
    var organizationID: String = ""
    var deepLink: String = ""
    var recordingEventID: String = ""
    var recordingCampaign: String = ""
}

private let mobileInboxQueue = DispatchQueue(label: "dev.wendy.ccr.inbox", qos: .utility)

extension GoClient {
    func notification(account: String, reference: MobilePushReference) async throws -> MobileCloudNotification {
        try await BlockingWork.run(on: mobileInboxQueue, unlessCancelled: true) {
            try Self.withCString(account) { account in
                try Self.withCString(reference.organizationID) { organization in
                    try Self.withCString(reference.notificationID) { id in
                        guard let result = WDNotification(account, organization, id) else { throw WendyError(message: "Missing notification response.") }
                        defer { WDInboxFree(result) }
                        try Self.check(result.pointee.status)
                        guard result.pointee.count == 1, let row = result.pointee.items?.pointee else { throw WendyError(message: "This notification is no longer available.") }
                        return MobileCloudNotification(id: Self.string(row.id), date: Date(timeIntervalSince1970: row.created_at),
                            title: Self.string(row.title), body: Self.string(row.body),
                            organizationID: Self.string(row.organization_id), deepLink: Self.string(row.deep_link),
                            recordingEventID: Self.string(row.recording_event_id), recordingCampaign: Self.string(row.recording_campaign))
                    }
                }
            }
        }
    }
}
