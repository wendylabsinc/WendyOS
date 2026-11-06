import Foundation
import GRPCCore
import Hummingbird
import HummingbirdTesting
import Testing
@testable import WendyNotify

@Test func `form preserves unicode and repeated roles without validating fields`() throws {
    let form = NotificationForm(encoded: "title=%3Cscript%3E%26%F0%9F%8C%B1&body=line+one%0Aline+two&roles=1&roles=3&userIDs=first%0D%0Asecond&notificationID=bad-id&severity=4&deepLink=bad-link")
    let message = try form.message()
    #expect(message.title == "<script>&🌱")
    #expect(message.body == "line one\nline two")
    #expect(message.audience.userIds == ["first", "second"])
    #expect(message.audience.roles == [.owner, .billingManager])
    #expect(message.notificationID == "bad-id")
    #expect(message.deepLink == "bad-link")
}

@Test func `metadata supports nested JSON and preserves parser errors`() throws {
    var form = NotificationForm()
    form.metadata = "{\"nested\":{\"list\":[true,null,3,\"text\"]}}"
    #expect(try form.message().metadata.fields["nested"]?.structValue.fields["list"]?.listValue.values.count == 4)
    form.metadata = "{broken"
    #expect(throws: (any Error).self) { try form.message() }
}

@Test func `rate panel uses Cloud retry time and never quarantines local refusals`() async throws {
    let history = SendHistory()
    let now = Date(timeIntervalSince1970: 1_800_000_000)
    let local = RPCError(code: .resourceExhausted, message: "notification rate exceeded; retry later with the same notification_id")
    for _ in 0..<3 {
        await history.record(.failure(local, notificationID: "id", context: ""), title: "test", now: now)
    }
    var snapshot = await history.snapshot(now: now)
    #expect(snapshot.limits.quarantineUntil == nil)
    #expect(snapshot.limits.retryAt == nil)
    #expect(snapshot.limits.refused)
    let end = now.addingTimeInterval(900)
    let cloud = RPCError(code: .resourceExhausted, message: "device app Notification source is temporarily quarantined; retry after \(end.ISO8601Format())")
    await history.record(.failure(cloud, notificationID: "id", context: ""), title: "test", now: now)
    snapshot = await history.snapshot(now: now)
    #expect(snapshot.limits.quarantineUntil == end)
    #expect(await history.snapshot(now: end).limits.quarantineUntil == nil)
}

@Test func `successful sends consume estimated budget and expire as one window`() async {
    let history = SendHistory()
    let now = Date(timeIntervalSince1970: 1_800_000_000)
    for index in 0..<12 {
        let result = SendOutcome(notificationID: "\(index)", code: "Created", message: "Created", error: nil)
        await history.record(result, title: "test", now: now.addingTimeInterval(Double(index)))
    }
    let snapshot = await history.snapshot(now: now.addingTimeInterval(15))
    #expect(snapshot.records.count == 10)
    #expect(snapshot.records.first?.outcome.notificationID == "11")
    #expect(snapshot.limits.remaining == 0)
    #expect(snapshot.limits.resetAt == now.addingTimeInterval(60))
    #expect(await history.snapshot(now: now.addingTimeInterval(60)).limits.remaining == 10)
}

@Test func `raw RPC message remains unchanged and HTML cannot execute it`() async {
    let text = "<script>alert('x')</script> & \"cause\""
    let error = RPCError(code: .permissionDenied, message: text)
    let outcome = SendOutcome.failure(error, notificationID: "id", context: "unused")
    #expect(outcome.message == text)
    var form = NotificationForm()
    form.title = text
    let snapshot = await SendHistory().snapshot()
    let html = renderPage(form: form, outcome: outcome, records: [], limits: snapshot.limits)
    #expect(!html.contains(text))
    #expect(html.contains("&lt;script&gt;"))
    #expect(html.contains("novalidate"))
}

@Test func `missing and absent sockets explain the entitlement`() async {
    let missing = await NotificationSender(socketPath: nil).send(.init())
    #expect(missing.message.contains("WENDY_SYSTEM_SOCKET is missing"))
    #expect(missing.message.contains("notifications entitlement"))
    let absent = await NotificationSender(socketPath: "/not/a/wendy/socket").send(.init())
    #expect(absent.message.contains("/not/a/wendy/socket"))
}

@Test func `page loads and submissions show specific errors without login`() async throws {
    let app = Application(router: makeRouter(sender: .init(socketPath: nil)))
    try await app.test(.router) { client in
        try await client.execute(uri: "/", method: .get) { response in
            #expect(response.status == .ok)
            #expect(String(buffer: response.body).contains("Compose notification"))
            #expect(response.headers[.init("Referrer-Policy")!] == "no-referrer")
            #expect(response.headers[.init("X-Frame-Options")!] == "DENY")
            #expect(response.headers[.cacheControl] == "no-store")
        }
        try await client.execute(uri: "/", method: .post, body: .init(string: "title=test")) { response in
            #expect(response.status == .ok)
            #expect(String(buffer: response.body).contains("WENDY_SYSTEM_SOCKET is missing"))
        }
    }
}
