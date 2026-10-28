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


// Preview exports deliberately identify their scope. Full episode export keeps
// all raw files; CSV and PLY contain only data loaded by the bounded viewer.
enum DataVisualExport {
    static func csv(_ samples: [DataVisualSample]) -> String {
        func quote(_ value: String) -> String { "\"" + value.replacingOccurrences(of: "\"", with: "\"\"") + "\"" }
        var rows = ["episode_nanoseconds,track,value,unit"]
        for sample in samples {
            guard let value = sample.value, value.isFinite else { continue }
            rows.append("\(sample.timestamp),\(quote(sample.track)),\(value),\(quote(sample.unit))")
        }
        return rows.joined(separator: "\r\n") + "\r\n"
    }
    static func ply(_ sample: DataVisualSample) -> String {
        var rows = ["ply", "format ascii 1.0", "comment Wendy preview, sensor frame, meters", "comment episode_nanoseconds \(sample.timestamp)", "element vertex \(sample.points.count)", "property float x", "property float y", "property float z", "end_header"]
        rows += sample.points.map { "\($0.x) \($0.y) \($0.z)" }
        return rows.joined(separator: "\n") + "\n"
    }
}
