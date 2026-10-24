import Foundation

enum DataVisualSearch {
    static func upperBound<T>(_ values: [T], at time: Double, time timestamp: (T) -> Double) -> Int {
        var low = 0, high = values.count
        while low < high {
            let mid = low + (high - low) / 2
            if timestamp(values[mid]) <= time { low = mid + 1 } else { high = mid }
        }
        return low
    }
    static func latest<T>(_ values: [T], at time: Double, time timestamp: (T) -> Double) -> T? {
        let end = upperBound(values, at: time, time: timestamp)
        return end == 0 ? nil : values[end - 1]
    }
}

struct DataVisualSeekState {
    private(set) var desired: Double?
    private(set) var inFlight: Double?
    private(set) var completed: Double?
    mutating func request(_ time: Double) { if time.isFinite { desired = time } }
    mutating func begin() -> Double? {
        guard inFlight == nil, let desired, desired != completed else { return nil }
        inFlight = desired
        return desired
    }
    mutating func finish(success: Bool) {
        if success { completed = inFlight }
        inFlight = nil
    }
}

