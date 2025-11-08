import GRPCCore
import GRPCNIOTransportHTTP2
import Logging

struct WendyErrorInterceptor: ServerInterceptor {
    let logger = Logger(label: "sh.wendy.agent.error-interceptor")
    func intercept<Input: Sendable, Output: Sendable>(
        request: StreamingServerRequest<Input>,
        context: ServerContext,
        next:
            @Sendable (
                _ request: StreamingServerRequest<Input>,
                _ context: ServerContext
            ) async throws -> StreamingServerResponse<Output>
    ) async throws -> StreamingServerResponse<Output> {
        do {
            var response = try await next(request, context)
            switch response.accepted {
            case .success(let success):
                response.accepted = .success(
                    .init(
                        metadata: success.metadata
                    ) { writer in
                        do {
                            return try await success.producer(writer)
                        } catch {
                            logger.error(
                                "Failed to handle request",
                                metadata: [
                                    "error": "\(error)"
                                ]
                            )
                            throw RPCError(
                                code: .internalError,
                                message: "Failed to handle request: \(error)"
                            )
                        }
                    }
                )
            case .failure(let error):
                logger.error(
                    "Failed to handle request",
                    metadata: [
                        "error": "\(error)"
                    ]
                )
            }
            return response
        } catch {
            logger.error("Failed to handle request", metadata: ["error": "\(error)"])
            throw RPCError(code: .internalError, message: "Failed to handle request: \(error)")
        }
    }
}
