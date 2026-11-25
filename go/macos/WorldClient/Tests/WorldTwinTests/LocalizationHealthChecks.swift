import Foundation
import AppKit
import XCTest
@testable import WorldTwin

@MainActor final class LocalizationHealthChecks: XCTestCase {
    struct Failure: Error { let message: String }
    var checks: [String] = []
    func check(_ value: Bool, _ message: String) throws {
        guard value else { throw Failure(message: message) }; checks.append(message)
    }
    func decode(_ value: [String:Any]) throws -> PatrolState.LocalizationHealth {
        try JSONDecoder().decode(PatrolState.LocalizationHealth.self,from:JSONSerialization.data(withJSONObject:value))
    }
    func write(_ object: Any, _ file: URL) throws {
        try JSONSerialization.data(withJSONObject:object,options:[.sortedKeys]).write(to:file,options:.atomic)
    }
    func testContracts() throws {
        let out = TestOutput.directory("localization")
        try FileManager.default.createDirectory(at:out,withIntermediateDirectories:true)
        let passive: [String:Any] = ["attempts":4,"pending":false,"last_scan_stamp":12.5,"last_attempt_age_s":1.2]
        let healthy: [String:Any] = ["schema":"wendy.nav.localization-health.v1","ready":true,"reasons":[],"sensor_age_s":0.1,"pose_age_s":0.2,"costmap_age_s":0.3,"variance":[0.001,0.002,0.003],"variance_limit":0.0225,"passive":passive,"motion_resumed":false]
        let decoded = try decode(healthy)
        try check(decoded.ready && !decoded.motion_resumed,"healthy diagnostics decode without motion authority")
        try check(decoded.summary(held:true) == "Observations recovered. Resume remains explicit.","held recovery requires explicit Resume copy")
        try check(decoded.summary(held:false) == "Navigation observations ready.","navigating health does not claim mission recovery")
        try check(decoded.passiveSummary.contains("4") && decoded.passiveSummary.contains("idle"),"actual passive retry count appears")
        for reason in PatrolState.LocalizationHealth.reasonText.keys.sorted() {
            var value=healthy; value["ready"]=false; value["reasons"]=[reason]
            let health=try decode(value)
            try check(health.summary(held:true).contains(PatrolState.LocalizationHealth.reasonText[reason]!),"precise diagnostic: "+reason)
        }
        var uncertain=healthy; uncertain["ready"]=false;uncertain["reasons"]=["covariance_high"];uncertain["variance"]=[0.001,0.002,0.1]
        try check(try decode(uncertain).summary(held:true).contains("uncertainty exceeds"),"high yaw uncertainty is not mislabeled as stale sensors")
        var unknown=healthy;unknown["ready"]=false;unknown["reasons"]=["sensor_stale","pose_stale","costmap_stale","covariance_invalid"]
        for key in ["sensor_age_s","pose_age_s","costmap_age_s"] {unknown[key]=NSNull()}
        unknown["variance"]=[];unknown["passive"]=["attempts":0,"pending":false,"last_scan_stamp":NSNull(),"last_attempt_age_s":NSNull()]
        try check(try decode(unknown).observationAges.contains("unknown"),"unknown ages remain unknown rather than zero")
        var invalidVariance=healthy;invalidVariance["ready"]=false;invalidVariance["reasons"]=["covariance_invalid"];invalidVariance["variance"]=[-0.1,0.1,0.1]
        try check(try decode(invalidVariance).variance[0] == -0.1,"negative covariance remains visible only as invalid diagnostics")
        var pending=healthy;var pendingValue=passive;pendingValue["pending"]=true;pending["passive"]=pendingValue
        try check(try decode(pending).passiveSummary.contains("awaiting response"),"in-flight observation request is visible")
        let invalid: [(String,Any)] = [("schema","wrong"),("ready",false),("reasons",["unknown"]),("reasons",["pose_stale","pose_stale"]),("sensor_age_s",-1.0),("pose_age_s",-1.0),("costmap_age_s",-1.0),("sensor_age_s",0.6),("pose_age_s",3.0),("costmap_age_s",3.0),("sensor_age_s",NSNull()),("variance",[]),("variance",[0.001,0.001]),("variance",[-0.01,0.001,0.001]),("variance",[0.0225,0.001,0.001]),("variance_limit",0.03),("motion_resumed",true),("sensor_age_s",1e13)]
        for (index,item) in invalid.enumerated() {
            var value=healthy;value[item.0]=item.1
            try check((try? decode(value)) == nil,"reject malformed health \(index): \(item.0)")
        }
        for key in ["sensor_age_s","pose_age_s","costmap_age_s","passive","variance","motion_resumed"] {
            var value=healthy;value.removeValue(forKey:key)
            try check((try? decode(value)) == nil,"reject missing contract field: "+key)
        }
        for (key,value) in [("attempts",-1 as Any),("attempts",1.5 as Any),("attempts",1_000_000_001 as Any),("last_scan_stamp",-1.0 as Any),("last_attempt_age_s",-1.0 as Any)] {
            var health=healthy;var retry=passive;retry[key]=value;health["passive"]=retry
            try check((try? decode(health)) == nil,"reject malformed passive field: "+key)
        }
        var health=healthy;health["passive"]=["attempts":0,"pending":true,"last_scan_stamp":NSNull(),"last_attempt_age_s":NSNull()]
        try check((try? decode(health)) == nil,"pending requires a recorded attempt")
        let nonfiniteDecoder=JSONDecoder()
        nonfiniteDecoder.nonConformingFloatDecodingStrategy = .convertFromString(positiveInfinity:"Infinity",negativeInfinity:"-Infinity",nan:"NaN")
        for key in ["sensor_age_s","pose_age_s","costmap_age_s","variance_limit"] {
            var value=healthy;value[key]="NaN"
            let data=try JSONSerialization.data(withJSONObject:value)
            try check((try? nonfiniteDecoder.decode(PatrolState.LocalizationHealth.self,from:data)) == nil,"reject nonfinite field: "+key)
        }
        var nonfinite=healthy;nonfinite["variance"]=["NaN",0.1,0.1]
        try check((try? nonfiniteDecoder.decode(PatrolState.LocalizationHealth.self,from:JSONSerialization.data(withJSONObject:nonfinite))) == nil,"reject nonfinite variance")
        var patrol: [String:Any] = ["mission_id":"health-check","state":"paused","expires_at_s":300.0,"nominal_speed_mps":0.55,"rooms":[],"recent_findings":[]]
        let old = try JSONDecoder().decode(PatrolState.self,from:JSONSerialization.data(withJSONObject:patrol))
        try check(old.valid && old.localization_health == nil,"older snapshots without health remain compatible")
        _ = NSApplication.shared; NSApplication.shared.setActivationPolicy(.prohibited)
        let session=out.appendingPathComponent("session",isDirectory:true)
        for path in [session,session.appendingPathComponent("commands"),session.appendingPathComponent("receipts")] {try FileManager.default.createDirectory(at:path,withIntermediateDirectories:true)}
        try write(["schema":"wendy.macos-demo.geometry.v1","run_id":"health-check","model_sha256":String(repeating:"a",count:64),"geoms":[]],session.appendingPathComponent("geometry.json"))
        let network: [String:Any] = ["edges":[],"internet":["id":"none","a":"hub","b":"internet","enabled":false,"up":false,"packets":0],"gossip_rounds":0,"peer_voxels":[:]]
        var snapshot: [String:Any] = ["run_id":"health-check","time_s":10.0,"phase":"paused","authorized":true,"stopped":true,"robots":[],"jobs":[],"completed":0,"total":1,"events":[],"voxels":[],"voxel_size":0.1,"map_node":"go2","network":network]
        var state: [String:Any] = ["schema":"wendy.macos-demo.state.v1","run_id":"health-check","sequence":1,"mode":"go2-fire-response","worker_status":"running","last_command_sequence":0,"dynamic_geoms":[]]
        patrol["localization_health"]=uncertain;snapshot["patrol"]=patrol;state["snapshot"]=snapshot
        try write(state,session.appendingPathComponent("state.json"))
        let model=DemoModel(session:session,capture:nil);model.poll()
        try check(model.connected && model.snapshot?.patrol?.localization_health?.ready == false,"native model displays uncertainty hold")
        patrol["localization_health"]=healthy;snapshot["patrol"]=patrol;state["snapshot"]=snapshot;state["sequence"]=2
        try write(state,session.appendingPathComponent("state.json"));model.poll()
        try check(model.snapshot?.patrol?.state == "paused" && model.snapshot?.patrol?.localization_health?.ready == true,"observation recovery leaves mission paused")
        try check(model.pending == nil && (try FileManager.default.contentsOfDirectory(atPath:session.appendingPathComponent("commands").path)).isEmpty,"health transition emits no automatic command")
        model.command("patrol_resume")
        try check(model.pending == 1,"explicit Resume still uses existing command path")
        try write(["status":"passed","count":checks.count,"checks":checks],out.appendingPathComponent("checks.json"))
        print("Passed \(checks.count) localization health checks")
    }
}
