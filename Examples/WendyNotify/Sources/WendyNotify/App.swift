import Hummingbird

@main
struct WendyNotify {
    static func main() async throws {
        let history = SendHistory()
        let sender = NotificationSender()
        let router = makeRouter(sender: sender, history: history)
        let app = Application(router: router, configuration: .init(address: .hostname("0.0.0.0", port: 8080)))
        try await app.runService()
    }
}

func makeRouter(sender: NotificationSender = .init(), history: SendHistory = .init()) -> Router<BasicRequestContext> {
    let router = Router()
    // SECURITY: This example intentionally has no login, as explicitly requested.
    // Anyone who can reach this port can send as the app; real apps add auth.
    router.get("/") { _, _ -> Response in
        let snapshot = await history.snapshot()
        return htmlResponse(renderPage(form: .init(), outcome: nil, records: snapshot.records,
                                       limits: snapshot.limits))
    }
    router.post("/") { request, context -> Response in
        let body = try await request.body.collect(upTo: 128 * 1024)
        let encoded = String(buffer: body)
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
                                       limits: snapshot.limits))
    }
    return router
}

func htmlResponse(_ html: String, status: HTTPResponse.Status = .ok) -> Response {
    Response(status: status, headers: [.contentType: "text/html; charset=utf-8", .cacheControl: "no-store",
             .init("Referrer-Policy")!: "no-referrer", .init("X-Frame-Options")!: "DENY",
             .init("X-Content-Type-Options")!: "nosniff"],
             body: .init(byteBuffer: .init(string: html)))
}
