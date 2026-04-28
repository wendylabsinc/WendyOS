import FlyingFox
import FlyingSocks
import Foundation

final class WebServer {
    private let server: HTTPServer
    private let state: AppState
    private let runStore: RunStore
    private let indexHTML: String

    init(port: UInt16, state: AppState, runStore: RunStore, indexHTML: String) throws {
        self.state = state
        self.runStore = runStore
        self.indexHTML = indexHTML
        self.server = HTTPServer(address: try sockaddr_in.inet(ip4: "0.0.0.0", port: port))
    }

    func start() async throws {
        await registerRoutes()
        try await server.run()
    }

    func waitUntilListening() async throws {
        try await server.waitUntilListening()
    }

    private func registerRoutes() async {
        await server.appendRoute("GET /") { [indexHTML] _ in
            htmlResponse(indexHTML)
        }

        await server.appendRoute("GET /api/state") { [state] _ in
            try jsonResponse(await state.snapshot())
        }

        await server.appendRoute("POST /api/prompt") { [state] request in
            let data = try await request.bodyData
            let update = try AppJSON.decoder.decode(PromptUpdateRequest.self, from: data)
            return try jsonResponse(await state.savePrompt(update.text))
        }

        await server.appendRoute("GET /api/runs") { [runStore] request in
            let limit = max(1, min(Int(request.query["limit"] ?? "4") ?? 4, 50))
            let response = try runStore.listRuns(limit: limit, before: request.query["before"])
            return try jsonResponse(response)
        }

        await server.appendRoute("GET /api/runs/:id") { [runStore] (id: String) in
            let run = try runStore.loadRun(id: id)
            return try jsonResponse(run)
        }

        await server.appendRoute("GET /frame.jpg") { [state] _ in
            guard let data = await state.liveFrameJPEG() else {
                return textResponse("Live frame not available yet.", status: .notFound)
            }
            return dataResponse(data, contentType: "image/jpeg")
        }

        await server.appendRoute(
            "GET,HEAD /artifacts/*",
            to: DirectoryHTTPHandler(root: runStore.rootURL, serverPath: "/artifacts")
        )
    }
}

private func htmlResponse(_ html: String) -> HTTPResponse {
    dataResponse(Data(html.utf8), contentType: "text/html; charset=utf-8")
}

private func textResponse(_ text: String, status: HTTPStatusCode = .ok) -> HTTPResponse {
    HTTPResponse(
        statusCode: status,
        headers: [
            .contentType: "text/plain; charset=utf-8",
            HTTPHeader("Cache-Control"): "no-store"
        ],
        body: Data(text.utf8)
    )
}

private func dataResponse(
    _ data: Data,
    contentType: String,
    status: HTTPStatusCode = .ok,
    cacheControl: String = "no-store"
) -> HTTPResponse {
    HTTPResponse(
        statusCode: status,
        headers: [
            .contentType: contentType,
            HTTPHeader("Cache-Control"): cacheControl
        ],
        body: data
    )
}

private func jsonResponse<T: Encodable>(_ value: T, status: HTTPStatusCode = .ok) throws -> HTTPResponse {
    let data = try AppJSON.encoder.encode(value)
    return dataResponse(data, contentType: "application/json; charset=utf-8", status: status)
}
