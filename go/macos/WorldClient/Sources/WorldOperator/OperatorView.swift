import SwiftUI
import MapKit
import UniformTypeIdentifiers
import WorldClient

enum ConsolePage: String, CaseIterable, Identifiable {
    case world = "World", sources = "Sources", missions = "Missions", notes = "Notes", areas = "Areas"
    var id: String { rawValue }
    var icon: String {
        switch self { case .areas: "rectangle.dashed"; case .world: "square.3.layers.3d"; case .sources: "sensor.tag.radiowaves.forward"; case .missions: "point.topleft.down.to.point.bottomright.curvepath"; case .notes: "note.text" }
    }
}

enum WorldStyle {
    static let background = Color(red: 0.035, green: 0.055, blue: 0.075)
    static let panel = Color(red: 0.075, green: 0.100, blue: 0.125)
    static let accent = Color(red: 0.35, green: 0.86, blue: 0.73)
    static func color(_ kind: String) -> Color {
        switch kind { case "gas": .orange; case "camera": .cyan; case "audio": .purple; case "hazard": .red; case "person": .mint; case "annotation": .yellow; default: accent }
    }
    static func icon(_ kind: String) -> String {
        switch kind { case "gas": "aqi.medium"; case "camera": "video.fill"; case "audio": "waveform"; case "hazard": "exclamationmark.triangle.fill"; case "person": "person.crop.circle"; case "annotation": "note.text"; default: "dot.radiowaves.left.and.right" }
    }
}

struct OperatorView: View {
    @ObservedObject var model: OperatorModel
    @Environment(\.scenePhase) private var scenePhase
    @Binding var page: ConsolePage?
    @State private var importing = false

    var body: some View {
        NavigationSplitView {
            VStack(alignment: .leading, spacing: 26) {
                HStack(spacing: 11) {
                    Image(systemName: "globe.americas.fill").font(.system(size: 30)).foregroundStyle(WorldStyle.accent)
                    VStack(alignment: .leading, spacing: 3) {
                        Text("WENDY WORLD").font(.system(size: 13, weight: .bold, design: .rounded)).tracking(1.2)
                        Text("Spatial operations").font(.caption).foregroundStyle(.secondary)
                    }
                }.padding(.horizontal, 18).padding(.top, 28)
                List(ConsolePage.allCases, selection: $page) { item in
                    Label {
                        Text(item.rawValue).foregroundStyle(page == item ? WorldStyle.accent : Color.white.opacity(0.9))
                    } icon: {
                        Image(systemName: item.icon).symbolRenderingMode(.monochrome).foregroundStyle(page == item ? WorldStyle.accent : Color.white.opacity(0.9))
                    }.tag(item).padding(.vertical, 7)
                }.listStyle(.sidebar).scrollContentBackground(.hidden)
                VStack(alignment: .leading, spacing: 12) {
                    Badge(label: model.canMission ? "SIMULATION OPERATOR" : model.canAnnotate ? "SIMULATION CONTRIBUTOR" : "SIMULATION READER", color: WorldStyle.accent)
                    Text(model.site).font(.subheadline.weight(.semibold)).lineLimit(2)
                    Text(model.canMission ? "Signed missions · private preview" : model.canAnnotate ? "Human notes · private preview" : "Read-only · private preview").font(.caption).foregroundStyle(.secondary)
                    Button { importing = true } label: { Label("Import profile", systemImage: "square.and.arrow.down") }.buttonStyle(.bordered)
                }.padding(18)
            }.background(WorldStyle.background).navigationSplitViewColumnWidth(min: 210, ideal: 235, max: 270)
        } detail: {
            ScrollView {
                VStack(alignment: .leading, spacing: 22) {
                    if page != .areas && !model.showAreasForTesting { header }
                    if let notice = model.notice, page != .areas && !model.showAreasForTesting {
                        Label(notice, systemImage: model.offline ? "wifi.slash" : "info.circle")
                            .font(.callout).foregroundStyle(model.offline ? .orange : .secondary)
                            .frame(maxWidth: .infinity, alignment: .leading).padding(14).background(.orange.opacity(0.07), in: RoundedRectangle(cornerRadius: 12))
                    }
                    if page == .areas || model.showAreasForTesting {
                        AreaWorkspaceView(model:model)
                    } else if page == .notes || model.showNotesForTesting {
                        AnnotationView(model: model)
                    } else if page == .missions || model.showMissionsForTesting {
                        MissionView(model: model)
                        missionList
                    } else if model.snapshot == nil {
                        emptyState
                    } else {
                        switch page ?? .world {
                        case .areas: AreaWorkspaceView(model:model)
                        case .world: overview
                        case .sources: sources
                        case .missions: missionList
                        case .notes: AnnotationView(model: model)
                        }
                    }
                }.padding(24).frame(maxWidth: 1600)
            }.background(WorldStyle.background).navigationTitle("")
        }
        .tint(WorldStyle.accent)
        .fileImporter(isPresented: $importing, allowedContentTypes: [.json]) { result in
            if case .success(let url) = result { model.actions.submit { await model.importProfile(url) } }
        }
        .sheet(item: $model.preview) { EvidenceView(preview: $0) }
        .onChange(of: scenePhase) { _, phase in model.setSceneActive(phase == .active) }
        .onAppear { if model.showAreasForTesting { page = .areas } else if model.showNotesForTesting { page = .notes } else if model.showMissionsForTesting { page = .missions } }
        .onChange(of: model.showAreasForTesting) { _, show in if show { page = .areas } }
        .onChange(of: model.showMissionsForTesting) { _, show in if show { page = .missions } }
        .onChange(of: model.showNotesForTesting) { _, show in if show { page = .notes } }
    }

