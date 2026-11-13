import Foundation
import GRPCCore

struct SendRecord: Sendable {
    let time: Date
    let title: String
    let outcome: SendOutcome
}

struct LimitSnapshot: Sendable {
    let remaining: Int
    let resetAt: Date?
    let retryAt: Date?
    let quarantineUntil: Date?
    let refused: Bool
}

actor SendHistory {
    private var records: [SendRecord] = []
    private var windowStart: Date?
    private var accepted = 0
    private var retryAt: Date?
    private var quarantineUntil: Date?
    private var lastRateRefusal: Date?

    func record(_ outcome: SendOutcome, title: String, now: Date = Date()) {
        expireWindow(now: now)
        records.insert(.init(time: now, title: title, outcome: outcome), at: 0)
        records = Array(records.prefix(10))
        if outcome.created {
            if windowStart == nil { windowStart = now }
            accepted += 1
            lastRateRefusal = nil
        } else if let error = outcome.error as? RPCError, error.code == .resourceExhausted {
            lastRateRefusal = now
            // Cloud reports these timestamps in the exact message; the Agent's
            // local burst refusal does not. Never turn a local refusal into quarantine.
            if let date = Self.reportedRetryDate(error.message) {
                if error.message.contains("temporarily quarantined") {
                    quarantineUntil = date
                } else if error.message.contains("Notifications per minute") {
                    retryAt = date
                }
            }
        }
    }

    func snapshot(now: Date = Date()) -> (records: [SendRecord], limits: LimitSnapshot) {
        expireWindow(now: now)
        return (records, .init(
            remaining: (retryAt.map { $0 > now } == true || quarantineUntil.map { $0 > now } == true)
                ? 0 : max(0, 10 - accepted),
            resetAt: windowStart?.addingTimeInterval(60),
            retryAt: retryAt.flatMap { $0 > now ? $0 : nil },
            quarantineUntil: quarantineUntil.flatMap { $0 > now ? $0 : nil },
            refused: lastRateRefusal.map { now.timeIntervalSince($0) < 60 } ?? false
        ))
    }

    static func reportedRetryDate(_ message: String) -> Date? {
        guard let range = message.range(of: "; retry after ") else { return nil }
        return try? Date(String(message[range.upperBound...]), strategy: .iso8601)
    }

    private func expireWindow(now: Date) {
        if let start = windowStart, now.timeIntervalSince(start) >= 60 {
            windowStart = nil
            accepted = 0
        }
    }
}
