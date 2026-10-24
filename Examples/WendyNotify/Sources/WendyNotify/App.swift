import Foundation
import Hummingbird

@main
struct WendyNotify {
    static func main() async throws {
        let history = SendHistory()
        let sender = NotificationSender()
        let accessKey = UUID().uuidString
        let router = makeRouter(accessKey: accessKey, sender: sender, history: history)
        let app = Application(router: router, configuration: .init(address: .hostname("0.0.0.0", port: 8080)))
        // This local capability is intentionally printed only for the developer
        // who can read the entitled app's logs. It changes on every restart.
        app.logger.notice("Open WendyNotify at http://<device>:8080/?key=\(accessKey)")
        try await app.runService()
    }
}

func makeRouter(accessKey: String, sender: NotificationSender = .init(), history: SendHistory = .init()) -> Router<BasicRequestContext> {
    let router = Router()
    router.get("/") { request, _ -> Response in
        guard request.uri.queryParameters.get("key") == accessKey else {
            return htmlResponse("<!doctype html><title>Access key required</title><p>Open WendyNotify using the private link in the app startup log.</p>", status: .unauthorized)
        }
        let snapshot = await history.snapshot()
        return htmlResponse(renderPage(form: .init(), outcome: nil, records: snapshot.records,
                                       limits: snapshot.limits, accessKey: accessKey))
    }
    router.post("/") { request, context -> Response in
        let body = try await request.body.collect(upTo: 128 * 1024)
        let encoded = String(buffer: body)
        // The same per-run secret protects reads and sends. Anonymous GETs
        // never reveal it, and cross-site forms cannot obtain it.
        let submittedToken = encoded.split(separator: "&").first { $0.hasPrefix("accessKey=") }
            .map { String($0.dropFirst("accessKey=".count)) }
        guard submittedToken == accessKey else {
            return htmlResponse("<!doctype html><title>Send refused</title><p>Access key is missing or expired. Open the private link from the app startup log again.</p>", status: .forbidden)
        }
        let form = NotificationForm(encoded: encoded)
        let outcome = await sender.send(form)
        if let error = outcome.error {
            context.logger.error("Notification send failed", metadata: [
                "code": "\(outcome.code)", "message": "\(outcome.message)", "cause": "\(String(describing: error))"
            ])
        }
        await history.record(outcome, title: form.title)
        let snapshot = await history.snapshot()
        return htmlResponse(renderPage(form: form, outcome: outcome, records: snapshot.records,
                                       limits: snapshot.limits, accessKey: accessKey))
    }
    return router
}

func htmlResponse(_ html: String, status: HTTPResponse.Status = .ok) -> Response {
    Response(status: status, headers: [.contentType: "text/html; charset=utf-8", .cacheControl: "no-store",
             .init("Referrer-Policy")!: "no-referrer", .init("X-Frame-Options")!: "DENY",
             .init("X-Content-Type-Options")!: "nosniff"],
             body: .init(byteBuffer: .init(string: html)))
}
