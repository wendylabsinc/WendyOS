import Foundation
@preconcurrency import NetworkExtension

enum FlowByteBridge {
    /// flow → device: returns nil at EOF (zero-length read).
    static func read(_ flow: NEAppProxyTCPFlow) -> @Sendable () async throws -> Data? {
        { @Sendable in
            try await withCheckedThrowingContinuation { cont in
                flow.readData { data, error in
                    if let error {
                        cont.resume(throwing: error)
                    } else if let data, !data.isEmpty {
                        cont.resume(returning: data)
                    } else {
                        cont.resume(returning: nil)
                    }  // zero-length == EOF
                }
            }
        }
    }

    /// device → flow.
    static func write(_ flow: NEAppProxyTCPFlow) -> @Sendable (Data) async throws -> Void {
        { @Sendable data in
            try await withCheckedThrowingContinuation {
                (cont: CheckedContinuation<Void, any Error>) in
                flow.write(data) { error in
                    if let error { cont.resume(throwing: error) } else { cont.resume() }
                }
            }
        }
    }
}