    private var header: some View {
        HStack(alignment: .top, spacing: 16) {
            VStack(alignment: .leading, spacing: 7) {
                Text(page?.rawValue == "World" ? "Your site, in context." : page?.rawValue ?? "World")
                    .font(.system(size: 29, weight: .semibold, design: .rounded))
                Text(model.site).font(.subheadline).foregroundStyle(.secondary)
                if let fetched = model.fetchedAt {
                    Text("Snapshot received \(fetched.formatted(date: .omitted, time: .standard)) · observations retain their own expiry")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            Spacer(minLength: 0)
            VStack(alignment: .trailing, spacing: 10) {
                Badge(label: model.state.uppercased(), color: model.state == "Connected" ? WorldStyle.accent : .orange)
                HStack(spacing: 8) {
                    Button { model.togglePause() } label: { Image(systemName: model.paused ? "play.fill" : "pause.fill") }.help(model.paused ? "Resume refresh" : "Pause refresh")
                    Button { model.actions.submit { await model.refresh() } } label: { Image(systemName: "arrow.clockwise") }.disabled(model.refreshing).help("Refresh snapshot")
                    Button { importing = true } label: { Image(systemName: "square.and.arrow.down") }.help("Import simulation profile")
                }.buttonStyle(.bordered)
            }
        }
    }

    private var emptyState: some View {
        VStack(alignment: .leading, spacing: 20) {
            Image(systemName: "square.3.layers.3d").font(.system(size: 58, weight: .light)).foregroundStyle(WorldStyle.accent)
            Text("A shared world starts at the gateway.").font(.title2.weight(.semibold))
            Text("Import a simulation reader profile to see the site's placement, sensor layers, and retained mission results. Your last snapshot stays available when the gateway disconnects.")
                .foregroundStyle(.secondary).frame(maxWidth: 560, alignment: .leading)
            Button { importing = true } label: { Label("Import simulation profile", systemImage: "square.and.arrow.down") }.buttonStyle(.borderedProminent)
            Text("Import a simulation profile to read the site. Separately commissioned operator profiles can authorize simulated inspections.").font(.caption).foregroundStyle(.secondary)
        }.frame(maxWidth: .infinity, minHeight: 340, alignment: .leading).padding(28).card()
    }

    private var overview: some View {
        VStack(alignment: .leading, spacing: 20) {
            LazyVGrid(columns: [GridItem(.adaptive(minimum: 160))], spacing: 12) {
                Metric(title: "OBSERVATIONS", number: "\(model.observations.count)", subtitle: "Local replica", icon: "square.stack.3d.up")
                Metric(title: "SENSOR LAYERS", number: "\(Set(model.observations.map(\.kind)).count)", subtitle: "Independently sourced", icon: "sensor.tag.radiowaves.forward")
                Metric(title: "MISSIONS", number: model.tasks["entries"]?.array == nil ? "—" : "\(model.missions.count)", subtitle: "Last-known status", icon: "checkmark.seal")
                Metric(title: "SITE PLACEMENT", number: model.placement == nil ? "—" : "WGS84", subtitle: model.placement?.placement.capitalized ?? "Not published", icon: "globe.europe.africa")
            }
            VStack(alignment: .leading, spacing: 16) {
                HStack {
                    VStack(alignment: .leading, spacing: 5) {
                        Text("Local world plan").font(.title3.weight(.semibold))
                        Text("Metres · X / Z plane · Y up").font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer()
                    if !model.frames.isEmpty {
                        Picker("Frame", selection: $model.selectedFrame) { ForEach(model.frames) { Text($0.title).tag($0.id) } }.labelsHidden().frame(maxWidth: 300)
                    }
                }
                ViewThatFits(in: .horizontal) {
                    HStack(spacing: 8) { layerButtons }
                    LazyVGrid(columns: [GridItem(.flexible()), GridItem(.flexible())], alignment: .leading, spacing: 8) { layerButtons }
                }
                TimelineView(.periodic(from: .now, by: 1)) { context in
                    LocalPlan(observations: model.visibleObservations, selected: $model.selectedObservation, now: context.date, offline: model.offline)
                }.frame(height: 330).clipShape(RoundedRectangle(cornerRadius: 12))
                Text("Only the selected frame is plotted. Sensor measurements and inferred people or hazards keep their reported positions. This view does not infer walls or free space.")
                    .font(.caption).foregroundStyle(.secondary)
            }.padding(20).card()
            ViewThatFits(in: .horizontal) {
                HStack(alignment: .top, spacing: 20) { geography.frame(minWidth: 370); inspector.frame(minWidth: 350) }
                VStack(spacing: 20) { geography; inspector }
            }
            if !model.missions.isEmpty { missionList }
        }
    }

    @ViewBuilder private var layerButtons: some View {
        ForEach(["camera", "gas", "audio", "hazard", "person", "annotation"], id: \.self) { kind in
            Button {
                if model.enabledLayers.contains(kind) { model.enabledLayers.remove(kind) } else { model.enabledLayers.insert(kind) }
            } label: {
                Label(kind == "person" ? "People" : kind.capitalized, systemImage: WorldStyle.icon(kind)).font(.caption.weight(.semibold)).padding(.horizontal, 12).padding(.vertical, 8)
                    .foregroundStyle(model.enabledLayers.contains(kind) ? WorldStyle.color(kind) : .secondary)
                    .background(model.enabledLayers.contains(kind) ? WorldStyle.color(kind).opacity(0.13) : Color.white.opacity(0.03), in: Capsule())
            }.buttonStyle(.plain)
        }
    }

    private var geography: some View {
        TimelineView(.periodic(from: .now, by: 30)) { context in geography(at: context.date) }
    }

    private func geography(at now: Date) -> some View {
        VStack(alignment: .leading, spacing: 15) {
            Label("Geographic placement", systemImage: "globe.europe.africa").font(.headline)
            if !model.placements.isEmpty {
                Picker("Publication", selection: $model.selectedSiteID) {
                    Text("Choose a publication").tag("")
                    ForEach(model.placements) { Text($0.revision + ($0.isValid(at: now) ? "" : " · " + $0.geographic.state(at: now).message)).tag($0.id) }
                }
            }
            if let placement = model.placement, placement.isValid(at: now), let latitude = placement.latitude, let longitude = placement.longitude {
                Map(initialPosition: .region(MKCoordinateRegion(center: CLLocationCoordinate2D(latitude: latitude, longitude: longitude), latitudinalMeters: 1600, longitudinalMeters: 1600))) {
                    Annotation(placement.site, coordinate: CLLocationCoordinate2D(latitude: latitude, longitude: longitude)) {
                        Image(systemName: "building.2.fill").font(.title2).foregroundStyle(.black).padding(12).background(WorldStyle.accent, in: Circle())
                    }
                }.mapStyle(.standard(elevation: .flat)).mapControls { MapCompass(); MapScaleView() }
                    .frame(height: 185).clipShape(RoundedRectangle(cornerRadius: 12)).id(placement.id)
                Badge(label: placement.placement.uppercased() + " PLACEMENT", color: placement.placement == "surveyed" ? WorldStyle.accent : .orange)
                Text(String(format: "%.6f° %@, %.6f° %@", abs(latitude), latitude < 0 ? "S" : "N", abs(longitude), longitude < 0 ? "W" : "E")).font(.system(.callout, design: .monospaced))
                Text("Anchor uncertainty: \(placement.uncertainty.map { String(format: "%.1f m", $0) } ?? "unknown") · \(placement.revision)").font(.caption).foregroundStyle(.secondary)
                Text(placement.geographic.rootOrientationText).font(.caption).foregroundStyle(.secondary)
                Text(placement.geographic.frameOrientationText).font(.caption).foregroundStyle(.secondary)
                Text(GeographicPlacement.uncertaintyExplanation).font(.caption).foregroundStyle(.secondary)
                Text(text(placement.document?["placement_source"], "The site publisher supplied this placement.")).font(.caption).foregroundStyle(.secondary)
                DetailRow(title: "Root frame", value: text(value(placement.document, "root", "id")) + " @ " + text(value(placement.document, "root", "revision")))
                DetailRow(title: "Document valid until", value: placement.validUntil?.formatted(date: .abbreviated, time: .standard) ?? "Unknown")
                Text("Basemap tiles may require internet. The saved local plan remains available offline. Local sensor positions are not projected onto this map.")
                    .font(.caption).foregroundStyle(.secondary)
            } else {
                Text(model.sites["publications"]?.array == nil ? "Site publications unavailable" : model.placements.isEmpty ? "No geographic placement published" : model.placement?.geographic.state(at: now).message ?? "Choose a geographic publication").font(.callout)
                Text("The local plan remains available. Multiple revisions require an explicit selection; an expired publication is not used for the map.").font(.caption).foregroundStyle(.secondary)
            }
        }.frame(maxWidth: .infinity, alignment: .leading).padding(20).card()
    }

    private var inspector: some View {
        VStack(alignment: .leading, spacing: 15) {
            Label("Observation detail", systemImage: "scope").font(.headline)
            if let observation = model.selected {
                HStack {
                    Image(systemName: WorldStyle.icon(observation.kind)).foregroundStyle(WorldStyle.color(observation.kind))
                    Text(observation.name).font(.title3.weight(.semibold)).lineLimit(2)
                    Spacer()
                }
                TimelineView(.periodic(from: .now, by: 1)) { context in
                    Badge(label: observation.freshness(at: context.date, offline: model.offline).uppercased(), color: observation.freshness(at: context.date, offline: model.offline) == "Fresh" ? WorldStyle.accent : .orange)
                }
                SensorValue(observation: observation)
                Divider()
                DetailRow(title: "Origin", value: observation.origin)
                DetailRow(title: "Device", value: observation.device)
                DetailRow(title: "App / service", value: text(value(observation.raw, "key", "source", "app")) + " / " + text(value(observation.raw, "key", "source", "service")))
                DetailRow(title: "Stream", value: text(value(observation.raw, "key", "source", "stream")))
                DetailRow(title: "Frame", value: observation.frame)
                DetailRow(title: "Model", value: observation.native.map { $0.modelID + " / " + $0.modelVersion } ?? text(observation.payload?["sensor_model"], "Not declared"))
                DetailRow(title: "Clock", value: text(value(observation.raw, "timing", "clock_domain"), "Unknown"))
                DetailRow(title: "Version", value: "Epoch \(value(observation.raw, "version", "epoch")?.int.map(String.init) ?? "?") · sequence \(value(observation.raw, "version", "sequence")?.int.map(String.init) ?? "?")")
                DetailRow(title: "Sample", value: observation.sampleTime?.formatted(date: .abbreviated, time: .standard) ?? "Unknown")
                if let position = observation.position { DetailRow(title: "Position", value: String(format: "X %.2f · Y %.2f · Z %.2f m", position[0], position[1], position[2])) }
                if let native = observation.native { NativeInferenceDetails(inference: native) }
                else if observation.kind == "hazard" {
                    DetailRow(title: "Fusion inputs", value: "\(observation.payload?["inputs"]?.array?.count ?? 0) source samples")
                    DetailRow(title: "Confidence", value: observation.payload?["confidence"]?.double.map { String(format: "%.0f%%", $0 * 100) } ?? "Unknown")
                    Text(text(observation.payload?["confidence_meaning"], "Rule confidence; not a probability of safety.")).font(.caption).foregroundStyle(.secondary)
                }
                evidenceButtons(observation.evidence)
            } else { Text("Select an observation on the local plan.").foregroundStyle(.secondary) }
        }.frame(maxWidth: .infinity, alignment: .leading).padding(20).card()
    }

    private var sources: some View {
        VStack(alignment: .leading, spacing: 18) {
            Text("Sensor measurements and inferred claims keep their source, frame, original sample time, and evidence references.").foregroundStyle(.secondary)
            LazyVGrid(columns: [GridItem(.adaptive(minimum: 300), alignment: .top)], alignment: .leading, spacing: 16) {
                ForEach(model.observations) { observation in
                    VStack(alignment: .leading, spacing: 14) {
                        HStack { Label(observation.name, systemImage: WorldStyle.icon(observation.kind)).font(.headline).foregroundStyle(WorldStyle.color(observation.kind)); Spacer() }
                        SensorValue(observation: observation)
                        TimelineView(.periodic(from: .now, by: 1)) { context in
                            Badge(label: observation.freshness(at: context.date, offline: model.offline).uppercased(), color: observation.freshness(at: context.date, offline: model.offline) == "Fresh" ? WorldStyle.accent : .orange)
                        }
                        DetailRow(title: "Origin", value: observation.origin)
                        DetailRow(title: "Frame", value: observation.frame)
                        DetailRow(title: "Device", value: observation.device)
                        if let native = observation.native { NativeInferenceDetails(inference: native) }
                        evidenceButtons(observation.evidence)
                        Button("Locate on plan") { model.selectedFrame = observation.frameKey; model.selectedObservation = observation.id; page = .world }
                            .buttonStyle(.bordered)
                    }.padding(20).card()
                }
            }
            if model.observations.isEmpty { Text("No observations in the current snapshot.").foregroundStyle(.secondary) }
        }
    }

    private var missionList: some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack { Text("Mission history").font(.title3.weight(.semibold)); Spacer(); Badge(label: "LAST-KNOWN", color: .secondary) }
            Text("Retained assignee reports show what was last received. They do not establish the robot's current motion or reachability.").font(.callout).foregroundStyle(.secondary)
            ForEach(model.missions) { mission in
                VStack(alignment: .leading, spacing: 15) {
                    HStack(alignment: .top) {
                        VStack(alignment: .leading, spacing: 5) {
                            Text(mission.name).font(.headline).textSelection(.enabled)
                            Text(mission.assignee).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
                        }
                        Spacer()
                        Badge(label: mission.phase.uppercased(), color: mission.phase == "succeeded" ? WorldStyle.accent : .orange)
                    }
                    if model.canMission {
                        Button("Stop task", role: .destructive) { model.actions.submit { await model.stopMission(id: mission.name) } }.buttonStyle(.bordered).disabled(model.missionStopping)
                    }
                    if let at = mission.observedAt { DetailRow(title: "Signed status", value: at.formatted(date: .abbreviated, time: .standard)) }
                    if let reason = value(mission.record, "status", "Reason")?.string, !reason.isEmpty { Text(reason).font(.callout).foregroundStyle(.secondary) }
                    if let result = mission.result {
                        Divider()
                        Label("Inspection result retained", systemImage: "checkmark.seal.fill").foregroundStyle(WorldStyle.accent)
                        DetailRow(title: "Run", value: text(result["run_id"]))
                        DetailRow(title: "Source samples", value: "\(result["samples"]?.array?.count ?? 0)")
                        DetailRow(title: "Obstacle report", value: result["obstacle_clear"]?.bool.map { $0 ? "Clear at capture" : "Not clear at capture" } ?? "Not provided")
                        evidenceButtons(mission.evidence)
                    }
                }.padding(20).card()
            }
            if model.missions.isEmpty { Text(model.tasks["entries"]?.array == nil ? "Mission reports are unavailable. Refresh to try again." : "No retained mission reports at this gateway.").foregroundStyle(.secondary).padding(20).card() }
        }
    }

    @ViewBuilder private func evidenceButtons(_ items: [EvidenceItem]) -> some View {
        if !items.isEmpty {
            VStack(alignment: .leading, spacing: 8) {
                ForEach(items) { item in
                    Button { model.actions.submit { await model.openEvidence(item) } } label: {
                        HStack {
                            Image(systemName: item.contentType.hasPrefix("audio") ? "waveform" : "photo")
                            VStack(alignment: .leading, spacing: 3) {
                                Text(item.name).lineLimit(1)
                                if let owner = item.nativeReference?.source {
                                    Text(text(owner["device"]) + " · " + text(owner["service"]))
                                        .font(.caption2).foregroundStyle(.secondary).lineLimit(2)
                                }
                            }
                            Spacer()
                            if model.evidenceLoading == item.id { ProgressView().controlSize(.small) } else { Image(systemName: "arrow.down.circle") }
                        }.font(.caption).padding(10).background(.white.opacity(0.04), in: RoundedRectangle(cornerRadius: 8))
                    }.buttonStyle(.plain).disabled(model.evidenceLoading != nil)
                }
                Text("Fetched on demand · SHA-256 verified before display").font(.caption2).foregroundStyle(.secondary)
            }
        }
    }
}

struct Badge: View {
    let label: String
    let color: Color
    var body: some View { Text(label).font(.system(size: 10, weight: .bold, design: .monospaced)).tracking(0.7).padding(.horizontal, 9).padding(.vertical, 6).foregroundStyle(color).background(color.opacity(0.12), in: Capsule()) }
}

private struct Metric: View {
    let title: String, number: String, subtitle: String, icon: String
    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack { Text(title).font(.system(size: 10, weight: .bold)).tracking(0.9).foregroundStyle(.secondary); Spacer(); Image(systemName: icon).foregroundStyle(WorldStyle.accent) }
            Text(number).font(.system(size: 28, weight: .semibold, design: .rounded))
            Text(subtitle).font(.caption).foregroundStyle(.secondary)
        }.padding(18).card()
    }
}

