import AppKit
import CryptoKit
import Darwin
import SwiftUI

struct ChangePoint: Codable {
  let captured_s, value_ppm, uncertainty_ppm, valid_until_s: Double
  let sha256: String
}
struct ChangeTrace: Codable, Identifiable {
  var id: String { sensor }
  let sensor, state: String
  let points: [ChangePoint]
  let min_rate_ppm_s, max_rate_ppm_s: Double
}
struct ChangeWindow: Codable {
  let state, reason, input_digest: String
  let evaluated_s, valid_until_s: Double
  let traces: [ChangeTrace]
}
struct ChangeEvent: Codable, Identifiable {
  var id: String { "\(episode)/\(at_s)/\(state)" }
  let episode, state, witness_sha256: String
  let at_s, original_valid_until_s: Double
  enum CodingKeys: String, CodingKey {
    case episode = "id"
    case state, witness_sha256, at_s, original_valid_until_s
  }
}
struct ChangeCandidate: Codable, Identifiable {
  var id: String { gateway }
  let gateway, state, reason: String
  let sha256: String?
  let remaining_s: Double
  let window: ChangeWindow?
  let history: [ChangeEvent]
}
struct ChangeSelection: Codable {
  let state, contract_sha256: String
  let candidates: [ChangeCandidate]
}
struct ChangeSnapshot: Codable {
  let schema, run_id, reader, node, image, image_sha256, mode: String
  let sequence, last_command_sequence: Int
  let now_s, written_unix_s: Double
  let selection: ChangeSelection
  let partition, duplicates, dropout, bias, controls: Bool
}

