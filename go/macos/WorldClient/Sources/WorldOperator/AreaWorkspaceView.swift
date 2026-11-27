import SwiftUI
import WorldClient

struct AreaWorkspaceView: View {
    @ObservedObject var model:OperatorModel
    @State private var name="West work area"
    @State private var frame=""
    @State private var revision=""
    @State private var minX="0"
    @State private var minZ="0"
    @State private var maxX="5"
    @State private var maxZ="5"
    @State private var layers:Set<String>=["wendy.sensor.v1","wendy.hazard.v1"]
    @State private var drawing=false
    private var draft:AreaRegion? {
        guard let x=Double(minX),let z=Double(minZ),let X=Double(maxX),let Z=Double(maxZ) else{return nil}
        return try? AreaRegion(frameID:frame,frameRevision:revision,minXZ:[x,z],maxXZ:[X,Z])
    }
    var body:some View {
        VStack(alignment:.leading,spacing:18) {
            HStack {VStack(alignment:.leading,spacing:5){Text("Area workspaces").font(.largeTitle.bold());Text("Exact-frame reads • saved complete views • no regional node storage").foregroundStyle(.secondary)};Spacer();Image(systemName:"rectangle.dashed").font(.largeTitle).foregroundStyle(.cyan)}
            if !model.canUseAreas {Text("Import a standalone World reader profile. Native-agent endpoints do not support area reads.").foregroundStyle(.orange)}
            if let notice=model.areaNotice {Text(notice).font(.callout).foregroundStyle(model.areaOnline ? Color.secondary : Color.orange)}
            HStack(alignment:.top,spacing:18) {
                VStack(alignment:.leading,spacing:12) {
                    Text("Saved workspaces").font(.headline)
                    ForEach(model.areaEntries) {entry in
                        Button{model.selectArea(entry.id)} label:{VStack(alignment:.leading){Text(entry.definition.name).font(.headline);Text(entry.definition.region.frameID+" @ "+entry.definition.region.frameRevision).font(.caption);Text(entry.snapshot==nil ? "No complete snapshot" : "Complete snapshot saved").font(.caption2)}}.buttonStyle(.bordered).tint(model.areaSelectedID==entry.id ? .cyan : .gray)
                    }
                    Divider()
                    Text("New workspace").font(.headline)
                    TextField("Name",text:$name)
                    HStack{TextField("Frame ID",text:$frame);TextField("Revision",text:$revision)}
                    HStack{TextField("Min X",text:$minX);TextField("Min Z",text:$minZ)}
                    HStack{TextField("Max X",text:$maxX);TextField("Max Z",text:$maxZ)}
                    Text("Metres · right-handed Y-up · rectangle on X/Z").font(.caption2).foregroundStyle(.secondary)
                    Toggle("Draw draft on plan",isOn:$drawing)
                    ForEach(AreaDefinition.supportedTypes,id:\.self){type in
                        Toggle(layerName(type),isOn:Binding(get:{layers.contains(type)},set:{on in if on{layers.insert(type)}else{layers.remove(type)}})).font(.caption)
                    }
                    Text("Original notes only. Corrections, conflicts and withdrawals are reviewed in Notes; suppressed originals stay hidden.").font(.caption2).foregroundStyle(.secondary)
                    Button("Save new workspace") {guard let draft else{return};model.actions.submit {await model.saveArea(name:name,frameID:frame,revision:revision,minXZ:draft.minXZ,maxXZ:draft.maxXZ,types:Array(layers))}}.buttonStyle(.borderedProminent).disabled(draft==nil || layers.isEmpty || !model.canUseAreas || model.areaBusy)
                }.textFieldStyle(.roundedBorder).frame(width:260).padding(16).background(WorldStyle.panel,in:RoundedRectangle(cornerRadius:14))
                VStack(alignment:.leading,spacing:14) {
                    if let entry=model.currentArea {
                        HStack{VStack(alignment:.leading){Text(entry.definition.name).font(.title2.bold());Text("Selected: "+entry.definition.region.frameID+" @ "+entry.definition.region.frameRevision).font(.caption).foregroundStyle(.cyan)};Spacer();Button(model.areaBusy ? "Reading pages…" : "Refresh complete view"){model.actions.submit {await model.refreshArea()}}.buttonStyle(.borderedProminent).disabled(model.areaBusy);Button("Watch next change"){model.actions.submit {await model.refreshArea(watching:true)}}.disabled(model.areaBusy || model.currentArea?.snapshot==nil);Button("Remove local"){model.actions.submit {await model.removeArea()}}.disabled(model.areaBusy)}
                    }
                    TimelineView(.periodic(from:.now,by:1)) {clock in
                        let snap=model.currentArea?.snapshot
                        let rows=(try? snap?.records()) ?? []
                        AreaCanvas(region:model.currentArea?.definition.region,draft:draft,rows:rows,snapshot:snap,now:clock.date,drawing:drawing) {lo,hi in
                            minX=String(format:"%.3f",lo[0]);minZ=String(format:"%.3f",lo[1]);maxX=String(format:"%.3f",hi[0]);maxZ=String(format:"%.3f",hi[1])
                        }.frame(height:310)
                        HStack{Label("Cyan: saved area",systemImage:"rectangle");Label("Purple: unsaved draft",systemImage:"rectangle.dashed");Spacer();Text("Declared spatial bounds; no clearance guarantee")}.font(.caption2).foregroundStyle(.secondary)
                        if let snap {
                            if snap.partialReplica {Text("PARTIAL REPLICA · This area may omit records outside the node’s storage subscription.").font(.caption.bold()).foregroundStyle(.orange)}
                            Text("Last complete read "+snap.receivedAt.formatted()+" · "+String(rows.count)+" records").font(.caption)
                            Text(model.areaOnline ? "A point-in-time read, not a live connection guarantee." : "OFFLINE / SAVED · HISTORICAL ONLY").font(.caption.bold()).foregroundStyle(.orange)
                            ForEach(rows) {row in
                                let expired=snap.expired(row,at:clock.date)
                                HStack{Image(systemName:row.located ? "mappin.circle" : "questionmark.square.dashed").foregroundStyle(expired ? .gray : row.located ? .cyan : .orange);VStack(alignment:.leading){Text(row.component.entity ?? row.id).font(.callout.bold());Text(row.reason).font(.caption2).foregroundStyle(.secondary)};Spacer();Text(expired ? "Expired / clock unavailable" : row.located ? "Located · last read" : "Unlocated · separate").font(.caption).foregroundStyle(expired ? .gray : .orange)}.padding(8).background(WorldStyle.panel,in:RoundedRectangle(cornerRadius:8))
                            }
                        } else {Text("Save a workspace, then refresh. No partial result is presented as complete.").foregroundStyle(.secondary)}
                    }
                }
            }
        }.onAppear {
            if let selected=model.currentArea {frame=selected.definition.region.frameID;revision=selected.definition.region.frameRevision}
            else if let observation=model.observations.first(where:{$0.frameKey==model.selectedFrame}) {frame=observation.raw["frame"]?["id"]?.string ?? "";revision=observation.raw["frame"]?["revision"]?.string ?? ""}
        }.onChange(of:model.areaSelectedID){_,_ in
            if let selected=model.currentArea {frame=selected.definition.region.frameID;revision=selected.definition.region.frameRevision}
        }
    }
    private func layerName(_ type:String)->String {switch type{case "wendy.sensor.v1":"Sensor samples";case "wendy.hazard.v1":"Hazard claims";case "wendy.annotation.note.v1":"Original notes v1";default:"Original notes v2"}}
}

