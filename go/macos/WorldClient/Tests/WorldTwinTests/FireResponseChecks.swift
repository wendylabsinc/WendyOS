import Foundation
import AppKit
import XCTest
@testable import WorldTwin

@MainActor final class FireResponseChecks: XCTestCase {
    struct Failure: Error { let message: String }
    var checks: [String] = []
    func check(_ value: Bool, _ message: String) throws {
        guard value else { throw Failure(message: message) }
        checks.append(message)
    }
    func write(_ object: Any, _ file: URL) throws {
        try JSONSerialization.data(withJSONObject: object, options: [.sortedKeys]).write(to: file, options: .atomic)
    }
    func testContracts() throws {
        let out = TestOutput.directory("fire")
        try FileManager.default.createDirectory(at: out, withIntermediateDirectories: true)
        _ = NSApplication.shared
        NSApplication.shared.setActivationPolicy(.prohibited)
        let robot: [String: Any] = ["id":"go2", "name":"Go2", "color":[0.1,0.5,0.4], "position":[0.0,0,0.3], "velocity":[0.0,0,0], "online":true, "available":true, "state":"navigating", "job_id":"", "path":[], "lidar_points":[], "local_voxels":0, "peer_voxels":0, "route":[]]
        let network: [String: Any] = ["edges":[], "internet":["id":"none","a":"hub","b":"internet","enabled":false,"up":false,"packets":0], "gossip_rounds":0, "peer_voxels":[:]]
        for mode in ["go2-fire-response", "go2-patrol"] {
            let session = out.appendingPathComponent(mode, isDirectory: true)
            for directory in [session, session.appendingPathComponent("commands"), session.appendingPathComponent("receipts")] {
                try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
            }
            try write(["schema":"wendy.macos-demo.geometry.v1", "run_id":"fire-ui-check", "model_sha256":String(repeating:"a",count:64), "geoms":[]], session.appendingPathComponent("geometry.json"))
            var patrol: [String: Any] = ["mission_id":"observation-mission", "state":"navigating", "expires_at_s":300.0, "nominal_speed_mps":0.55, "rooms":[], "recent_findings":[]]
            var snapshot: [String: Any] = ["run_id":"fire-ui-check", "time_s":10.0, "phase":"responding", "authorized":true, "stopped":false, "robots":[robot], "jobs":[], "completed":0, "total":1, "events":[], "voxels":[], "voxel_size":0.1, "map_node":"go2", "network":network, "patrol":patrol]
            var state: [String: Any] = ["schema":"wendy.macos-demo.state.v1", "run_id":"fire-ui-check", "sequence":1, "mode":mode, "worker_status":"running", "last_command_sequence":0, "snapshot":snapshot, "dynamic_geoms":[]]
            try write(state, session.appendingPathComponent("state.json"))
            let model = DemoModel(session:session, capture:nil)
            model.poll()
            try check(model.patrolMode && model.fireResponseMode == (mode == "go2-fire-response") && model.selectedRobot == "go2", mode+" selects the Go2 control path")
            try check(model.canCommand && model.availableLayers.contains(.hazards) && !model.availableLayers.contains(.links), mode+" retains relevant control and display bounds")
            model.command("start")
            try check(model.pending == nil, mode+" refuses generic start")
            model.command("patrol_resume")
            try check(model.pending == nil, mode+" refuses resume while navigating")
            model.command("patrol_pause")
            try check(model.pending == 1, mode+" sends pause through the existing command path")
            try write(["schema":"wendy.macos-demo.command-result.v1","run_id":"fire-ui-check","sequence":1,"action":"patrol_pause","accepted":true], session.appendingPathComponent("receipts/000001.json"))
            patrol["state"] = "paused"; snapshot["patrol"] = patrol
            state["snapshot"] = snapshot; state["sequence"] = 2; state["last_command_sequence"] = 1
            try write(state, session.appendingPathComponent("state.json")); model.poll()
            model.command("patrol_resume")
            try check(model.pending == 2, mode+" resumes only after the paused acknowledgement")
            model.close()
            try check(!FileManager.default.fileExists(atPath:session.appendingPathComponent("stop").path), mode+" viewer close leaves mission control unchanged")
        }
        let invalid = out.appendingPathComponent("invalid", isDirectory:true)
        try FileManager.default.createDirectory(at:invalid, withIntermediateDirectories:true)
        try write(["schema":"wendy.macos-demo.geometry.v1","run_id":"invalid","model_sha256":String(repeating:"b",count:64),"geoms":[]], invalid.appendingPathComponent("geometry.json"))
        let snapshot: [String:Any] = ["run_id":"invalid","time_s":0.0,"phase":"waiting","authorized":false,"stopped":true,"robots":[robot],"jobs":[],"completed":0,"total":0,"events":[],"voxels":[],"voxel_size":0.1,"map_node":"go2","network":network]
        try write(["schema":"wendy.macos-demo.state.v1","run_id":"invalid","sequence":1,"mode":"go2-fire-response","worker_status":"running","last_command_sequence":0,"snapshot":snapshot,"dynamic_geoms":[]], invalid.appendingPathComponent("state.json"))
        let model = DemoModel(session:invalid,capture:nil); model.poll()
        try check(model.state == nil && !model.canCommand && model.error != nil, "fire response without patrol policy cannot expose controls")
        for variation in ["valid", "authorized", "moving", "controls", "unspecified-controls", "wrong-phase", "ordinary-patrol"] {
            let session = out.appendingPathComponent("waiting-"+variation,isDirectory:true)
            try FileManager.default.createDirectory(at:session.appendingPathComponent("commands"),withIntermediateDirectories:true)
            try write(["schema":"wendy.macos-demo.geometry.v1","run_id":"invalid","model_sha256":String(repeating:"b",count:64),"geoms":[]],session.appendingPathComponent("geometry.json"))
            var waiting=snapshot
            waiting["phase"]="waiting_for_fire";waiting["controls_available"]=false
            if variation == "authorized" {waiting["authorized"]=true}
            if variation == "moving" {waiting["stopped"]=false}
            if variation == "controls" {waiting["controls_available"]=true}
            if variation == "unspecified-controls" {waiting.removeValue(forKey:"controls_available")}
            if variation == "wrong-phase" {waiting["phase"]="navigating"}
            var state:[String:Any] = ["schema":"wendy.macos-demo.state.v1","run_id":"invalid","sequence":1,"mode":variation == "ordinary-patrol" ? "go2-patrol":"go2-fire-response","worker_status":"running","last_command_sequence":0,"snapshot":waiting,"dynamic_geoms":[]]
            try write(state,session.appendingPathComponent("state.json"))
            let viewer=DemoModel(session:session,capture:nil);viewer.poll()
            if variation == "valid" {
                try check(viewer.connected && viewer.error == nil && viewer.fireResponseMode && !viewer.canCommand,"explicit camera waiting state displays read-only")
                for action in ["start","patrol_pause","patrol_resume","patrol_stop","patrol_recall"] {viewer.command(action)}
                try check(viewer.pending == nil && (try FileManager.default.contentsOfDirectory(atPath:session.appendingPathComponent("commands").path)).isEmpty,"waiting camera state emits no commands")
                viewer.close()
                try check(!FileManager.default.fileExists(atPath:session.appendingPathComponent("stop").path),"closing camera waiting view leaves simulation running")
                waiting["patrol"]=["mission_id":"confirmed-cue-mission","state":"navigating","expires_at_s":300.0,"nominal_speed_mps":0.55,"rooms":[],"recent_findings":[]]
                waiting["phase"]="navigating";waiting["controls_available"]=true;waiting["authorized"]=true;waiting["stopped"]=false
                state["sequence"]=2;state["snapshot"]=waiting
                try write(state,session.appendingPathComponent("state.json"));viewer.poll()
                try check(viewer.canCommand && viewer.commandMessage.contains("signed Go2 mission"),"confirmed mission replaces camera waiting state and enables existing controls")
            } else {
                try check(viewer.state == nil && !viewer.canCommand && viewer.error != nil,"missing policy refuses waiting variation: "+variation)
            }
        }
        try write(["status":"passed","count":checks.count,"checks":checks], out.appendingPathComponent("checks.json"))
    }
}
