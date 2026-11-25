import AppKit
import CryptoKit
import Darwin
import SwiftUI
import WorldClient

struct NativeZoneProfile: Decodable {
  let version: Int
  let credentials_file: String
  let trust_file: String
}
struct NativeZoneContext: Decodable {
  let schema: String
  let run_id: String
  let model_sha256: String
  let context: ZoneContext
}

@MainActor final class DemoZones: ObservableObject {
  weak var model: DemoModel?
  @Published var showing = false
  @Published var rows: [ZoneRevision] = []
  @Published var draft = [
    ZoneRectangle(
      id: "zone-1", label: "Work area", minXZ: [-2.5, -1.5], maxXZ: [-0.5, 0.5], dwellSeconds: 1)
  ]
  @Published var name = "Office zones"
  @Published var revision = "Edit 1"
  @Published var message = "Import an existing zone operator profile."
  @Published var busy = false
  @Published var context: ZoneContext?
  @Published var active: ZoneRevision?
  @Published var pending: ZoneRevision?
  @Published var closed = false
  @Published var configured = false
  @Published var drawIndex = 0
  private var client: ZoneClient?
  private var trustPath: String?
  private var generation = 0
  private var lastClock = Date.distantPast
  private var selectionSequence: Int?
  init(model: DemoModel) {
    self.model = model
    let args = CommandLine.arguments
    if let i = args.firstIndex(of: "--zone-profile"), i + 1 < args.count {
      do { try configure(URL(fileURLWithPath: args[i + 1])) } catch {
        message = error.localizedDescription
      }
    }
  }
  static func privateRead(_ path: String, limit: Int = 128 << 10) throws -> Data {
    guard path.hasPrefix("/") else { throw ZoneError.invalid }
    let fd = Darwin.open(path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
    guard fd >= 0 else { throw ZoneError.invalid }
    defer { Darwin.close(fd) }
    var st = stat()
    guard fstat(fd, &st) == 0, st.st_mode & S_IFMT == S_IFREG, st.st_uid == geteuid(),
      st.st_mode & 0o077 == 0, st.st_size > 0, st.st_size <= limit
    else { throw ZoneError.invalid }
    let f = FileHandle(fileDescriptor: fd, closeOnDealloc: false)
    guard let data = try f.read(upToCount: limit + 1), data.count <= limit else {
      throw ZoneError.invalid
    }
    return data
  }
  func configure(_ url: URL) throws {
    guard !busy, pending == nil, active == nil else { throw ZoneError.invalid }
    let profile = try JSONDecoder().decode(NativeZoneProfile.self, from: Self.privateRead(url.path))
    guard profile.version == 1 else { throw ZoneError.invalid }
    let credentials = try ZoneCredentials(data: Self.privateRead(profile.credentials_file))
    let c = try ZoneClient(
      credentials: credentials, trustData: Self.privateRead(profile.trust_file))
    client = c
    trustPath = profile.trust_file
    configured = true
    generation += 1
    message = "Profile loaded. Refresh to verify replica catalogs."
  }
  var canPublish: Bool { client?.credentials.canPublish == true }
  func owns(_ row: ZoneRevision) -> Bool { row.source == client?.credentials.source }
  func loadContext() throws {
    guard let model, let geometry = model.geometry, let state = model.state else {
      throw ZoneError.unavailable
    }
    let raw = try Self.privateRead(
      model.session.appendingPathComponent("zone-context.json").path, limit: 8192)
    let supplied = try JSONDecoder().decode(NativeZoneContext.self, from: raw)
    guard supplied.schema == "wendy.native-zone.context.v1", supplied.run_id == state.run_id,
      supplied.context.runID == state.run_id, supplied.model_sha256 == geometry.model_sha256,
      supplied.context.tenant == client?.credentials.tenant,
      supplied.context.site == client?.credentials.site
    else { throw ZoneError.invalid }
    try supplied.context.validate()
    if let context, context != supplied.context {
      closed = true
      throw ZoneError.invalid
    }
    context = supplied.context
  }
  func refresh() async {
    guard !busy, let client, let trustPath else { return }
    busy = true
    defer { busy = false }
    let token = generation
    do {
      try loadContext()
      let now = Date()
      guard now >= lastClock else { throw ZoneError.expired }
      lastClock = now
      try await client.updateTrust(Self.privateRead(trustPath, limit: 1 << 20))
      if let active {
        do { _ = try await client.verify(active.record) }
        catch { closed = true }
      }
      let fetched = try await client.list()
      guard token == generation else { return }
      rows = fetched
      if let active {
        guard let found = fetched.first(where: { $0.id == active.id }), !found.deleted else {
          closed = true
          message = "Selected revision deleted or unavailable. Historical events only."
          return
        }
        guard active.deadline > Date() else {
          closed = true
          throw ZoneError.expired
        }
      }
      message = "Verified this replica. Select a source and digest explicitly."
    } catch {
      if error is ZoneError { closed = true }
      message = error.localizedDescription
    }
  }
  func publish() async {
    guard !busy, let client, let context, let trustPath else { return }
    busy = true
    defer { busy = false }
    let token = generation
    do {
      try loadContext()
      try await client.updateTrust(Self.privateRead(trustPath, limit: 1 << 20))
      let row = try await client.publish(
        context: context, name: name, revision: revision, zones: draft)
      guard token == generation else { return }
      rows.removeAll { $0.id == row.id }
      rows.append(row)
      message = "Published \(row.digest.prefix(16)). The active session is unchanged."
    } catch { message = error.localizedDescription }
  }
  func select(_ row: ZoneRevision) async {
    guard !busy, !row.deleted, let client, let model, model.canCommand, pending == nil, let context,
      row.context == context
    else { return }
    busy = true
    defer { busy = false }
    let token = generation
    do {
      try loadContext()
      if let trustPath { try await client.updateTrust(Self.privateRead(trustPath, limit: 1 << 20)) }
      let fresh = try await client.list()
      guard let selected = fresh.first(where: { $0.id == row.id }), !selected.deleted,
        selected.context == context, token == generation
      else { throw ZoneError.unavailable }
      let target = String(decoding: try selected.selection.encoded(), as: UTF8.self)
      model.command("zone_select", target: target)
      guard let number = model.pending else { throw ZoneError.unavailable }
      beginSelection(selected, sequence: number)
      message = "Waiting for a new presence session."
    } catch { message = error.localizedDescription }
  }
  func beginSelection(_ revision: ZoneRevision, sequence: Int) {
    pending = revision
    selectionSequence = sequence
  }
  func acknowledge(sequence: Int, target: String, accepted: Bool, tracking: TrackingState?) {
    guard let pending, selectionSequence == sequence else { return }
    defer {
      self.pending = nil
      selectionSequence = nil
    }
    let requested = try? JSONValue.decode(Data(target.utf8))
    let expected = pending.selection
    guard accepted, requested == expected, let actual = tracking?.zone_catalog,
      actual.sha256 == pending.digest, actual.source == pending.source,
      actual.selection_sequence == sequence
    else {
      message = "Selection was not acknowledged; previous session remains selected."
      return
    }
    active = pending
    closed = false
    lastClock = Date()
    message =
      "New session selected \(pending.digest.prefix(16)). Earlier history was not rewritten."
  }
  func delete(_ row: ZoneRevision) async {
    guard !busy, let client, let trustPath else { return }
    busy = true
    defer { busy = false }
    do {
      try await client.updateTrust(Self.privateRead(trustPath, limit: 1 << 20))
      try await client.delete(row)
      if active?.id == row.id { closed = true }
      rows.removeAll { $0.id == row.id }
      message = "Deletion published for this source and revision. Remote receipt may lag."
    } catch { message = error.localizedDescription }
  }
  func edit(_ row: ZoneRevision) {
    guard !row.deleted, !busy, row.context == context else { return }
    draft = row.zones
    name = row.name
    revision = "Edit \(Int(Date().timeIntervalSince1970))"
    drawIndex = 0
    message = "Editing a copy. Publishing and selecting are separate actions."
  }
  var current: Bool {
    guard let active, !closed, Date() >= lastClock, Date() < active.deadline, let model,
      model.connected, let status = model.snapshot?.tracking?.zone_catalog,
      status.sha256 == active.digest, status.source == active.source
    else { return false }
    return ["available_at_replica", "cached_snapshot"].contains(status.state)
  }
  var status: String {
    guard let active else { return "No catalog selected" }
    if closed || Date() >= active.deadline { return "Closed · historical geometry only" }
    if !current { return "Current presence unavailable" }
    return model?.snapshot?.tracking?.zone_catalog?.state == "cached_snapshot"
      ? "Cached geometry · origin state unknown" : "Available at checked replica"
  }
  func capture(_ path: URL) -> Bool {
    guard let model else { return false }
    let host = NSHostingView(rootView: NativeZonesView(model: model, zones: self).environment(\.colorScheme, .light))
    host.appearance = NSAppearance(named: .aqua)
    host.frame = CGRect(x: 0, y: 0, width: 880, height: 760)
    host.layoutSubtreeIfNeeded()
    guard let bitmap = host.bitmapImageRepForCachingDisplay(in: host.bounds) else { return false }
    host.cacheDisplay(in: host.bounds, to: bitmap)
    guard let png = bitmap.representation(using: .png, properties: [:]) else { return false }
    let value: [String: Any] = [
      "schema": "wendy.native-zone.capture.v1", "scope": "Recorded native SwiftUI panel and application callbacks, not OS input",
      "run_id": model.state?.run_id ?? "", "sequence": model.state?.sequence ?? 0,
      "catalogs": rows.map { ["sha256": $0.digest, "deleted": $0.deleted] as [String:Any] },
      "selected_sha256": active?.digest ?? "", "original_deadline": active?.deadline.timeIntervalSince1970 ?? 0,
      "current": current, "status": status, "message": message,
      "observed_counts": model.snapshot?.tracking?.zone_presence?.zones.map { current ? $0.observed : 0 } ?? [],
      "historical_events": model.snapshot?.tracking?.zone_presence?.events.count ?? 0,
      "png_sha256": SHA256.hash(data: png).map { String(format: "%02x", $0) }.joined()
    ]
    do {
      try png.write(to: path, options: .atomic)
      try JSONSerialization.data(withJSONObject: value, options: [.prettyPrinted, .sortedKeys]).write(to: path.appendingPathExtension("json"), options: .atomic)
      return true
    } catch { return false }
  }

  var activeOverlay: [ZoneRectangle] { active?.zones ?? [] }
  var activeOverlayCurrent: Bool { current }
  var overlay: [ZoneRectangle] {
    guard context != nil else { return [] }
    return draft
  }
  func draw(from: CGPoint, to: CGPoint, size: CGSize) {
    guard size.width > 0, size.height > 0, draft.indices.contains(drawIndex) else { return }
    func point(_ p: CGPoint) -> [Double] {
      [
        (min(max(Double(p.x / size.width), 0), 1) * 10 - 5),
        (min(max(Double(p.y / size.height), 0), 1) * 8 - 4),
      ].map { ($0 * 1000).rounded() / 1000 }
    }
    let a = point(from)
    let b = point(to)
    let low = [min(a[0], b[0]), min(a[1], b[1])]
    let high = [max(a[0], b[0]), max(a[1], b[1])]
    guard high[0] - low[0] >= 0.001, high[1] - low[1] >= 0.001 else { return }
    draft[drawIndex].minXZ = low
    draft[drawIndex].maxXZ = high
    model?.renderer?.update(model!, force: true)
  }
}

struct NativeZonesButton: View {
  @ObservedObject var model: DemoModel
  var body: some View {
    Button {
      model.zones.showing = true
    } label: {
      Label("Zones", systemImage: "square.dashed")
    }.accessibilityIdentifier("zones-open").buttonStyle(.bordered).sheet(isPresented: Binding(get: { model.zones.showing }, set: { model.zones.showing = $0 })) {
      NativeZonesView(model: model, zones: model.zones)
    }
  }
}
struct NativeZonesView: View {
    @State private var actions = TwinActions()