@MainActor final class GasChangeModel: ObservableObject {
  let session: URL
  @Published var snapshot: ChangeSnapshot?
  @Published var image: NSImage?
  @Published var error: String?
  @Published var tick = 0
  @Published var pending: Int?
  var clock: () -> ContinuousClock.Instant = { .now }
  private var timer: Timer?
  private var deadlines: [String: (String, ContinuousClock.Instant)] = [:]
  private var updated: ContinuousClock.Instant?
  private var lastCommand = 0
  private var qaSequence = 0
  private var pendingAt: ContinuousClock.Instant?
  init(session: URL) { self.session = session }
  var fresh: Bool { updated.map { $0.duration(to: clock()) < .seconds(3) } ?? false }
  func live(_ c: ChangeCandidate) -> Bool {
    fresh && c.window != nil && deadlines[c.gateway].map { clock() < $0.1 } == true
  }
  func remaining(_ c: ChangeCandidate) -> Double {
    guard live(c), let d = deadlines[c.gateway] else { return 0 }
    let x = clock().duration(to: d.1).components
    return max(0, Double(x.seconds) + Double(x.attoseconds) / 1e18)
  }
  var state: String {
    let states = Set((snapshot?.selection.candidates ?? []).filter { live($0) }.map(\.state))
    return states.count == 1 ? states.first! : states.isEmpty ? "unknown" : "mixed"
  }
  var canCommand: Bool {
    fresh && error == nil && pending == nil && snapshot?.controls == true && lastCommand < 4096
  }
  func start() {
    guard timer == nil else { return }
    poll()
    timer = Timer.scheduledTimer(withTimeInterval: 0.1, repeats: true) { [weak self] _ in
      MainActor.assumeIsolated { self?.poll() }
    }
  }
  func stop() {
    timer?.invalidate()
    timer = nil
  }
  private func read(_ name: String, _ limit: Int) throws -> Data {
    let p = session.appendingPathComponent(name)
    let v = try p.resourceValues(forKeys: [.isRegularFileKey, .isSymbolicLinkKey, .fileSizeKey])
    guard v.isRegularFile == true, v.isSymbolicLink != true, (v.fileSize ?? limit + 1) <= limit
    else { throw CocoaError(.fileReadCorruptFile) }
    return try Data(contentsOf: p)
  }
  private func valid(_ s: ChangeSnapshot) -> Bool {
    func hash(_ h: String) -> Bool {
      h.count == 64 && h.allSatisfy { "0123456789abcdef".contains($0) }
    }
    let states = ["unknown", "warming", "rising", "falling", "steady", "mixed"]
    guard s.schema == "wendy.gas-change-demo.v1", !s.run_id.isEmpty, s.run_id.count <= 128,
      s.sequence > 0, s.now_s.isFinite, s.now_s >= 0, s.written_unix_s.isFinite,
      abs(Date().timeIntervalSince1970 - s.written_unix_s) < 10,
      ["east", "west"].contains(s.reader), s.node == (s.reader == "east" ? "node-2" : "node-3"),
      s.controls == (s.reader == "east"), ["scene-0.png", "scene-1.png"].contains(s.image),
      hash(s.image_sha256), hash(s.selection.contract_sha256), states.contains(s.selection.state),
      s.selection.candidates.count == 2,
      Set(s.selection.candidates.map(\.gateway)) == Set(["east", "west"])
    else { return false }
    for c in s.selection.candidates {
      guard states.contains(c.state), c.reason.count <= 1024, c.remaining_s.isFinite,
        c.remaining_s >= 0, c.remaining_s <= 5, c.history.count <= 8
      else { return false }
      for e in c.history {
        guard hash(e.episode), hash(e.witness_sha256),
          ["rising", "falling", "steady", "mixed", "interrupted"].contains(e.state),
          e.at_s.isFinite, e.at_s >= 0, e.at_s <= s.now_s, e.original_valid_until_s.isFinite
        else { return false }
      }
      if let w = c.window {
        guard ["rising", "falling", "steady", "mixed"].contains(c.state), w.state == c.state,
          c.sha256.map(hash) == true, c.remaining_s > 0, w.evaluated_s.isFinite, w.evaluated_s >= 0,
          w.evaluated_s <= s.now_s, w.valid_until_s.isFinite, w.valid_until_s > s.now_s,
          c.remaining_s <= w.valid_until_s - s.now_s + 0.02, hash(w.input_digest),
          w.traces.count == 4, Set(w.traces.map(\.sensor)).count == 4
        else { return false }
        for t in w.traces {
          guard !t.sensor.isEmpty, t.sensor.count <= 128, states.contains(t.state),
            t.points.count == 3, t.min_rate_ppm_s.isFinite, t.max_rate_ppm_s.isFinite
          else { return false }
          var previous = -1.0
          for p in t.points {
            guard p.captured_s.isFinite, p.captured_s > previous, p.captured_s <= w.evaluated_s,
              p.value_ppm.isFinite, (0...10000).contains(p.value_ppm), p.uncertainty_ppm.isFinite,
              (0.1...100).contains(p.uncertainty_ppm), p.valid_until_s.isFinite,
              p.valid_until_s > w.evaluated_s, w.valid_until_s <= p.valid_until_s + 1e-7,
              hash(p.sha256)
            else { return false }
            previous = p.captured_s
          }
        }
      } else if c.state != "unknown" || c.remaining_s != 0 {
        return false
      }
    }
    return true
  }
  func poll() {
    tick &+= 1
    defer { pollQA() }
    do {
      let s = try JSONDecoder().decode(ChangeSnapshot.self, from: read("snapshot.json", 262144))
      guard valid(s) else { throw CocoaError(.fileReadCorruptFile) }
      if let old = snapshot {
        guard old.run_id == s.run_id, old.reader == s.reader,
          old.selection.contract_sha256 == s.selection.contract_sha256, s.sequence >= old.sequence,
          s.now_s >= old.now_s
        else { throw CocoaError(.fileReadCorruptFile) }
      }
      if s.sequence != snapshot?.sequence {
        let raw = try read(s.image, 2_097_152)
        guard SHA256.hash(data: raw).map({ String(format: "%02x", $0) }).joined() == s.image_sha256,
          let img = NSImage(data: raw)
        else { return }
        let now = clock()
        let lag = max(0, Date().timeIntervalSince1970 - s.written_unix_s)
        for c in s.selection.candidates {
          guard let hash = c.sha256, c.window != nil else { continue }
          let until = now.advanced(by: .seconds(max(0, c.remaining_s - lag)))
          if let old = deadlines[c.gateway], old.0 == hash {
            deadlines[c.gateway] = (hash, min(old.1, until))
          } else {
            deadlines[c.gateway] = (hash, until)
          }
        }
        snapshot = s
        image = img
        updated = now
        lastCommand = max(lastCommand, s.last_command_sequence)
        error = nil
      }
      if let seq = pending {
        struct Receipt: Decodable {
          let sequence: Int
          let accepted: Bool
          let error: String
        }
        if let raw = try? read(String(format: "receipts/%06d.json", seq), 4096),
          let receipt = try? JSONDecoder().decode(Receipt.self, from: raw), receipt.sequence == seq
        {
          pending = nil
          if !receipt.accepted { error = receipt.error }
        } else if pendingAt.map({ $0.duration(to: clock()) > .seconds(5) }) == true {
          pending = nil
          error = "Control receipt timed out"
        }
      }
    } catch { self.error = "Cannot validate the current local simulation snapshot" }
  }
  @discardableResult func command(_ action: String) -> Bool {
    guard canCommand, let s = snapshot,
      [
        "ramp", "decline", "steady", "spike", "opposing", "bias", "dropout", "partition",
        "duplicates", "restart_east",
      ].contains(action)
    else { return false }
    let seq = lastCommand + 1
    let value: [String: Any] = [
      "run_id": s.run_id, "sequence": seq, "action": action, "target": "",
    ]
    do {
      let raw = try JSONSerialization.data(withJSONObject: value)
      let p = session.appendingPathComponent(String(format: "commands/%06d.json", seq))
      let tmp = p.deletingLastPathComponent().appendingPathComponent(
        ".command-" + UUID().uuidString)
      try raw.write(to: tmp, options: .withoutOverwriting)
      defer { try? FileManager.default.removeItem(at: tmp) }
      guard link(tmp.path, p.path) == 0 else { return false }
      lastCommand = seq
      pending = seq
      pendingAt = clock()
      return true
    } catch {
      self.error = "Could not write simulation control"
      return false
    }
  }
  private func pollQA() {
    struct Request: Decodable {
      let sequence: Int
      let action: String
    }
    guard let raw = try? read("native-qa.json", 4096),
      let q = try? JSONDecoder().decode(Request.self, from: raw), q.sequence > qaSequence,
      q.sequence <= 4096
    else { return }
    qaSequence = q.sequence
    var accepted = false
    var capture = ""
    if q.action == "capture" {
      let host = NSHostingView(
        rootView: GasChangeContent(model: self).environment(\.colorScheme, .light))
      host.appearance = NSAppearance(named: .aqua)
      host.frame = CGRect(x: 0, y: 0, width: 1500, height: 960)
      host.layoutSubtreeIfNeeded()
      if let bitmap = host.bitmapImageRepForCachingDisplay(in: host.bounds) {
        host.cacheDisplay(in: host.bounds, to: bitmap)
        if let png = bitmap.representation(using: .png, properties: [:]) {
          capture = String(format: "native-%04d.png", q.sequence)
          do {
            try png.write(to: session.appendingPathComponent(capture), options: .atomic)
            accepted = true
          } catch {}
        }
      }
    } else {
      accepted = command(q.action)
    }
    let receipt: [String: Any] = [
      "sequence": q.sequence, "accepted": accepted, "action": q.action, "capture": capture,
      "state": state, "snapshot_sequence": snapshot?.sequence ?? 0,
    ]
    if let raw = try? JSONSerialization.data(withJSONObject: receipt) {
      try? raw.write(
        to: session.appendingPathComponent(String(format: "native-receipt-%04d.json", q.sequence)),
        options: .atomic)
    }
  }
}
private let changeTeal = Color(red: 0.04, green: 0.43, blue: 0.38)
struct GasChangeContent: View {
  @StateObject private var model: GasChangeModel
  private var captureMode = false
  init(session: URL) { _model = StateObject(wrappedValue: GasChangeModel(session: session)) }
  init(model: GasChangeModel) {
    _model = StateObject(wrappedValue: model)
    captureMode = true
  }
  private func color(_ state: String) -> Color {
    state == "rising"
      ? .orange : state == "falling" ? .blue : state == "steady" ? changeTeal : .secondary
  }
  var body: some View {
    VStack(alignment: .leading, spacing: 18) {
      HStack {
        VStack(alignment: .leading, spacing: 5) {
          Text("WENDY WORLD / TEMPORAL SENSOR FUSION").font(.system(size: 11, weight: .semibold))
            .tracking(2).foregroundStyle(changeTeal)
          Text("A change needs a history.").font(.system(size: 32, weight: .semibold))
          Text("Four commissioned sensors · original capture times · independent gateway histories")
            .font(.system(size: 13)).foregroundStyle(.secondary)
        }
        Spacer()
        VStack(alignment: .trailing) {
          Text("OPERATOR \((model.snapshot?.reader ?? "—").uppercased())").font(
            .system(size: 12, weight: .semibold))
          Text(model.snapshot?.node ?? "Connecting").font(.system(size: 12)).foregroundStyle(
            .secondary)
          Text("SIMULATED / ANALYTIC GAS").font(.system(size: 10, weight: .bold)).padding(8)
            .background(changeTeal.opacity(0.1)).clipShape(Capsule())
        }
      }
      HStack(alignment: .top, spacing: 20) {
        VStack(alignment: .leading, spacing: 12) {
          if let image = model.image {
            Image(nsImage: image).resizable().aspectRatio(contentMode: .fit)
              .frame(height: 430).frame(maxWidth: .infinity).background(.black)
              .clipShape(RoundedRectangle(cornerRadius: 12))
          } else {
            Rectangle().fill(.gray.opacity(0.1)).frame(height: 430)
          }
          HStack {
            Text("MuJoCo geometry and sensor placements").font(.system(size: 12, weight: .medium))
            Spacer()
            Text("No motion authority").font(.system(size: 11)).foregroundStyle(.secondary)
          }
          VStack(alignment: .leading, spacing: 10) {
            HStack {
              Circle().fill(color(model.state)).frame(width: 10, height: 10)
              Text(model.state.uppercased()).font(.system(size: 16, weight: .bold)).tracking(1)
              Spacer()
              Text(model.snapshot?.mode.uppercased() ?? "WAITING").font(
                .system(size: 11, weight: .medium)
              ).foregroundStyle(.secondary)
            }
            Text(
              "A rise requires three or more original captures from every sensor, with three sensors agreeing across every interval. Missing or stale data means unknown — never safe."
            ).font(.system(size: 12)).foregroundStyle(.secondary).fixedSize(
              horizontal: false, vertical: true)
            LazyVGrid(
              columns: [GridItem(.flexible()), GridItem(.flexible()), GridItem(.flexible())],
              spacing: 8
            ) {
              control("Sustained rise", "ramp")
              control("Declining values", "decline")
              control("Hold steady", "steady")
              control("One transient spike", "spike")
              control("Opposing sensor", "opposing")
              control(model.snapshot?.bias == true ? "Remove fixed bias" : "Add fixed bias", "bias")
              control(
                model.snapshot?.dropout == true ? "Restore sensor D" : "Drop sensor D", "dropout")
              control(
                model.snapshot?.partition == true ? "Reconnect replicas" : "Partition east",
                "partition")
              control(
                model.snapshot?.duplicates == true ? "Resume originals" : "Repeat old packets",
                "duplicates")
            }
            HStack {
              Button("Restart east history") { model.command("restart_east") }.disabled(
                !model.canCommand)
              Spacer()
              Text(
                model.snapshot?.controls == true
                  ? (model.pending == nil
                    ? "Controls affect this synthetic fixture only"
                    : "Waiting for control receipt…") : "Independent read-only operator"
              ).font(.system(size: 10)).foregroundStyle(.secondary)
            }
          }.padding(16).background(.white).clipShape(RoundedRectangle(cornerRadius: 12))
        }.frame(maxWidth: .infinity)
        ScrollView {
          VStack(alignment: .leading, spacing: 12) {
            ForEach(model.snapshot?.selection.candidates ?? []) { c in candidate(c) }
            if let error = model.error {
              Text(error).font(.system(size: 11)).foregroundStyle(.orange)
            }
            Text(
              "Published windows retain gateway-attested original components. Reproduction checks the rule, not independent sensor authenticity. Region estimation and immediate robot gas policy are separate."
            ).font(.system(size: 11)).foregroundStyle(.secondary).fixedSize(
              horizontal: false, vertical: true)
          }
        }.frame(width: 555, height: 720)
      }.frame(height: 720, alignment: .top)
      Spacer(minLength: 0)
      HStack {
        Text(
          "Bounded history · 4 sensors × 3 originals · 0.6 s minimum span · 1.5 s maximum gap · 1 ppm/s rule"
        ).font(.system(size: 11)).foregroundStyle(.secondary)
        Spacer()
        Text("No exposure threshold or physical leak diagnosis").font(
          .system(size: 11, weight: .medium)
        ).foregroundStyle(changeTeal)
      }
    }.padding(28).foregroundStyle(Color(red: 0.12, green: 0.20, blue: 0.22)).background(
      Color(red: 0.95, green: 0.96, blue: 0.94)
    ).onAppear { if !captureMode { model.start() } }.onDisappear {
      if !captureMode { model.stop() }
    }
  }
  private func control(_ label: String, _ action: String) -> some View {
    Button(label) { model.command(action) }.font(.system(size: 11)).buttonStyle(.bordered).disabled(
      !model.canCommand
    ).frame(maxWidth: .infinity)
  }
  private func candidate(_ c: ChangeCandidate) -> some View {
    VStack(alignment: .leading, spacing: 7) {
      HStack {
        Text("\(c.gateway.uppercased()) GATEWAY").font(.system(size: 11, weight: .bold)).tracking(1)
        Spacer()
        Text(model.live(c) ? c.state.uppercased() : "UNKNOWN").font(
          .system(size: 12, weight: .bold)
        ).foregroundStyle(color(model.live(c) ? c.state : "unknown"))
      }
      Text(
        model.live(c)
          ? String(format: "Original support expires in %.1f s", model.remaining(c))
          : "No current complete history. Waiting for fresh originals."
      ).font(.system(size: 11)).foregroundStyle(.secondary)
      if let w = c.window, model.live(c) {
        ChangeTraces(traces: w.traces).frame(height: 80)
        HStack {
          Text("CONSERVATIVE INTERVAL RATES").font(.system(size: 9, weight: .semibold))
          Spacer()
          Text("ppm/s").font(.system(size: 9))
        }.foregroundStyle(.secondary)
        ForEach(w.traces) { t in
          HStack {
            Text(t.sensor.uppercased()).font(.system(size: 11, weight: .semibold)).frame(width: 20)
            Text(t.state).font(.system(size: 11)).foregroundStyle(.secondary)
            Spacer()
            Text(String(format: "%+.1f … %+.1f", t.min_rate_ppm_s, t.max_rate_ppm_s)).font(
              .system(size: 11)
            ).monospacedDigit()
          }
        }
        Text("Witness \(w.input_digest.prefix(24))…").font(.system(size: 9, design: .monospaced))
          .foregroundStyle(.secondary)
      }
      Divider()
      Text("LOCAL EPISODE HISTORY · NOT CURRENT EVIDENCE").font(.system(size: 9, weight: .semibold))
        .tracking(0.5).foregroundStyle(.secondary)
      if c.history.isEmpty {
        Text("No sustained rise observed by this reader yet.").font(.system(size: 11))
          .foregroundStyle(.secondary)
      } else {
        ForEach(Array(c.history.suffix(3))) { e in
          HStack {
            Text(
              (e.state == "interrupted" ? "observed " : "capture ")
                + String(format: "%.2f s", e.at_s)
            ).font(.system(size: 10, design: .monospaced))
            Text(e.state).font(.system(size: 11, weight: .medium))
            Spacer()
            Text(String(e.episode.prefix(10))).font(.system(size: 9, design: .monospaced))
              .foregroundStyle(.secondary)
          }
        }
      }
    }.padding(15).background(.white).clipShape(RoundedRectangle(cornerRadius: 12))
  }
}
struct ChangeTraces: View {
  let traces: [ChangeTrace]
  var body: some View {
    Canvas { context, size in
      let points = traces.flatMap(\.points)
      guard let lo = points.map(\.captured_s).min(), let hi = points.map(\.captured_s).max(),
        hi > lo
      else { return }
      let minV = max(0, (points.map(\.value_ppm).min() ?? 0) - 3)
      let maxV = (points.map(\.value_ppm).max() ?? 1) + 3
      let colors: [Color] = [.teal, .blue, .orange, .purple]
      for (i, t) in traces.enumerated() {
        var path = Path()
        for (j, p) in t.points.enumerated() {
          let q = CGPoint(
            x: 8 + (p.captured_s - lo) / (hi - lo) * (size.width - 16),
            y: size.height - 12 - (p.value_ppm - minV) / (maxV - minV) * (size.height - 24))
          if j == 0 { path.move(to: q) } else { path.addLine(to: q) }
          context.fill(
            Path(ellipseIn: CGRect(x: q.x - 3, y: q.y - 3, width: 6, height: 6)),
            with: .color(colors[i % 4]))
        }
        context.stroke(path, with: .color(colors[i % 4]), lineWidth: 2)
      }
    }.background(Color.gray.opacity(0.05)).clipShape(RoundedRectangle(cornerRadius: 7))
  }
}
