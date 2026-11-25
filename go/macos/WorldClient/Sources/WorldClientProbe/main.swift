import Foundation
import Darwin
import WorldClient

enum ProbeError: Error { case usage, missingData(String), evidenceUnavailable }

struct MediaReport: Encodable {
    let sha256: String
    let contentType: String
    let bytes: Int
}

struct ProbeReport: Encodable {
    let mode: String
    let components: Int
    var lastKnown: Bool? = nil
    var networkRequests: Int? = nil
    var scopeMatches: Bool? = nil
    var fetchedAt: String? = nil
    var componentTypes: [String]? = nil
    var sitePublications: Int? = nil
    var missionPhase: String? = nil
    var inspectionSamples: Int? = nil
    var verifiedEvidence: [MediaReport]? = nil
    var scopedCacheRoundTrip: Bool? = nil
    var sensorLineageMatched: Bool? = nil
    var transport: String? = nil
}

@main
struct WorldClientProbe {
    static func main() async {
        do { try await run() }
        catch {
            try? FileHandle.standardError.write(contentsOf: Data("World client probe failed: \(error)\n".utf8))
            exit(1)
        }
    }

    static func run() async throws {
        let environment = ProcessInfo.processInfo.environment
        guard let mode = environment["WORLD_PROBE_MODE"], ["online", "offline", "annotation-draft", "annotation-status", "annotation-sync", "annotation-clock", "annotation-untimed"].contains(mode),
              let profilePath = environment["WORLD_PROFILE_PATH"],
              let cachePath = environment["WORLD_CACHE_DIR"] else { throw ProbeError.usage }
        let profile = try JSONValue.decode(Data(contentsOf: URL(fileURLWithPath: profilePath)), limit: 512 << 10)
        let credentials = try WorldCredentials(data: (profile["credentials"] ?? profile).encoded())
        if mode.hasPrefix("annotation-") {
            try await AnnotationProbe.run(mode: mode, profile: profile, credentials: credentials, directory: URL(fileURLWithPath: cachePath))
            return
        }
        let cache = OfflineSnapshotCache(directory: URL(fileURLWithPath: cachePath))
        if mode == "offline" {
            guard let cached = try cache.load(scopeID: credentials.scopeID), cached.isLastKnown else {
                throw ProbeError.missingData("offline snapshot")
            }
            try output(ProbeReport(mode: "offline", components: cached.snapshot.components.count,
                        lastKnown: cached.isLastKnown, networkRequests: 0,
                        scopeMatches: cached.scopeID == credentials.scopeID,
                        fetchedAt: ISO8601DateFormatter().string(from: cached.fetchedAt)))
            return
        }
        let client = WorldClient(credentials: credentials)
        async let snapshotRead = client.query()
        async let sitesRead = client.sites()
        async let tasksRead = client.taskRelay()
        let (snapshot, sites, tasks) = try await (snapshotRead, sitesRead, tasksRead)
        guard snapshot.components.filter({ $0.type == "wendy.sensor.v1" }).count >= 4,
              let publications = sites["publications"]?.array, !publications.isEmpty,
              let task = tasks["entries"]?.array?.first(where: { $0["task_id"]?.string == "native-inspection" }),
              task["status"]?["record"]?["status"]?["Phase"]?.string == "succeeded",
              let samples = task["status"]?["record"]?["result"]?["samples"]?.array, samples.count == 4 else {
            throw ProbeError.missingData("live sensors, site or completed inspection")
        }
        let expectedKinds = ["camera-a": "camera", "camera-b": "camera", "gas-1": "gas", "audio-1": "audio"]
        let result = task["status"]?["record"]?["result"]
        guard let run = result?["run_id"]?.string, !run.isEmpty,
              let frame = result?["frame"], frame["id"]?.string == "office-floor", frame["revision"]?.string == "office-frame-v1",
              let package = samples.first?["package_sha256"]?.string, package.count == 64,
              publications.contains(where: { $0["component"]?["payload"]?["package_sha256"]?.string == package }),
              Set(samples.compactMap { $0["sensor_id"]?.string }) == Set(expectedKinds.keys) else {
            throw ProbeError.missingData("expected sensor identities and package")
        }
        for sample in samples {
            guard let id = sample["sensor_id"]?.string, sample["kind"]?.string == expectedKinds[id],
                  sample["run_id"]?.string == run, sample["package_sha256"]?.string == package,
                  sample["frame"] == frame, sample["site_id"]?.string == credentials.site,
                  snapshot.components.contains(where: {
                      let live = $0.raw["payload"]
                      return live?["sensor_id"]?.string == id && live?["kind"]?.string == expectedKinds[id]
                          && live?["run_id"]?.string == run && live?["package_sha256"]?.string == package && live?["frame"] == frame
                  }) else { throw ProbeError.missingData("matching live and retained sensor lineage") }
        }
        let captures = try (profile["evidence_sources"]?.array ?? []).map { try WorldClient(credentials: WorldCredentials(data: $0.encoded())) }
        var references: [String: (String, Int)] = [:]
        for sample in samples where sample["kind"]?.string != "gas" {
            let expectedType = sample["kind"]?.string == "camera" ? "image/png" : "audio/wav"
            guard let media = sample["evidence"], let digest = media["sha256"]?.string,
                  media["content_type"]?.string == expectedType, let size = media["size"]?.int,
                  size > 0, size <= 8 << 20 else { throw ProbeError.missingData("expected PNG and WAV media") }
            references[digest] = (expectedType, Int(size))
        }
        guard references.count == 3 else { throw ProbeError.missingData("three distinct inspection media") }
        var verified: [MediaReport] = []
        for (digest, reference) in references.sorted(by: { $0.key < $1.key }) {
            var evidence: VerifiedEvidence?
            for capture in captures {
                do { evidence = try await capture.downloadEvidence(digest: digest, maxBytes: reference.1); break } catch { continue }
            }
            guard let evidence, evidence.contentType == reference.0, evidence.data.count == reference.1 else { throw ProbeError.evidenceUnavailable }
            verified.append(MediaReport(sha256: digest, contentType: evidence.contentType, bytes: evidence.data.count))
        }
        let cached = CachedWorldReadModel(snapshot: snapshot, sites: sites, tasks: tasks, fetchedAt: Date(), scopeID: credentials.scopeID)
        try cache.save(cached)
        guard let reopened = try cache.load(scopeID: credentials.scopeID),
              reopened.snapshot == snapshot, reopened.tasks == tasks, reopened.sites == sites else {
            throw ProbeError.missingData("cache round trip")
        }
        try output(ProbeReport(mode: "online", components: snapshot.components.count,
                    componentTypes: Array(Set(snapshot.components.compactMap(\.type))).sorted(),
                    sitePublications: publications.count, missionPhase: "succeeded", inspectionSamples: samples.count,
                    verifiedEvidence: verified, scopedCacheRoundTrip: true, sensorLineageMatched: true,
                    transport: "explicit simulation HTTP; records validated by gateway"))
    }

    static func output<T: Encodable>(_ value: T) throws {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        var data = try encoder.encode(value)
        data.append(10)
        try FileHandle.standardOutput.write(contentsOf: data)
    }
}
