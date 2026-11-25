import Foundation
@preconcurrency import SceneKit
import Testing
@testable import WendyDesktop

@Suite struct SitePersonMotionTests {
    @Test func retargetingPreservesPositionAndVelocity() {
        var motion = SitePersonMotion(position: SIMD3(3, 0, 2), at: 0)
        motion.retarget(SIMD3(4, 0, 2), at: 0)
        let before = motion.sample(at: 0.13)
        #expect(before.position.x > 3 && before.position.x < 4)
        #expect(before.velocity.x > 0)
        motion.retarget(SIMD3(3.5, 0, 3), at: 0.13)
        let after = motion.sample(at: 0.13)
        #expect(simd_distance(before.position, after.position) < 1e-10)
        #expect(simd_distance(before.velocity, after.velocity) < 1e-10)
        let settled = motion.sample(at: 2)
        #expect(simd_distance(settled.position, SIMD3(3.5, 0, 3)) < 0.001)
        #expect(simd_length(settled.velocity) < 0.001)
    }

    @Test func stopsAtTheLastObservationWithoutExtrapolating() {
        var motion = SitePersonMotion(position: .zero, at: 0)
        motion.retarget(SIMD3(1, 0, 0), at: 0)
        var previous = 0.0
        for frame in 0...120 {
            let sample = motion.sample(at: Double(frame) / 60)
            #expect(sample.position.x >= previous)
            #expect(sample.position.x <= 1)
            #expect(sample.position.y == 0 && sample.position.z == 0)
            previous = sample.position.x
        }
        #expect(abs(previous - 1) < 0.001)
    }
}

@Suite @MainActor struct SitePersonMarkerTests {
    @Test func spawnsInPlaceAndRefreshDoesNotRestartAnimations() throws {
        let now = Date()
        let person = SitePersonTrack(id: UUID(), position: SIMD3(4, 0, -3), observedAt: now)
        let marker = SitePersonMarker(person: person, now: now, reduceMotion: false)
        #expect(marker.node.simdPosition == SIMD3<Float>(person.position))
        #expect(marker.node.animationPlayer(forKey: "person.position") == nil)
        let fade = try #require(marker.node.animationPlayer(forKey: "person.freshness"))
        marker.update(person, now: now.addingTimeInterval(0.25), reduceMotion: false)
        #expect(marker.node.animationPlayer(forKey: "person.freshness") === fade)
        #expect(marker.node.animationPlayer(forKey: "person.position") == nil)

        var moved = person
        moved.observedAt = now.addingTimeInterval(0.3)
        moved.position.x += 0.3
        marker.update(moved, now: moved.observedAt, reduceMotion: false)
        let movement = try #require(marker.node.animationPlayer(forKey: "person.position"))
        marker.update(moved, now: now.addingTimeInterval(0.5), reduceMotion: false)
        #expect(marker.node.animationPlayer(forKey: "person.position") === movement)
    }

    @Test func reduceMotionSnapsWithoutLeavingAnOldAnimationRunning() {
        let now = Date()
        var person = SitePersonTrack(id: UUID(), position: .zero, observedAt: now)
        let marker = SitePersonMarker(person: person, now: now, reduceMotion: false)
        person.position.x = 1
        person.observedAt = now.addingTimeInterval(0.3)
        marker.update(person, now: person.observedAt, reduceMotion: false)
        #expect(marker.node.animationPlayer(forKey: "person.position") != nil)
        marker.update(person, now: person.observedAt, reduceMotion: true)
        #expect(marker.node.animationPlayer(forKey: "person.position") == nil)
        #expect(marker.node.simdPosition.x == 1)
    }
}
