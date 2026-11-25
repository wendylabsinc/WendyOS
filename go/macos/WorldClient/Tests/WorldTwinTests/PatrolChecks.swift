import Foundation
import CryptoKit
import AppKit
import SceneKit
import SwiftUI
import XCTest
@testable import WorldTwin

struct PatrolCheckFailure: Error { let message:String }
@MainActor final class PatrolChecks: XCTestCase {
    func require(_ condition:Bool,_ message:String) throws {
        if !condition {throw PatrolCheckFailure(message:message)}
    }
    func write(_ object:Any,_ file:URL) throws {
        try JSONSerialization.data(withJSONObject:object,options:[.sortedKeys]).write(to:file,options:.atomic)
    }
    func testContracts() throws {
        let out=TestOutput.directory("patrol")
        do {
            _=NSApplication.shared;NSApplication.shared.setActivationPolicy(.prohibited)
            let session=out.appendingPathComponent("fixture")
            for name in ["fixture","fixture/commands","fixture/receipts"] {
                try FileManager.default.createDirectory(at:out.appendingPathComponent(name),withIntermediateDirectories:true)
            }
            let offlinePath=out.appendingPathComponent("offline-kits.json")
            let offlineNow=Date();let offlineMS=Int64(offlineNow.timeIntervalSince1970*1000)
            let offlineID="kit-0123456789abcdef01234567"
            var offlineEntry:[String:Any]=["profile":["id":offlineID,"name":"Approved office patrol","run_id":"patrol-"+offlineID,"package_sha256":String(repeating:"a",count:64),"model_sha256":String(repeating:"b",count:64),"prepared_unix_ms":offlineMS-1000,"expires_unix_ms":offlineMS+60000,"frame":["id":"studio","revision":"v1"]],"kit_sha256":String(repeating:"c",count:64),"state":"ready","bytes":1024,"file_count":6,"policy":["rooms":["studio"],"duration_seconds":60,"observation_policy":"observe-and-report","dwell_seconds":2,"no_progress_seconds":15,"defer_seconds":30]]
            try write([offlineEntry],offlinePath)
            let offlineKit=try PatrolOfflineEntry.load(offlinePath)[0]
            try require(offlineKit.activatable(at:offlineNow),"original unexpired kit can be selected")
            var unreviewed=false;do {_=try offlineKit.selection(at:offlineNow,reviewed:false)}catch{unreviewed=true}
            try require(unreviewed,"activation requires explicit policy review")
            let selected=try JSONSerialization.jsonObject(with:offlineKit.selection(at:offlineNow,reviewed:true)) as! [String:String]
            try require(selected==["action":"activate","id":offlineID],"chooser selects exact original approval without editing it")
            try require(!offlineKit.activatable(at:offlineNow.addingTimeInterval(61)) && offlineKit.label(at:offlineNow.addingTimeInterval(61))=="Expired","native activation expires while chooser is open")
            let offlineSelection=out.appendingPathComponent("offline-selected.json")
            let offlineModel=PatrolOfflineModel(inputURL:offlinePath,outputURL:offlineSelection)
            offlineModel.select(offlineID)
            try require(!offlineModel.activate(),"native model cannot save before explicit review")
            offlineModel.reviewed=true
            try require(offlineModel.activate(),"native activation writes the reviewed exact kit")
            let selectedBytes=try Data(contentsOf:offlineSelection)
            try require(!offlineModel.activate(),"native selection cannot overwrite an earlier choice")
            let unchangedBytes=try Data(contentsOf:offlineSelection)
            try require(selectedBytes==unchangedBytes,"failed duplicate action preserves original selection")
            offlineEntry["state"]="consumed";try write([offlineEntry],offlinePath)
            let consumed=try PatrolOfflineEntry.load(offlinePath)[0]
            try require(!consumed.activatable(at:offlineNow),"consumed approval cannot activate")
            offlineEntry["state"]="unavailable";offlineEntry["reason"]="kit file changed";offlineEntry.removeValue(forKey:"policy")
            try write([offlineEntry],offlinePath)
            let unavailable=PatrolOfflineModel(inputURL:offlinePath,outputURL:out.appendingPathComponent("unavailable-choice.json"))
            unavailable.select(offlineID);unavailable.reviewed=true
            try require(unavailable.entries.count==1 && !unavailable.activate(),"unavailable catalog entry remains inspectable but cannot activate through callback")
            try write([offlineEntry,offlineEntry],offlinePath)
            var duplicateKit=false;do {_=try PatrolOfflineEntry.load(offlinePath)}catch{duplicateKit=true}
            try require(duplicateKit,"ambiguous kit selection refused")
            let identity=[1.0,0,0,0,1,0,0,0,1]
            let mesh:[String:Any]=["id":1,"name":"thigh","type":"mesh","size":[0.1,0.1,0.2],"rgba":[0.1,0.2,0.2,1],
                "position":[0.0,0,0.3],"rotation":identity,"dynamic":true,"group":0,"robot_id":"go2",
                "vertices":[[0.0,0,0],[0.1,0,0],[0.0,0,0.2]],"faces":[[0,1,2]]]
            let geometry:[String:Any]=["schema":"wendy.macos-demo.geometry.v1","run_id":"patrol-test","model_sha256":String(repeating:"a",count:64),"geoms":[mesh]]
            try write(geometry,session.appendingPathComponent("geometry.json"))
            let robot:[String:Any]=["id":"go2","name":"Go2","color":[0.1,0.5,0.4],"position":[0.0,0,0.3],"velocity":[0.0,0,0],"online":true,"available":true,"state":"patrolling","job_id":"","path":[[2.0,2,0],[3.0,2,0]],"lidar_points":[[1.0,0,0.4]],"local_voxels":0,"peer_voxels":0,"route":[]]
            let room:[String:Any]=["id":"studio","name":"Studio","polygon":[[-2.0,-2],[2.0,-2],[2.0,2],[-2.0,2]],"visits":1,"last_observed_s":8.0,"evidence":["sha256:fixture"]]
            let scheduleInput=out.appendingPathComponent("schedule-review.json")
            try write(["rooms":[["id":"studio","name":"Studio"]],"revisit_seconds":["studio":30]],scheduleInput)
            let review=try PatrolScheduleReviewInput.load(scheduleInput)
            let edited=try review.intervals(["studio":"90"])
            try require(edited["studio"]==90,"operator edits room revisit interval")
            for invalid in [["studio":"9"],["studio":"86401"],["studio":"1.5"],["unknown":"30"]] {
                var refused=false
                do {_=try review.intervals(invalid)} catch {refused=true}
                try require(refused,"review rejects invalid interval or room identity")
            }
            try write(["rooms":[["id":"studio","name":"Studio"]],"revisit_seconds":["other":30]],scheduleInput)
            var badReview=false
            do {_=try PatrolScheduleReviewInput.load(scheduleInput)} catch {badReview=true}
            try require(badReview,"review refuses schedule for different room set")
            try write(["rooms":[["id":"studio","name":"Studio"]],"duration_seconds":600,"rendezvous_options":[["id":"start","name":"Start","x":-6.0,"y":0.0,"yaw":0.2]]],scheduleInput)
            let returnReview=try PatrolScheduleReviewInput.load(scheduleInput)
            let returnPolicy=try returnReview.returnPolicy(id:"start",before:"90")
            try require(returnPolicy.position==[-6,0.3,0] && returnPolicy.yaw==0.2 && returnPolicy.before_expiry_seconds==90,"review binds selected map point and window")
            for pair in [("unknown","90"),("start","600"),("start","-1")] {
                var refused=false;do {_=try returnReview.returnPolicy(id:pair.0,before:pair.1)}catch{refused=true}
                try require(refused,"invalid return point/window refused")
            }
            let tourInput=out.appendingPathComponent("tour-input.json")
            let tourOutput=out.appendingPathComponent("tour-selected.json")
            try write(["rooms":[["id":"studio","name":"Studio"],["id":"lounge","name":"Lounge"]],"duration_seconds":600,"rendezvous_options":[["id":"start","name":"Start","x":-6.0,"y":0.0,"yaw":0.2],["id":"studio","name":"Studio","x":-5.0,"y":1.0,"yaw":0.0],["id":"lounge","name":"Lounge","x":-4.0,"y":2.0,"yaw":0.1]]],tourInput)
            try require(!PatrolTourPolicy.Stop(room_id:"studio",position:[1,1.5,1],yaw:0).valid,"planar adapter refuses unsupported signed observation height")
            let tourModel=PatrolTourReviewModel(inputURL:tourInput,outputURL:tourOutput)
            try require(!tourModel.approve() && !tourModel.select("unknown"),"tour requires selected commissioned rooms")
            try require(tourModel.select("studio") && tourModel.select("lounge") && tourModel.move("lounge",by:-1),"operator chooses and reorders tour")
            try require(tourModel.policy.stops.map(\.room_id)==["lounge","studio"] && tourModel.policy.stops[0].position==[-4,0.3,-2],"review preserves fixed ordered world positions")
            tourModel.reviewed=true
            try require(!tourModel.approve(),"review alone cannot skip complete route preview")
            tourModel.legs=Array(repeating:PatrolTourReviewModel.Leg(reachable:true,length:1,route:[[0,0],[1,1]],reason:nil),count:3)
            try require(tourModel.move("studio",by:-1) && !tourModel.reviewed && tourModel.legs.isEmpty,"changing order invalidates preview and review")
            tourModel.legs=Array(repeating:PatrolTourReviewModel.Leg(reachable:true,length:1,route:[[0,0],[1,1]],reason:nil),count:3)
            tourModel.reviewed=true
            try require(tourModel.approve(),"explicit reviewed tour writes configuration")
            let tourDocument=try JSONSerialization.jsonObject(with:Data(contentsOf:tourOutput)) as! [String:Any]
            let signedTour=try JSONDecoder().decode(PatrolTourPolicy.self,from:JSONSerialization.data(withJSONObject:tourDocument["tour"]!))
            try require(signedTour.stops.map(\.room_id)==["studio","lounge"] && tourDocument["revisit_seconds"]==nil && tourDocument["return"] != nil,"finite tour retains order and required return without continuous cadence")
            try require(!tourModel.approve(),"tour approval cannot overwrite earlier selection")
            let workPolicy=try PatrolWorkPolicy.parse(limit:"6",reserve:"4",hasReturn:true)
            try require(workPolicy.limit_j==6000 && workPolicy.return_reserve_j==4000,"operator kJ selection binds exact joules")
            for args in [("0","0",true),("6","6",true),("6","4",false),("nan","0",true),("6","-1",true)] {
                var refused=false;do {_=try PatrolWorkPolicy.parse(limit:args.0,reserve:args.1,hasReturn:args.2)} catch {refused=true}
                try require(refused,"invalid work allowance or reserve refused")
            }
            let environmentPolicy=try PatrolEnvironmentPolicy.parse(hold:"80",clear:"65",seconds:"2")
            try require(environmentPolicy.clear_for_ms==2000 && environmentPolicy.hold_ppm==80,"environment controls retain signed units and interval")
            for args in [("65","65","2"),("80","65","0"),("80","65","31"),("nan","65","2"),("80","-1","2")] {
                var refused=false;do {_=try PatrolEnvironmentPolicy.parse(hold:args.0,clear:args.1,seconds:args.2)} catch {refused=true}
                try require(refused,"invalid environmental thresholds or dwell refused")
            }
            var localization:[String:Any]=["position":[2.0,2,0],"yaw":0.0,"covariance":[0.04,0,0,0.01],"valid":true]
            var patrol:[String:Any]=["mission_id":"patrol-1","state":"navigating","current_room":"studio","expires_at_s":3600.0,"nominal_speed_mps":0.55,"rooms":[room],"recent_findings":[],"localization":localization]
            let workSample:[String:Any]=["run_id":"patrol-test","boot_id":"boot","model_sha256":String(repeating:"a",count:64),"method":"post-step-positive-joint-power-v1","captured_unix_ms":1000,"simulation_ns":1_000_000_000,"sequence":500,"total_j":125.0,"power_w":30.0]
            let work:[String:Any]=["policy":["limit_j":6000.0,"return_reserve_j":4000.0],"baseline":workSample,"last":workSample,"used_j":125.0,"remaining_j":5875.0,"valid":true,"permanent":false,"exhausted":false]
            patrol["work"]=work
            var snapshot:[String:Any]=["run_id":"patrol-test","time_s":10.0,"phase":"patrol","authorized":true,"stopped":false,"robots":[robot],"jobs":[],"completed":1,"total":1,"events":[],"voxels":[],"voxel_size":0.1,"map_node":"go2","network":["edges":[],"internet":["id":"none","a":"hub","b":"internet","enabled":false,"up":false,"packets":0],"gossip_rounds":0,"peer_voxels":[:]],"patrol":patrol]
            var state:[String:Any]=["schema":"wendy.macos-demo.state.v1","run_id":"patrol-test","sequence":1,"mode":"go2-patrol","worker_status":"running","last_command_sequence":0,"snapshot":snapshot,"dynamic_geoms":[["id":1,"position":[0.5,0,0.3],"rotation":identity]]]
            try write(state,session.appendingPathComponent("state.json"))
            let model=DemoModel(session:session,capture:nil)
            let renderer=SceneRenderer();model.renderer=renderer;model.poll();renderer.update(model)
            try require(model.snapshot?.patrol?.work?.remaining_j==5875,"native meter preserves measured remaining work")
            try require(model.patrolMode && model.selectedRobot=="go2","patrol mode selects only robot")
            try require(model.canCommand && model.availableLayers.contains(.coverage) && model.availableLayers.contains(.localization),"patrol controls and layers")
            try require(!model.availableLayers.contains(.links),"patrol hides unrelated mesh controls")
            let part=renderer.world.childNodes.first {$0.name=="robot:go2"}
            try require(part?.childNodes.first?.geometry?.sources(for:.vertex).first?.vectorCount==3,"compiled triangle mesh rendered")
            try require(abs(Double(part?.position.x ?? 0)-0.5)<0.00001,"physics transform animates actual mesh")
            try require(renderer.world.childNode(withName:"estimated-navigation-pose",recursively:true) != nil,"separate estimate displayed")
            try require(renderer.diagnostics().first {$0.name=="Room coverage"}?.child_nodes==1,"coverage geometry present")
            model.setCamera(.robot);renderer.update(model);renderer.followInput(horizontal:50,zoom:-20)
            let distance=renderer.cameraDiagnostics().distance,yaw=renderer.cameraDiagnostics().yaw
            state["sequence"]=2;state["dynamic_geoms"]=[["id":1,"position":[0.7,0,0.3],"rotation":identity]]
            try write(state,session.appendingPathComponent("state.json"));model.poll();renderer.update(model)
            try require(abs(Double(part?.position.x ?? 0)-0.7)<0.00001,"joint geometry accepts next physics sample")
            try require(renderer.cameraDiagnostics().distance==distance && renderer.cameraDiagnostics().yaw==yaw,"follow zoom and yaw persist")
            try require(abs(renderer.camera.simdWorldTransform.columns.0.y)<0.00001,"follow horizon remains level")
            model.setLayer(.localization,enabled:false);renderer.update(model)
            try require(renderer.diagnostics().first {$0.name=="Localization"}?.hidden==true,"localization toggle affects renderer")
            model.command("start");try require(model.pending==nil,"legacy start cannot dispatch in patrol")
            model.command("patrol_resume");try require(model.pending==nil,"resume refused unless paused")
            model.command("patrol_pause");try require(model.pending==1,"pause sends numbered command")
            let command=try JSONSerialization.jsonObject(with:Data(contentsOf:session.appendingPathComponent("commands/000001.json"))) as! [String:Any]
            try require(command["action"] as? String=="patrol_pause","pause action contract")
            patrol["state"]="paused";snapshot["patrol"]=patrol;state["snapshot"]=snapshot;state["sequence"]=3;state["last_command_sequence"]=1
            try write(["schema":"wendy.macos-demo.command-result.v1","run_id":"patrol-test","sequence":1,"action":"patrol_pause","accepted":true],session.appendingPathComponent("receipts/000001.json"))
            try write(state,session.appendingPathComponent("state.json"));model.poll()
            try require(model.pending==nil && model.snapshot?.patrol?.rooms.first?.visits==1,"pause acknowledges and retains progress")
            model.command("patrol_resume");try require(model.pending==2,"resume sends after paused acknowledgement")
            model.pending=nil
            model.command("patrol_stop");try require(model.pending==3,"stop sends terminal mission action")
            model.pending=nil;patrol["state"]="expired";snapshot["patrol"]=patrol;state["snapshot"]=snapshot
            model.state=try JSONDecoder().decode(DemoState.self,from:JSONSerialization.data(withJSONObject:state))
            model.command("patrol_resume");try require(model.pending==nil,"expired mission cannot resume")
            patrol["state"]="navigating";patrol["return"]=["destination":["x":-6.0,"y":0.0,"yaw":0.2],"before_expiry_seconds":90,"requested":false]
            snapshot["patrol"]=patrol;state["snapshot"]=snapshot
            model.state=try JSONDecoder().decode(DemoState.self,from:JSONSerialization.data(withJSONObject:state))
            model.command("patrol_recall");try require(model.pending != nil,"recall available only with approved point")
            model.pending=nil;patrol["state"]="return_held"
            patrol["return"]=["destination":["x":-6.0,"y":0.0,"yaw":0.2],"before_expiry_seconds":90,"requested":true]
            snapshot["patrol"]=patrol;state["snapshot"]=snapshot
            model.state=try JSONDecoder().decode(DemoState.self,from:JSONSerialization.data(withJSONObject:state))
            model.command("patrol_recall");try require(model.pending==nil,"duplicate recall refused")
            model.command("patrol_resume");try require(model.pending != nil,"held return supports explicit retry")
            model.pending=nil
            let worldContract:[String:Any]=["tenant":"simulation","field":["site":"studio-01","frame":["id":"office-floor","revision":"v1"],"simulation":["run_id":"patrol-test"],"min_xz":[-7,-3],"max_xz":[-1,3],"sensors":Array(repeating:["position_xz":[-6,0]],count:4)],"gateways":["east":[:],"west":[:]]]
            let worldPolicy:[String:Any]=["method":"synthetic-source-region-outside-watch-v1","contract":worldContract,"contract_sha256":String(repeating:"a",count:64),"reader_principal":"reader","reader_audience":"node-3","min_xz":[-4.5,-1],"max_xz":[-2,1],"max_age_ms":4000]
            let decodedWorld=try JSONDecoder().decode(PatrolWorldPolicy.self,from:JSONSerialization.data(withJSONObject:worldPolicy))
            try require(decodedWorld.valid,"commissioned World policy decodes")
            var editedWorld=decodedWorld;editedWorld.min_xz=[-4,-0.5]
            let encodedWorld=try JSONDecoder().decode(PatrolWorldPolicy.self,from:JSONSerialization.data(withJSONObject:editedWorld.document()))
            try require(encodedWorld.contract==decodedWorld.contract && encodedWorld.contract_sha256==decodedWorld.contract_sha256 && encodedWorld.min_xz==[-4,-0.5],"rectangle editing preserves the commissioned contract")
            var worldStatus:[String:Any]=["policy":worldPolicy,"ready":false,"held":true,"permanent":false,"reason":"World evidence expired","transport":"unavailable","remaining_ms":0,"original_unix_ms":1000,"selection":[:]]
            patrol["world_watch"]=worldStatus;patrol["state"]="world_held";snapshot["patrol"]=patrol;state["snapshot"]=snapshot
            model.state=try JSONDecoder().decode(DemoState.self,from:JSONSerialization.data(withJSONObject:state))
            try require(model.snapshot?.patrol?.valid==true,"World hold remains a valid explicit state")
            model.command("patrol_resume");try require(model.pending==nil,"missing World evidence refuses native Resume")
            model.command("patrol_recall");try require(model.pending==nil,"World hold refuses native recall")
            worldStatus["ready"]=true;worldStatus["remaining_ms"]=2000;worldStatus["transport"]="connected";patrol["world_watch"]=worldStatus;snapshot["patrol"]=patrol;state["snapshot"]=snapshot
            model.state=try JSONDecoder().decode(DemoState.self,from:JSONSerialization.data(withJSONObject:state));model.command("patrol_resume")
            try require(model.pending != nil,"compatible World evidence enables explicit native Resume callback")
            model.pending=nil;patrol.removeValue(forKey:"world_watch")
            let environmentSample:[String:Any]=["sequence":10,"simulation_ns":1_000_000_000,"captured_unix_ms":1000,"value_ppm":60.0,"uncertainty_ppm":1.5,"valid":true]
            var environmental:[String:Any]=["policy":try JSONSerialization.jsonObject(with:JSONEncoder().encode(environmentPolicy)),"last":environmentSample,"screening_ppm":61.5,"clear_elapsed_ms":1000,"ready":false,"held":true,"permanent":false,"reason":"Collecting original clear samples"]
            patrol["environment"]=environmental;patrol["state"]="environment_held";snapshot["patrol"]=patrol;state["snapshot"]=snapshot
            model.state=try JSONDecoder().decode(DemoState.self,from:JSONSerialization.data(withJSONObject:state))
            try require(model.snapshot?.patrol?.valid==true,"environmental ledger renders without changing policy")
            model.command("patrol_resume");try require(model.pending==nil,"uncleared environmental hold cannot request Resume")
            model.command("patrol_recall");try require(model.pending==nil,"environmental hold cannot request return")
            environmental["ready"]=true;environmental["clear_elapsed_ms"]=2000;patrol["environment"]=environmental;snapshot["patrol"]=patrol;state["snapshot"]=snapshot
            model.state=try JSONDecoder().decode(DemoState.self,from:JSONSerialization.data(withJSONObject:state))
            model.command("patrol_resume");try require(model.pending != nil,"clear original interval enables an explicit Resume callback")
            model.pending=nil;patrol.removeValue(forKey:"environment")
            model.stale=true;model.command("patrol_stop");try require(model.pending==nil,"stale controls refused")
            var scheduledRoom=room;scheduledRoom["revisit_seconds"]=30;scheduledRoom["next_due_s"]=8.0
            patrol["rooms"]=[scheduledRoom]
            let scheduled=try JSONDecoder().decode(PatrolState.self,from:JSONSerialization.data(withJSONObject:patrol))
            try require(scheduled.valid && scheduled.rooms[0].next_due_s==8 && scheduled.rooms[0].last_observed_s==8,"deadline and observation fields retained independently")
            scheduledRoom["revisit_seconds"]=9;patrol["rooms"]=[scheduledRoom]
            let badSchedule=try JSONDecoder().decode(PatrolState.self,from:JSONSerialization.data(withJSONObject:patrol))
            try require(!badSchedule.valid,"native display refuses invalid schedule")
            patrol["rooms"]=[room]
            let restricted:[String:Any]=["frame":["id":"office","revision":"1"],"revision":1,"rectangles":[["id":"area","min_mm":[1000,1000],"max_mm":[2000,2000]]],"sha256":String(repeating:"a",count:64)]
            patrol["exclusions"]=restricted;snapshot["patrol"]=patrol;state["snapshot"]=snapshot
            model.state=try JSONDecoder().decode(DemoState.self,from:JSONSerialization.data(withJSONObject:state));renderer.update(model)
            try require(model.snapshot?.patrol?.exclusions?.valid==true,"signed restriction display retains frame and geometry")
            try require(renderer.world.childNode(withName:"restricted-area:area",recursively:true) != nil,"restricted region appears in actual SceneKit twin")
            var malformed=restricted;malformed["rectangles"]=[["id":"area","min_mm":[1000],"max_mm":[2000,2000]]]
            patrol["exclusions"]=malformed
            let badPolicy=try JSONDecoder().decode(PatrolState.self,from:JSONSerialization.data(withJSONObject:patrol))
            try require(!badPolicy.valid,"malformed restricted area refused by native display")
            patrol["exclusions"]=restricted
            model.close();try require(!FileManager.default.fileExists(atPath:session.appendingPathComponent("stop").path),"closing patrol viewer does not stop worker")
            localization["covariance"]=[-1.0,0,0,1];patrol["localization"]=localization
            let invalid=try JSONDecoder().decode(PatrolState.self,from:JSONSerialization.data(withJSONObject:patrol))
            try require(!invalid.valid,"invalid covariance refused")
            state.removeValue(forKey:"mode");snapshot.removeValue(forKey:"patrol");state["snapshot"]=snapshot
            let old=try JSONDecoder().decode(DemoState.self,from:JSONSerialization.data(withJSONObject:state))
            model.state=old;try require(!model.patrolMode && old.snapshot.patrol==nil,"old bridge compatibility")
            model.close();try require(FileManager.default.fileExists(atPath:session.appendingPathComponent("stop").path),"legacy viewer retains close behavior")
            let evidence=session.appendingPathComponent("evidence")
            try FileManager.default.createDirectory(at:evidence,withIntermediateDirectories:true)
            let original=Data("{\"sensor\":\"gas\",\"value_ppm\":18}".utf8)
            let digest=SHA256.hash(data:original).map {String(format:"%02x",$0)}.joined()
            let evidencePath=evidence.appendingPathComponent(digest+".json")
            try original.write(to:evidencePath)
            let verified=try PatrolEvidenceFile.load(digest,session:session)
            try require(verified.data==original && verified.format=="json","original evidence digest verified")
            try Data("tampered".utf8).write(to:evidencePath)
            var rejected=false
            do {_=try PatrolEvidenceFile.load(digest,session:session)} catch {rejected=true}
            try require(rejected,"tampered evidence refused")
            try FileManager.default.removeItem(at:evidencePath)
            let elsewhere=out.appendingPathComponent("original.json");try original.write(to:elsewhere)
            try FileManager.default.createSymbolicLink(at:evidencePath,withDestinationURL:elsewhere)
            rejected=false
            do {_=try PatrolEvidenceFile.load(digest,session:session)} catch {rejected=true}
            try require(rejected,"symlink evidence refused")
            rejected=false
            do {_=try PatrolEvidenceFile.load("../original",session:session)} catch {rejected=true}
            try require(rejected,"non-digest evidence reference refused")
            try write(["status":"passed","scope":"Actual SceneKit and native model with synthetic test inputs; no live simulator or OS input"],out.appendingPathComponent("result.json"))
        } catch { XCTFail(String(describing: error)) }
    }
}