struct DetailRow: View {
    let title: String, value: String
    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(title.uppercased()).font(.system(size: 9, weight: .semibold)).tracking(0.8).foregroundStyle(.secondary)
            Text(value).font(.system(size: 12, design: .monospaced)).textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
        }
    }
}

private struct SensorValue: View {
    let observation: Observation
    var body: some View {
        VStack(alignment: .leading, spacing: 5) {
            if let person = observation.native?.person {
                Text("Anonymous person location").font(.title3.weight(.semibold))
                Text(person.ambiguous ? "Association uncertain" : "Camera-derived track").font(.caption).foregroundStyle(.secondary)
                Text("Height not reported · " + (person.motionKnown ? "Motion estimated from two observations" : "Motion unknown")).font(.caption).foregroundStyle(.secondary)
            } else if let hazard = observation.native?.hazard {
                Text("Possible gas hazard").font(.title3.weight(.semibold)).foregroundStyle(.red)
                Text("Inferred claim · " + hazard.locationBasis).font(.caption).foregroundStyle(.secondary)
                Text(String(format: "Gas input %.1f ± %.1f ppm · %@", hazard.gasValuePPM, hazard.gasUncertaintyPPM, hazard.gasSpecies)).font(.callout)
            } else if observation.type == NativeInference.componentType {
                Text("This inference cannot be displayed.").font(.callout)
                Text("Its schema is unsupported or its recorded fields are incomplete.").font(.caption).foregroundStyle(.secondary)
            } else if observation.kind == "annotation" {
                Text(text(observation.payload?["text"])).font(.body).textSelection(.enabled)
                Text("Human assertion by " + text(observation.payload?["author_principal"])).font(.caption).foregroundStyle(.yellow)
            } else if let gas = value(observation.payload, "gas", "value_ppm")?.double ?? observation.payload?["gas_value_ppm"]?.double {
                Text(String(format: "%.1f ppm", gas)).font(.system(size: 30, weight: .medium, design: .rounded)).foregroundStyle(WorldStyle.color(observation.kind))
                let uncertainty = value(observation.payload, "gas", "uncertainty_ppm")?.double ?? observation.payload?["gas_uncertainty_ppm"]?.double
                Text("\(text(value(observation.payload, "gas", "species"), text(observation.payload?["gas_species"], "Gas"))) · uncertainty \(uncertainty.map { String(format: "±%.1f ppm", $0) } ?? "unknown")").font(.caption).foregroundStyle(.secondary)
            } else if observation.kind == "camera" {
                Text("\(value(observation.payload, "camera", "width")?.int.map(String.init) ?? "?") × \(value(observation.payload, "camera", "height")?.int.map(String.init) ?? "?")").font(.system(size: 28, weight: .medium, design: .rounded))
                Text("Camera sample · \(observation.evidence.count) evidence reference").font(.caption).foregroundStyle(.secondary)
            } else if observation.kind == "audio" {
                Text("\(value(observation.payload, "audio", "sample_rate_hz")?.int.map(String.init) ?? "?") Hz").font(.system(size: 28, weight: .medium, design: .rounded))
                Text("\(value(observation.payload, "audio", "channels")?.int.map(String.init) ?? "Unknown") channel audio sample").font(.caption).foregroundStyle(.secondary)
            } else { Text(observation.kind.capitalized).font(.title3) }
        }
    }
}

