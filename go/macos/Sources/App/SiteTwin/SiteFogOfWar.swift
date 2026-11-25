import Foundation

/// Display-only coverage. These cells never enter the navigation planner.
struct SiteFogGrid {
    let minX, minZ, cellSize: Double
    let width, height: Int
    private(set) var explored: Set<Int> = []
    private(set) var visible: Set<Int> = []
    private var lastOrigin: SitePoint?

    init?(minX: Double, maxX: Double, minZ: Double, maxZ: Double) {
        guard [minX, maxX, minZ, maxZ].allSatisfy({ $0.isFinite && abs($0) <= 1_000_000 }),
              maxX > minX, maxZ > minZ else { return nil }
        self.minX = minX; self.minZ = minZ
        cellSize = max(0.25, max(maxX-minX, maxZ-minZ) / 128)
        width = min(128, Int(ceil((maxX-minX) / cellSize)))
        height = min(128, Int(ceil((maxZ-minZ) / cellSize)))
    }

    var exploredArea: Double { Double(explored.count) * cellSize * cellSize }

    /// Quantize updates to 10 cm, not heading or joint changes. Visibility is a
    /// 2.5 m circle clipped by imported geometry at Go2 camera height.
    @discardableResult
    mutating func observe(_ position: SitePoint?, fresh: Bool,
                         canSee: (SitePoint, SitePoint) -> Bool) -> Bool {
        guard fresh, let position, position.isValid,
              position.x >= minX, position.x < minX + Double(width)*cellSize,
              position.z >= minZ, position.z < minZ + Double(height)*cellSize else {
            let changed = !visible.isEmpty
            visible = []; lastOrigin = nil
            return changed
        }
        if let lastOrigin, hypot(position.x-lastOrigin.x, position.z-lastOrigin.z) < 0.1,
           abs(position.y-lastOrigin.y) < 0.1 { return false }
        lastOrigin = position
        let radius = 2.5
        let left = max(0, Int(floor((position.x-radius-minX)/cellSize)))
        let right = min(width-1, Int(floor((position.x+radius-minX)/cellSize)))
        let top = max(0, Int(floor((position.z-radius-minZ)/cellSize)))
        let bottom = min(height-1, Int(floor((position.z+radius-minZ)/cellSize)))
        let origin = SitePoint(x: position.x, y: position.y+0.35, z: position.z)
        var next: Set<Int> = []
        for row in top...bottom {
            for column in left...right {
                let target = SitePoint(x: minX+(Double(column)+0.5)*cellSize,
                                       y: origin.y, z: minZ+(Double(row)+0.5)*cellSize)
                if hypot(target.x-origin.x, target.z-origin.z) <= radius, canSee(origin, target) {
                    next.insert(row*width+column)
                }
            }
        }
        let changed = next != visible
        visible = next
        explored.formUnion(next)
        return changed
    }

    /// Red is explored, green is currently visible. Texture rows follow +Z.
    var pixels: [UInt8] {
        var bytes = [UInt8](repeating: 0, count: width*height*4)
        for cell in 0..<(width*height) {
            bytes[cell*4] = explored.contains(cell) ? 255 : 0
            bytes[cell*4+1] = visible.contains(cell) ? 255 : 0
            bytes[cell*4+3] = 255
        }
        return bytes
    }
}

public struct SiteFogConfiguration: Equatable {
    var enabled = false
    var reset = UUID()
    var context = ""

    public init() {}
}

public struct SiteFogSummary: Equatable {
    var exploredArea = 0.0
    var receiving = false
    var available = true

    public init() {}
}