  @ObservedObject var model: DemoModel
  @ObservedObject var zones: DemoZones
  @Environment(\.dismiss) var dismiss
  @State private var importing = false
  @State private var publishReview = false
  @State private var deleting: ZoneRevision?
  var body: some View {
        Group {

    VStack(alignment: .leading, spacing: 14) {
      HStack {
        Text("Site zones").font(.system(size: 26, weight: .semibold))
        Spacer()
        Button("Import profile") { importing = true }
        Button("Done") { dismiss() }
      }
      Text("Simulation metadata · no navigation or task authority").font(.caption).foregroundStyle(
        .secondary)
      if let c = zones.context {
        Text("\(c.site) · \(c.frameID) / \(c.frameRevision) · \(c.runID)").font(
          .caption.monospaced()
        ).lineLimit(2)
      }
      HStack(alignment: .top, spacing: 20) {
        VStack(alignment: .leading, spacing: 10) {
          Text("Draw a bounded rectangle").font(.headline)
          GeometryReader { g in
            ZStack {
              Color(red: 0.93, green: 0.96, blue: 0.95)
              Path { p in
                for x in stride(from: 0.0, through: 10, by: 1) {
                  let v = x / 10 * g.size.width
                  p.move(to: CGPoint(x: v, y: 0))
                  p.addLine(to: CGPoint(x: v, y: g.size.height))
                }
                for y in stride(from: 0.0, through: 8, by: 1) {
                  let v = y / 8 * g.size.height
                  p.move(to: CGPoint(x: 0, y: v))
                  p.addLine(to: CGPoint(x: g.size.width, y: v))
                }
              }.stroke(.gray.opacity(0.2))
              ForEach(zones.activeOverlay) { z in
                Rectangle().stroke(zones.activeOverlayCurrent ? Color.teal : Color.gray, style: StrokeStyle(lineWidth: 3, dash: zones.activeOverlayCurrent ? [] : [6, 4]))
                  .frame(width: (z.maxXZ[0] - z.minXZ[0]) / 10 * g.size.width, height: (z.maxXZ[1] - z.minXZ[1]) / 8 * g.size.height)
                  .position(x: ((z.minXZ[0] + z.maxXZ[0]) / 2 + 5) / 10 * g.size.width, y: ((z.minXZ[1] + z.maxXZ[1]) / 2 + 4) / 8 * g.size.height)
              }
              ForEach(zones.draft) { z in
                Rectangle().fill(.purple.opacity(0.15)).overlay(
                  Rectangle().stroke(.purple, lineWidth: 2)
                ).frame(
                  width: (z.maxXZ[0] - z.minXZ[0]) / 10 * g.size.width,
                  height: (z.maxXZ[1] - z.minXZ[1]) / 8 * g.size.height
                ).position(
                  x: ((z.minXZ[0] + z.maxXZ[0]) / 2 + 5) / 10 * g.size.width,
                  y: ((z.minXZ[1] + z.maxXZ[1]) / 2 + 4) / 8 * g.size.height)
              }
              ForEach(model.snapshot?.tracking?.tracks ?? []) { t in
                Circle().fill(.teal).frame(width: 8, height: 8).position(
                  x: (t.position_xz[0] + 5) / 10 * g.size.width,
                  y: (t.position_xz[1] + 4) / 8 * g.size.height)
              }
            }.contentShape(Rectangle()).gesture(
              DragGesture(minimumDistance: 3).onEnded {
                zones.draw(from: $0.startLocation, to: $0.location, size: g.size)
              }
            ).accessibilityIdentifier("zones-canvas")
          }.frame(width: 400, height: 320).clipShape(RoundedRectangle(cornerRadius: 10))
          Text("X −5…5 m · Z −4…4 m · purple draft · teal pinned · gray historical").font(.caption2)
            .foregroundStyle(.secondary)
          TextField("Catalog name", text: $zones.name).accessibilityIdentifier("zone-name")
          TextField("Revision label", text: $zones.revision).accessibilityIdentifier(
            "zone-revision")
          ForEach(zones.draft.indices, id: \.self) { i in
            HStack {
              Button(zones.draft[i].label) { zones.drawIndex = i }.buttonStyle(.plain)
              Spacer()
              Text(
                String(
                  format: "[%.3f, %.3f] to [%.3f, %.3f]", zones.draft[i].minXZ[0],
                  zones.draft[i].minXZ[1], zones.draft[i].maxXZ[0], zones.draft[i].maxXZ[1])
              ).font(.caption2.monospaced())
            }
          }
          if zones.draft.indices.contains(zones.drawIndex) {
            TextField("Zone label", text: $zones.draft[zones.drawIndex].label)
            HStack {
              Text("Dwell seconds")
              TextField(
                "Seconds", value: $zones.draft[zones.drawIndex].dwellSeconds,
                format: .number.precision(.fractionLength(0...3))
              ).frame(width: 80)
            }
          }
          HStack {
            Button("Add zone") {
              guard zones.draft.count < 16 else { return }
              zones.draft.append(
                ZoneRectangle(
                  id: "zone-\(UUID().uuidString.prefix(8))", label: "New area", minXZ: [0, 0],
                  maxXZ: [1, 1], dwellSeconds: 1))
              zones.drawIndex = zones.draft.count - 1
            }
            Button("Remove selected") {
              guard zones.draft.count > 1 else { return }
              zones.draft.remove(at: zones.drawIndex)
              zones.drawIndex = min(zones.drawIndex, zones.draft.count - 1)
            }.disabled(zones.draft.count <= 1)
            Spacer()
            Button("Review publish") { publishReview = true }.buttonStyle(.borderedProminent).tint(
              .teal
            ).disabled(zones.busy || zones.context == nil || !zones.canPublish)
              .accessibilityIdentifier("zone-publish-review")
          }
        }.frame(width: 400)
        VStack(alignment: .leading, spacing: 10) {
          HStack {
            Text("Verified revisions").font(.headline)
            Spacer()
            Button("Refresh") { actions.submit { [self] in  await zones.refresh() } }.disabled(zones.busy)
          }
          TimelineView(.periodic(from: .now, by: 0.25)) { _ in
            Text(zones.status).font(.callout).foregroundStyle(zones.current ? .teal : .orange)
          }
          ScrollView {
            VStack(alignment: .leading, spacing: 10) {
              ForEach(zones.rows) { row in
                VStack(alignment: .leading, spacing: 5) {
                  Text(row.name).font(.headline)
                  Text(row.digest).font(.caption2.monospaced()).textSelection(.enabled)
                  Text(["device", "app", "service", "stream"].map { row.source[$0]?.string ?? "?" }.joined(separator: " / "))
                    .font(.caption2.monospaced()).lineLimit(2)
                  if zones.active?.id == row.id {
                    Text("Pinned session revision").font(.caption.weight(.semibold)).foregroundStyle(.teal)
                  }
                  Text(row.deleted ? "Deleted at this replica" : row.revision).font(.caption)
                    .foregroundStyle(.secondary)
                  HStack {
                    Button("Use revision") { actions.submit { [self] in  await zones.select(row) } }.disabled(
                      row.deleted || row.context != zones.context || zones.busy
                        || model.pending != nil
                    ).accessibilityIdentifier("zone-select-" + row.digest.prefix(12))
                    Button("Edit copy") { zones.edit(row) }.disabled(row.deleted || zones.busy)
                    Button("Delete", role: .destructive) { deleting = row }.disabled(
                      row.deleted || zones.busy || !zones.owns(row))
                  }
                }.padding(10).background(
                  .gray.opacity(0.07), in: RoundedRectangle(cornerRadius: 10))
              }
            }
          }.frame(maxHeight: 360)
          if let presence = model.snapshot?.tracking?.zone_presence {
            Divider()
            Text("Presence in the pinned revision").font(.headline)
            ForEach(presence.zones) { z in
              Text(
                "\(z.zone.label): \(zones.current ? z.state:"unknown") · \(zones.current ? z.observed:0) observed"
              ).font(.caption)
            }
            Text(
              "\(presence.events.count) historical events; timestamps and original evidence remain unchanged."
            ).font(.caption2).foregroundStyle(.secondary)
          }
          Spacer()
          Text(zones.message).font(.caption).fixedSize(horizontal: false, vertical: true)
        }.frame(width: 405)
      }
    }.padding(24).frame(width: 880, height: 760)
      .foregroundStyle(Color(red: 0.14, green: 0.22, blue: 0.24))
      .background(Color(red: 0.97, green: 0.97, blue: 0.95))
      .fileImporter(isPresented: $importing, allowedContentTypes: [.json]) { r in
        if case .success(let url) = r {
          do {
            try zones.configure(url)
            actions.submit { [self] in  await zones.refresh() }
          } catch { zones.message = error.localizedDescription }
        }
      }
      .confirmationDialog(
        "Publish this reviewed geometry?", isPresented: $publishReview, titleVisibility: .visible
      ) {
        Button("Publish signed revision") { actions.submit { [self] in  await zones.publish() } }
          .accessibilityIdentifier("zone-publish-confirm")
      } message: {
        Text(
          "\(zones.draft.count) zones in the displayed frame. This creates another immutable revision; it does not change the active presence session."
        )
      }
      .alert(
        "Delete this source's revision?",
        isPresented: Binding(get: { deleting != nil }, set: { if !$0 { deleting = nil } })
      ) {
        Button("Cancel", role: .cancel) { deleting = nil }
        Button("Delete revision", role: .destructive) {
          if let row = deleting { actions.submit { [self] in  await zones.delete(row) } }
          deleting = nil
        }
      } message: {
        Text("A signed tombstone is replicated. Historical presence events are retained.")
      }
      .task {
        await zones.refresh()
        while !Task.isCancelled {
          try? await Task.sleep(for: .seconds(2))
          if !Task.isCancelled { await zones.refresh() }
        }
      }

        }.task { await actions.run() }
}
}