private struct NativeInferenceDetails: View {
    let inference: NativeInference
    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            DetailRow(title: "Run", value: inference.runID)
            DetailRow(title: "Observed", value: inference.observedAt.formatted(date: .abbreviated, time: .standard))
            DetailRow(title: "Original sample deadline", value: inference.sampleValidUntil.formatted(date: .abbreviated, time: .standard))
            if let person = inference.person {
                DetailRow(title: "Position · height not reported", value: String(format: "X %.2f · Z %.2f m", person.positionXZ[0], person.positionXZ[1]))
                DetailRow(title: "Position covariance · m²", value: person.covariance.map { String(format: "%.4f", $0) }.joined(separator: ", "))
                if let velocity = person.velocityXZ { DetailRow(title: "Velocity · X / Z", value: velocity.map { String(format: "%.2f", $0) }.joined(separator: ", ") + " m/s") }
                DetailRow(title: "Camera support", value: "\(person.current.count) current · \(person.previous.count) previous")
            }
            if let hazard = inference.hazard {
                DetailRow(title: "Location uncertainty", value: String(format: "± %.2f m", hazard.uncertaintyM))
                DetailRow(title: "Rule score", value: String(format: "%.2f", hazard.confidence))
                Text(hazard.confidenceMeaning).font(.caption).foregroundStyle(.secondary)
                DetailRow(title: "Input measurements", value: hazard.inputs.compactMap { $0["kind"]?.string }.joined(separator: " · "))
            }
            DisclosureGroup("Source and evidence details") {
                VStack(alignment: .leading, spacing: 10) {
                    DetailRow(title: "Schema", value: inference.schema)
                    DetailRow(title: "Physical interpretation deadline", value: inference.physicalValidUntil.formatted(date: .abbreviated, time: .standard))
                    ForEach(Array(inference.inputs.enumerated()), id: \.offset) { _, ref in
                        DetailRow(title: text(ref.source["stream"]), value: text(ref.source["device"]) + " · " + text(ref.source["app"]) + "/" + text(ref.source["service"]) + "\nEpoch \(ref.epoch) · sequence \(ref.sequence)\n" + ref.envelopeSHA256)
                    }
                    ForEach(Array(inference.evidence.enumerated()), id: \.offset) { _, ref in
                        DetailRow(title: "\(ref.contentType) · \(ref.size) bytes", value: text(ref.reference.source["stream"]) + " · " + ref.sha256)
                    }
                }.padding(.top, 8)
            }
        }
    }
}

extension View {
    func card() -> some View { background(WorldStyle.panel, in: RoundedRectangle(cornerRadius: 16)).overlay(RoundedRectangle(cornerRadius: 16).strokeBorder(.white.opacity(0.06))) }
}
