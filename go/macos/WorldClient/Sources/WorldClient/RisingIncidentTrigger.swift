import Foundation

// Historical operator review context. Exact bytes and original samples are
// retained; the publishing gateway attests the inference, not physical truth.
enum RisingIncidentTriggerWire {
    static let schema = "wendy.rising-incident.inspection.v1"
    static func shape(_ value: JSONValue, _ keys: [String], optional: [String] = []) throws {
        guard let object=value.object, Set(keys).isSubset(of:Set(object.keys)), Set(object.keys).isSubset(of:Set(keys+optional)), object.values.allSatisfy({$0 != .null}) else {throw MissionError.invalidDraft}
    }
    static func nanos(_ value:JSONValue?)throws->Int64 {try MissionTimestamp.nanos(value)}
    static func time(_ value:JSONValue?)throws->Date {try MissionTimestamp.time(value)}
    static func deadlineNanos(_ value:JSONValue)throws->Int64 {try MissionTimestamp.deadlineNanos(value)}
    static func number(_ value: JSONValue?) throws -> Double {
        guard let n=value?.double,n.isFinite else {throw MissionError.invalidDraft};return n
    }
    static func payload(_ value: JSONValue) throws -> Data {
        let data = try NativeTriggerWire.object(value, required:["schema","joined_sha256","joined_json","temporal_sha256","temporal_json","spatial_json","valid_until","temporal_codec"]) { _,v in try MissionWire.value(v) }
        guard data.count<=20<<10 else{throw MissionError.invalidDraft};return data
    }
    static func component(_ value: JSONValue, outer: JSONValue, originalTransitAge:Bool=false) throws {
        try shape(value,["key","version","frame","timing","ttl_ns","provenance","payload","tombstone"])
        guard value["tombstone"]?.bool==false,value["key"]?["tenant"]==outer["key"]?["tenant"],value["key"]?["site"]==outer["key"]?["site"],value["frame"]==outer["frame"],value["timing"]?["clock_domain"]==outer["timing"]?["clock_domain"],value["timing"]?["error_known"]?.bool==true,value["timing"]?["error_bound_ns"]?.int==0,let age=value["timing"]?["age_hint_ns"]?.int,let ttl=value["ttl_ns"]?.int,ttl>0,ttl<=10_000_000_000,age>=0,age<ttl,(originalTransitAge || age==0) else {throw MissionError.invalidDraft}
        _ = try NativeTriggerWire.key(value["key"]!);_ = try NativeTriggerWire.version(value["version"]!)
        try shape(value["frame"]!,["id","revision"]);try shape(value["timing"]!,["sample_time","clock_domain","error_known","error_bound_ns","age_hint_ns"]);_ = (try time(value["timing"]?["sample_time"]))
    }
    static func until(_ value: JSONValue) throws -> Date {(try time(value["timing"]?["sample_time"])).addingTimeInterval((try number(value["ttl_ns"]))/1e9)}
    static func review(_ c:JSONValue,fixedTarget:[Double],destination:MissionInspectionDestination?) throws -> (schema:String,target:[Double]) {
        guard let p=c["payload"],p["schema"]?.string==schema,c["provenance"]?["app_version"]?.string=="world-rising-inspection-v1",c["provenance"]?["model_id"]?.string=="retained-exact-rise-selection",c["provenance"]?["model_version"]?.string=="1",let domain=c["timing"]?["clock_domain"]?.string,domain.hasPrefix("simulation:") else {throw MissionError.invalidDraft}
        _ = try payload(p);let run=String(domain.dropFirst(11));guard !run.isEmpty,c["key"]?["entity"]?.string==run+"/rising-inspection" else {throw MissionError.invalidDraft}
        func embedded(_ key:String)throws->(Data,JSONValue){guard let encoded=p[key]?.string.flatMap({Data(base64Encoded:$0)}) else{throw MissionError.invalidDraft};let data=key=="temporal_json" ? try RisingTemporalCodec.decode(encoded,codec:p["temporal_codec"]?.string):encoded;guard data.count<=40<<10 else{throw MissionError.invalidDraft};let value=try JSONValue.decode(data,limit:40<<10);try component(value,outer:c);return(data,value)}
        let (joinedRaw,joined)=try embedded("joined_json"),(temporalRaw,temporal)=try embedded("temporal_json"),(_,spatial)=try embedded("spatial_json")
        guard p["joined_sha256"]?.string==WorldCredentials.digest(joinedRaw),p["temporal_sha256"]?.string==WorldCredentials.digest(temporalRaw),joined["key"]?["type"]?.string=="wendy.incident.change.v1",joined["key"]?["entity"]?.string==run+"/rising-incident",temporal["key"]?["type"]?.string=="wendy.gas.change.v1",temporal["key"]?["entity"]?.string==run+"/gas-change",c["key"]?["source"]==joined["key"]?["source"],c["version"]==joined["version"],spatial["key"]?["source"]==joined["key"]?["source"],spatial["version"]==joined["version"],c["timing"]==joined["timing"],c["ttl_ns"]==joined["ttl_ns"],c["provenance"]?["inputs"]==joined["provenance"]?["inputs"],joined["provenance"]?["app_version"]?.string=="world-incident-change-v1",joined["provenance"]?["model_id"]?.string=="exact-original-witness-join",joined["provenance"]?["model_version"]?.string=="1",temporal["provenance"]?["app_version"]?.string=="bounded-gas-change-v1",temporal["provenance"]?["model_id"]?.string=="bounded-gas-change-v1",temporal["provenance"]?["model_version"]?.string=="1" else{throw MissionError.invalidDraft}
        _ = try NativeTriggerWire.component(spatial)
        let location=try IncidentTriggerWire.review(spatial,fixedTarget:fixedTarget,destination:destination)
        guard let j=joined["payload"],let claim=temporal["payload"],let old=spatial["payload"] else{throw MissionError.invalidDraft}
        try shape(j,["schema","contract_sha256","state","reason","supports","witnesses","valid_until","input_digest"],optional:["carriers"])
        try shape(claim,["schema","contract_sha256","gateway","window","inputs"])
        guard j["schema"]?.string=="wendy.incident.change.v1",j["state"]?.string=="supported_rise",claim["schema"]?.string=="wendy.gas.change.v1",let supports=j["supports"]?.array,supports.count==3,let witnesses=j["witnesses"]?.array,witnesses.count==4,let w=claim["window"],let originals=claim["inputs"]?.array,originals.count==12,let change=supports.first(where:{$0["kind"]?.string=="change"}),supports.filter({$0["kind"]?.string != "change"})==old["supports"]?.array,let gas=supports.first(where:{$0["kind"]?.string=="gas"}),change["sha256"]==p["temporal_sha256"],change["source"]==temporal["key"]?["source"],change["source"]==gas["source"],change["version"]==temporal["version"],change["captured_at"]==temporal["timing"]?["sample_time"] else{throw MissionError.invalidDraft}
        for field in [j["contract_sha256"],j["input_digest"],claim["contract_sha256"]]{guard let hash=field?.string,WorldClient.isDigest(hash)else{throw MissionError.invalidDraft}}
        let end=(try nanos(p["valid_until"]))
        guard p["valid_until"]==j["valid_until"],(try nanos(p["valid_until"])) == (try deadlineNanos(c)),end<=(try nanos(old["valid_until"])),end<=(try nanos(change["valid_until"])),(try nanos(change["valid_until"]))<=(try deadlineNanos(temporal)) else{throw MissionError.invalidDraft}
        for support in supports{try shape(support,["kind","sha256","source","version","captured_at","valid_until"]);guard end<=(try nanos(support["valid_until"]))else{throw MissionError.invalidDraft}}
        let carriers=j["carriers"]?.array ?? [];guard carriers.count<=1 else{throw MissionError.invalidDraft}
        for pair in carriers{try shape(pair,["sha256","source","version","gas_sha256","change_sha256","valid_until"]);guard pair["gas_sha256"]==gas["sha256"],pair["change_sha256"]==change["sha256"],pair["source"]==gas["source"],pair["version"]==gas["version"],pair["version"]==change["version"],let hash=pair["sha256"]?.string,WorldClient.isDigest(hash),end<=(try nanos(pair["valid_until"]))else{throw MissionError.invalidDraft}}
        try shape(w,["state","reason","evaluated_s","valid_until_s","traces","input_digest"])
        guard w["state"]?.string=="rising",let traces=w["traces"]?.array,traces.count==4,(try number(w["valid_until_s"]))>(try number(w["evaluated_s"]))else{throw MissionError.invalidDraft}
        var indexed=[String:JSONValue](),rawComponents=[String:JSONValue](),anchor:Date?,identities=Set<String>()
        for (index,input) in originals.enumerated(){
            try component(input,outer:c,originalTransitAge:true);guard let o=input["payload"]else{throw MissionError.invalidDraft}
            try shape(o,["id","sensor","source","version","site","frame","provenance","calibration","sensor_type","units","species","captured_s","valid_until_s","value_ppm","uncertainty_ppm"])
            guard let sensor=o["sensor"]?.string,!sensor.isEmpty,input["key"]?["entity"]?.string==run+"/sensor/"+sensor,input["key"]?["type"]?.string=="wendy.gas.observation.v1",input["key"]?["source"]==o["source"],input["version"]==o["version"],o["site"]==c["key"]?["site"],o["frame"]==c["frame"],o["provenance"]==input["provenance"],o["units"]?.string=="ppm",let provenance=input["provenance"],provenance["origin"]?.string=="simulated",provenance["simulation"]?["run_id"]?.string==run,provenance["simulation"]?["clock_domain"]?.string==domain else{throw MissionError.invalidDraft}
            try shape(provenance,["origin","app_version","simulation"]);try shape(provenance["simulation"]!,["run_id","scenario_version","seed","clock_domain"]);guard provenance["simulation"]?["seed"]?.uint != nil,let scenario=provenance["simulation"]?["scenario_version"]?.string,!scenario.isEmpty,let app=provenance["app_version"]?.string,!app.isEmpty else{throw MissionError.invalidDraft}
            for field in ["calibration","sensor_type","species"]{guard let text=o[field]?.string,!text.isEmpty else{throw MissionError.invalidDraft}}
            let capture=(try number(o["captured_s"])),valid=(try number(o["valid_until_s"])),ppm=(try number(o["value_ppm"])),uncertainty=(try number(o["uncertainty_ppm"]))
            guard capture>=0,valid>capture,valid-capture<=5.000001,(0...10000).contains(ppm),(0.1...100).contains(uncertainty)else{throw MissionError.invalidDraft}
            let a=(try time(input["timing"]?["sample_time"])).addingTimeInterval(-capture);if anchor==nil{anchor=a};guard abs(a.timeIntervalSince(anchor!))<1e-6,abs((try until(input)).timeIntervalSince(anchor!)-valid)<1e-6 else{throw MissionError.invalidDraft}
            let bytes=try MissionRawJSON.field(temporalRaw,path:["payload","inputs",String(index)],limit:40<<10,requireCompact:true),hash=WorldCredentials.digest(try MissionRawJSON.originalIdentity(bytes))
            let identity=sensor+"/"+String(input["version"]?["epoch"]?.uint ?? 0)+"/"+String(input["version"]?["sequence"]?.uint ?? 0)
            guard indexed[hash]==nil,identities.insert(identity).inserted else{throw MissionError.invalidDraft};indexed[hash]=o;rawComponents[hash]=input
        }
        let hashes=indexed.keys.sorted();guard WorldCredentials.digest(try MissionWire.value(.array(hashes.map(JSONValue.string))))==w["input_digest"]?.string,let anchor,abs((try until(temporal)).timeIntervalSince(anchor)-(try number(w["valid_until_s"])))<1e-6 else{throw MissionError.invalidDraft}
        var seen=Set<String>(),used=Set<String>(),rising=0
        for trace in traces{
            try shape(trace,["sensor","state","points","min_rate_ppm_s","max_rate_ppm_s"])
            guard let sensor=trace["sensor"]?.string,seen.insert(sensor).inserted,let state=trace["state"]?.string,["rising","steady"].contains(state),let points=trace["points"]?.array,points.count==3 else{throw MissionError.invalidDraft};if state=="rising"{rising+=1}
            var previous:JSONValue?,previousVersion:JSONValue?,lo=Double.infinity,hi = -Double.infinity
            for point in points{
                try shape(point,["captured_s","value_ppm","uncertainty_ppm","sha256","valid_until_s"])
                guard let hash=point["sha256"]?.string,used.insert(hash).inserted,let o=indexed[hash],let input=rawComponents[hash],o["sensor"]?.string==sensor,point["captured_s"]==o["captured_s"],(try number(point["captured_s"]))<=(try number(w["evaluated_s"])),point["value_ppm"]==o["value_ppm"],point["uncertainty_ppm"]==o["uncertainty_ppm"],(try number(point["valid_until_s"]))<=(try number(o["valid_until_s"]))+1e-6,(try number(w["valid_until_s"]))<=(try number(point["valid_until_s"])) else{throw MissionError.invalidDraft}
                if let a=previous,let pv=previousVersion{let dt=(try number(o["captured_s"]))-(try number(a["captured_s"]));let epoch=input["version"]!["epoch"]!.uint!,seq=input["version"]!["sequence"]!.uint!;guard dt>0,epoch>pv["epoch"]!.uint! || (epoch==pv["epoch"]!.uint! && seq>pv["sequence"]!.uint!),o["source"]==a["source"],o["calibration"]==a["calibration"],o["sensor_type"]==a["sensor_type"],o["species"]==a["species"]else{throw MissionError.invalidDraft};lo=min(lo,((try number(o["value_ppm"]))-(try number(o["uncertainty_ppm"]))-(try number(a["value_ppm"]))-(try number(a["uncertainty_ppm"])))/dt);hi=max(hi,((try number(o["value_ppm"]))+(try number(o["uncertainty_ppm"]))-(try number(a["value_ppm"]))+(try number(a["uncertainty_ppm"])))/dt)}
                previous=o;previousVersion=input["version"]
            }
            guard abs(lo-((try number(trace["min_rate_ppm_s"]))))<1e-7,abs(hi-((try number(trace["max_rate_ppm_s"]))))<1e-7,state != "rising" || lo>0 else{throw MissionError.invalidDraft}
            let matched=witnesses.filter{$0["sensor"]?.string==sensor};guard matched.count==1,let witness=matched.first,let hash=points.last?["sha256"]?.string,let input=rawComponents[hash]else{throw MissionError.invalidDraft};try shape(witness,["sensor","sha256","captured_at","valid_until"]);guard witness["sha256"]?.string==hash,witness["captured_at"]==input["timing"]?["sample_time"],(try nanos(witness["valid_until"])) == (try deadlineNanos(input)),end<=(try nanos(witness["valid_until"]))else{throw MissionError.invalidDraft}
        }
        guard rising>=3,used.count==12 else{throw MissionError.invalidDraft}
        for value in [c,joined,temporal]{_ = try NativeTriggerWire.provenance(value["provenance"]!,maxInputs:32);guard let leaves=value["provenance"]?["inputs"]?.array else{throw MissionError.invalidDraft};var ids=Set<String>();for leaf in leaves{guard let id=leaf["id"]?.string,ids.insert(id).inserted,leaf["simulation"]?["run_id"]?.string==run,leaf["simulation"]?["clock_domain"]?.string==domain else{throw MissionError.invalidDraft}};if value==temporal{guard ids==Set(hashes.map{"sha256:"+$0})else{throw MissionError.invalidDraft}}}
        return(schema,location.target)
    }
}
