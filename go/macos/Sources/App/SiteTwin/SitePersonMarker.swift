@preconcurrency import SceneKit
import SwiftUI

/// A critically damped position response. Retargeting keeps both position and
/// velocity continuous, even when camera observations arrive unevenly.
struct SitePersonMotion {
    private var origin: SIMD3<Double>
    private var velocity = SIMD3<Double>.zero
    private var target: SIMD3<Double>
    private var startedAt: TimeInterval
    private static let response = 14.0

    init(position: SIMD3<Double>, at time: TimeInterval) {
        origin = position; target = position; startedAt = time
    }

    func sample(at time: TimeInterval) -> (position: SIMD3<Double>, velocity: SIMD3<Double>) {
        let t = max(0, time - startedAt)
        let offset = origin - target
        let coefficient = velocity + Self.response * offset
        let decay = exp(-Self.response * t)
        return (target + (offset + coefficient * t) * decay,
                (velocity - Self.response * coefficient * t) * decay)
    }

    mutating func retarget(_ position: SIMD3<Double>, at time: TimeInterval) {
        let current = sample(at: time)
        origin = current.position; velocity = current.velocity
        target = position; startedAt = time
    }
}

@MainActor final class SitePersonMarker {
    let node = SCNNode()
    private var motion: SitePersonMotion
    private var observedAt: Date
    private var reduceMotion: Bool

    init(person: SitePersonTrack, now: Date, reduceMotion: Bool) {
        observedAt = person.observedAt
        self.reduceMotion = reduceMotion
        motion = SitePersonMotion(position: person.position, at: now.timeIntervalSinceReferenceDate)
        node.name = person.id.uuidString
        let body = SCNNode(geometry: SCNCapsule(capRadius: 0.19, height: 0.85))
        body.position.y = 0.6
        let head = SCNNode(geometry: SCNSphere(radius: 0.15))
        head.position.y = 1.18
        let halo = SCNNode(geometry: SCNCylinder(radius: 0.32, height: 0.025))
        halo.position.y = 0.04
        #if os(macOS)
        let accent = NSColor.systemGreen
        #else
        let accent = UIColor.systemGreen
        #endif
        for part in [body, head, halo] {
            part.categoryBitMask = 1 << 30
            part.geometry?.firstMaterial?.diffuse.contents = accent
            part.geometry?.firstMaterial?.lightingModel = .constant
            part.opacity = part === halo ? 0.35 : 0.8
            node.addChildNode(part)
        }
        // Spawn at the observation, never animate from the scene origin.
        node.simdPosition = SIMD3<Float>(person.position)
        scheduleFade(now: now)
    }

    func update(_ person: SitePersonTrack, now: Date, reduceMotion: Bool) {
        guard person.observedAt > observedAt || self.reduceMotion != reduceMotion else { return }
        let time = now.timeIntervalSinceReferenceDate
        if reduceMotion {
            motion = SitePersonMotion(position: person.position, at: time)
            node.removeAnimation(forKey: "person.position")
        } else {
            motion.retarget(person.position, at: time)
            // SceneKit interpolates these samples on its render clock. The
            // workspace's 4 Hz refresh only supplies new observations.
            let animation = CAKeyframeAnimation(keyPath: "position")
            animation.duration = 1
            animation.calculationMode = .linear
            animation.values = (0...60).map { frame in
                let p = motion.sample(at: time + Double(frame) / 60).position
                return NSValue(scnVector3: SCNVector3(p.x, p.y, p.z))
            }
            node.addAnimation(animation, forKey: "person.position")
        }
        SCNTransaction.begin()
        SCNTransaction.disableActions = true
        node.simdPosition = SIMD3<Float>(person.position)
        SCNTransaction.commit()
        observedAt = person.observedAt
        self.reduceMotion = reduceMotion
        scheduleFade(now: now)
    }

    private func scheduleFade(now: Date) {
        let age = max(0, now.timeIntervalSince(observedAt))
        let remaining = max(0.001, SitePeopleTracker.lifetime - age)
        let hold = max(0, 0.65 - age)
        let opacity = min(1, max(0, (SitePeopleTracker.lifetime - age) / (SitePeopleTracker.lifetime - 0.65)))
        let animation = CAKeyframeAnimation(keyPath: "opacity")
        animation.duration = remaining
        animation.values = [opacity, opacity, 0]
        animation.keyTimes = [0, NSNumber(value: min(1, hold / remaining)), 1]
        animation.calculationMode = .linear
        SCNTransaction.begin()
        SCNTransaction.disableActions = true
        node.opacity = 0
        node.addAnimation(animation, forKey: "person.freshness")
        SCNTransaction.commit()
    }
}