private struct AreaCanvas:View {
    let region:AreaRegion?,draft:AreaRegion?,rows:[AreaRecord],snapshot:AreaSnapshot?,now:Date,drawing:Bool
    let changed:([Double],[Double])->Void
    var body:some View {
        GeometryReader{g in
            let base=region ?? draft
            let loX=(base?.minXZ[0] ?? 0)-2,hiX=(base?.maxXZ[0] ?? 5)+2,loZ=(base?.minXZ[1] ?? 0)-2,hiZ=(base?.maxXZ[1] ?? 5)+2
            let scale=min((g.size.width-50)/(hiX-loX),(g.size.height-50)/(hiZ-loZ))
            let point:(Double,Double)->CGPoint={x,z in CGPoint(x:25+(x-loX)*scale,y:25+(z-loZ)*scale)}
            Canvas{context,size in
                for i in 0...10 {var p=Path();let x=size.width*Double(i)/10,z=size.height*Double(i)/10;p.move(to:CGPoint(x:x,y:0));p.addLine(to:CGPoint(x:x,y:size.height));p.move(to:CGPoint(x:0,y:z));p.addLine(to:CGPoint(x:size.width,y:z));context.stroke(p,with:.color(.white.opacity(0.06)),lineWidth:1)}
                if let region {let a=point(region.minXZ[0],region.minXZ[1]),b=point(region.maxXZ[0],region.maxXZ[1]);let p=Path(CGRect(x:a.x,y:a.y,width:b.x-a.x,height:b.y-a.y));context.fill(p,with:.color(.cyan.opacity(0.07)));context.stroke(p,with:.color(.cyan),lineWidth:2)}
                if let draft,draft.frameID==base?.frameID,draft.frameRevision==base?.frameRevision {let a=point(draft.minXZ[0],draft.minXZ[1]),b=point(draft.maxXZ[0],draft.maxXZ[1]);context.stroke(Path(CGRect(x:a.x,y:a.y,width:b.x-a.x,height:b.y-a.y)),with:.color(.purple),style:StrokeStyle(lineWidth:2,dash:[5,4]))}
                for row in rows where row.located && !(snapshot?.expired(row,at:now) ?? true) {
                    guard let p=row.position,p.count==3 else{continue};let at=point(p[0],p[2]),r=min(200,(row.uncertainty ?? 0)*scale)
                    if r>0{context.stroke(Path(ellipseIn:CGRect(x:at.x-r,y:at.y-r,width:r*2,height:r*2)),with:.color(.orange.opacity(0.6)),lineWidth:1)}
                    context.fill(Path(ellipseIn:CGRect(x:at.x-5,y:at.y-5,width:10,height:10)),with:.color(.orange));context.draw(Text(row.component.entity ?? "record").font(.system(size:10)).foregroundColor(.white),at:CGPoint(x:at.x,y:at.y+16))
                }
            }.background(WorldStyle.background).clipShape(RoundedRectangle(cornerRadius:12))
            .gesture(DragGesture(minimumDistance:8).onEnded{v in
                guard drawing else{return}
                guard let frame=base,let drawn=try? AreaRegion.fromCanvasDrag(frameID:frame.frameID,frameRevision:frame.frameRevision,viewportMin:[loX,loZ],viewportMax:[hiX,hiZ],size:[g.size.width,g.size.height],start:[v.startLocation.x,v.startLocation.y],end:[v.location.x,v.location.y]) else{return}
                changed(drawn.minXZ,drawn.maxXZ)
            })
            Text(drawing ? "Drag to draw a new draft · Save explicitly" : "+X →  +Z ↓  · metres").font(.caption).foregroundStyle(.secondary).padding(10)
        }
    }
}
