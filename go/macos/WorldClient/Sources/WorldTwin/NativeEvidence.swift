import Foundation
import Darwin
import SwiftUI
import AppKit
import AVFoundation
import CryptoKit
import WorldClient

/// Metadata projected from the same original reader result as its containing
/// track/hazard. The SDK retains the exact capture owner and proof cache key.
struct DemoNativeEvidence: Decodable, Identifiable, Equatable {
    let label: String
    let role: String
    let sampleTime: String?
    let reference: NativeEvidenceReference
    var id: String { reference.cacheKey + ":" + role }
    var sourceLabel: String { reference.source["stream"]?.string ?? label }
    var roleLabel: String { role == "original" ? "Original image" : (role == "previous" ? "Earlier observation" : "Captured sample") }
    enum CodingKeys: String, CodingKey { case label,role,source,sha256,size,content_type,proof_envelope_sha256,sample_time }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        label = try c.decode(String.self, forKey: .label)
        role = try c.decode(String.self, forKey: .role)
        sampleTime = try c.decodeIfPresent(String.self, forKey: .sample_time)
        guard !label.isEmpty, label.utf8.count <= 160, ["active","previous","original"].contains(role),
              sampleTime.map({ $0.utf8.count <= 64 && demoEvidenceTime($0) != nil }) ?? true else { throw DemoError.invalidFile }
        reference = try NativeEvidenceReference(source: c.decode(JSONValue.self, forKey: .source),
            sha256: c.decode(String.self, forKey: .sha256), size: c.decode(Int.self, forKey: .size),
            contentType: c.decode(String.self, forKey: .content_type), proofEnvelopeSHA256: c.decode(String.self, forKey: .proof_envelope_sha256))
        guard role != "original" || reference.contentType == "image/png" else { throw DemoError.invalidFile }
    }
}

func demoEvidenceTime(_ raw: String) -> Date? {
    let f = ISO8601DateFormatter(); f.formatOptions = [.withInternetDateTime,.withFractionalSeconds]
    if let date = f.date(from: raw) { return date }
    f.formatOptions = [.withInternetDateTime]; return f.date(from: raw)
}
func validNativeEvidence(_ refs: [DemoNativeEvidence], original: String?) -> Bool {
    refs.count <= 16 && Set(refs.map(\.id)).count == refs.count && (refs.isEmpty || original != nil)
}

struct DemoOriginalSelection: Identifiable {
    let originalEnvelopeSHA256: String
    let runID: String
    let title: String
    let observationTime: String
    let evidence: [DemoNativeEvidence]
    let observationID: String?
    var id: String { originalEnvelopeSHA256 + ":" + (observationID ?? title) }
    var preferredEvidence: DemoNativeEvidence? {
        evidence.first { $0.reference.source["stream"]?.string == observationID && $0.role == "active" } ?? evidence.first
    }
}

@MainActor final class DemoNativeMedia: ObservableObject {
    @Published private(set) var selected: DemoNativeEvidence?
    @Published private(set) var preview: VerifiedEvidence?
    @Published private(set) var loading = false
    @Published private(set) var message: String?
    @Published var showsSourceDetails = false
    private let cache: InspectionMediaCache?
    private let client: WorldClient?
    private let configured: Bool
    private var generation = 0

    init(profile: URL?, cacheDirectory: URL) {
        var cache: InspectionMediaCache?, client: WorldClient?
        do {
            if let profile {
                let fd = Darwin.open(profile.path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
                guard fd >= 0 else { throw DemoError.invalidFile }
                let file = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
                defer { try? file.close() }
                var info = stat()
                guard fstat(fd, &info) == 0, info.st_mode & S_IFMT == S_IFREG, info.st_size > 0, info.st_size <= 64 << 10,
                      let data = try file.read(upToCount: (64 << 10)+1), data.count <= 64 << 10 else { throw DemoError.invalidFile }
                let object = try JSONValue.decode(data, limit: 64 << 10)
                guard try WorldReadMode(profile: object) == .nativeInference, let raw = object["credentials"] else { throw DemoError.invalidFile }
                let credentials = try WorldCredentials(data: raw.encoded())
                guard credentials.scopes.contains("read"), credentials.scopes.contains("evidence-read") else { throw DemoError.invalidFile }
                let scope = SHA256.hash(data: Data((credentials.scopeID+":native_inference").utf8)).map { String(format:"%02x",$0) }.joined()
                cache = try InspectionMediaCache(directory: cacheDirectory, scopeID: scope)
                client = WorldClient(credentials: credentials)
            }
        } catch { message = "Media connection is unavailable." }
        self.cache = cache; self.client = client; configured = client != nil
    }
    // The same SDK cache path can reopen an exact selection without a network.
    init(cache: InspectionMediaCache?, client: WorldClient?) {
        self.cache = cache; self.client = client; configured = client != nil
    }
    func open(_ item: DemoNativeEvidence) async {
        expire()
        if selected == item, preview != nil { return }
        generation += 1; let request = generation
        selected = item; preview = nil; loading = true; message = nil
        defer { if request == generation { loading = false } }
        do {
            if let cache, let value = try await cache.load(reference: item.reference) {
                try Task.checkCancellation(); guard request == generation else { return }
                preview = value; message = "Saved on this Mac"; return
            }
            try Task.checkCancellation(); guard request == generation else { return }
            guard let client else { message = configured ? "Media is unavailable. Try again when connected." : "Not saved on this Mac. No media connection is configured."; return }
            let value = try await client.downloadNativeEvidence(item.reference)
            try Task.checkCancellation(); guard request == generation else { return }
            if let cache {
                try await cache.store(value, reference: item.reference)
                guard let retained = try await cache.load(reference: item.reference) else { throw InspectionMediaCacheError.expired }
                try Task.checkCancellation(); guard request == generation else { return }
                preview = retained
            } else { preview = value }
            message = "Captured media"
        } catch is CancellationError {
            if request == generation { message = "Loading canceled." }
        } catch InspectionMediaCacheError.expired {
            if request == generation { preview = nil; message = "This media has expired." }
        } catch {
            if request == generation { preview = nil; message = "Media is unavailable. Try again when connected." }
        }
    }
    func expire(now: Date = Date()) {
        if let value = preview, value.retainUntil <= now { preview = nil; message = "This media has expired." }
    }
    func dismiss() { generation += 1; preview = nil; selected = nil; loading = false; showsSourceDetails = false }
}

struct DemoNativeEvidenceSheet: View {
    @State private var actions = TwinActions()

    let selection: DemoOriginalSelection
    @ObservedObject var media: DemoNativeMedia
    @Environment(\.dismiss) private var dismiss
    @State private var player: AVAudioPlayer?
    @State private var playing = false
    @State private var playbackMessage: String?
    private let teal = Color(red:0.12,green:0.47,blue:0.43)
    var body: some View {
        Group {

        VStack(alignment:.leading,spacing:18) {
            Text("Native-agent capture retention is not supported by standalone investigation pins. Local saved media remains separate.").font(.caption).foregroundStyle(.secondary)
            HStack(alignment:.top) {
                VStack(alignment:.leading,spacing:5) {
                    Text(selection.title).font(.title2.weight(.semibold))
                    Text("SIMULATION OBSERVATION · "+selection.observationTime).font(.caption).foregroundStyle(.secondary)
                }
                Spacer(); Button("Done") { dismiss() }.keyboardShortcut(.cancelAction)
            }
            HStack(alignment:.top,spacing:22) {
                ScrollView {
                    VStack(alignment:.leading,spacing:8) {
                        ForEach(selection.evidence) { item in
                            Button { stopAudio(); actions.submit { [self] in  await media.open(item) } } label: {
                                VStack(alignment:.leading,spacing:4) {
                                    Label(item.label,systemImage:item.reference.contentType == "audio/wav" ? "waveform":"photo").font(.system(size:12,weight:.medium))
                                    Text(item.roleLabel).font(.caption).foregroundStyle(.secondary)
                                }.frame(maxWidth:.infinity,alignment:.leading).padding(11)
                                    .background(media.selected?.id == item.id ? teal.opacity(0.12):Color.secondary.opacity(0.04),in:RoundedRectangle(cornerRadius:9))
                            }.buttonStyle(.plain)
                        }
                        if selection.evidence.isEmpty { Text("No retained media references for this observation.").font(.callout).foregroundStyle(.secondary) }
                    }
                }.frame(width:220)
                VStack(alignment:.leading,spacing:14) {
                    if let value=media.preview {
                        if value.contentType.hasPrefix("image/"),let image=DemoInspectionMedia.image(value.data) {
                            Image(nsImage:image).resizable().scaledToFit().frame(maxWidth:.infinity,maxHeight:360).background(.black.opacity(0.04),in:RoundedRectangle(cornerRadius:12))
                        } else if value.contentType == "audio/wav" {
                            VStack(spacing:20) {
                                Image(systemName:"waveform").font(.system(size:72,weight:.ultraLight)).foregroundStyle(teal)
                                Button(playing ? "Stop audio":"Play audio") {
                                    guard value.retainUntil > Date() else { stopAudio(); media.expire(); return }
                                    if playing { stopAudio() } else {
                                        do { let audio=try AVAudioPlayer(data:value.data); audio.prepareToPlay(); playing=audio.play(); player=audio; playbackMessage=playing ? nil:"Audio playback could not start." }
                                        catch { playbackMessage="This audio sample could not be decoded." }
                                    }
                                }.buttonStyle(.borderedProminent).tint(teal)
                            }.frame(maxWidth:.infinity,minHeight:230)
                        } else { Text("This image could not be displayed.") }
                        Text("SHA-256 verified · Retained until "+value.retainUntil.formatted(date:.abbreviated,time:.standard)).font(.caption).foregroundStyle(.secondary)
                    } else {
                        ContentUnavailableView("Captured evidence",systemImage:media.loading ? "arrow.down.circle":"photo.on.rectangle",description:Text(media.message ?? "Choose an image or audio sample."))
                            .frame(maxWidth:.infinity,minHeight:270)
                    }
                    if media.loading { ProgressView("Loading media…").controlSize(.small) }
                    if let message=media.message,media.preview != nil { Text(message).font(.caption).foregroundStyle(.secondary) }
                    if let playbackMessage { Text(playbackMessage).font(.caption).foregroundStyle(.secondary) }
                    if let item=media.selected {
                        DisclosureGroup("Source details",isExpanded:$media.showsSourceDetails) {
                            VStack(alignment:.leading,spacing:5) {
                                Text(item.reference.source["device"]?.string ?? "")
                                Text((item.reference.source["app"]?.string ?? "")+" / "+(item.reference.source["service"]?.string ?? ""))
                                if let time=item.sampleTime { Text("Simulation capture time: "+time) }
                                Text("Original result: "+selection.originalEnvelopeSHA256)
                                Text("Media: "+item.reference.sha256)
                                Text("Capture proof: "+item.reference.proofEnvelopeSHA256)
                            }.font(.system(size:10,design:.monospaced)).textSelection(.enabled).fixedSize(horizontal:false,vertical:true)
                        }.font(.caption)
                    }
                }.frame(maxWidth:.infinity,alignment:.leading)
            }
        }.padding(28).frame(width:900,height:650).background(Color(red:0.97,green:0.97,blue:0.95)).preferredColorScheme(.light)
            .task(id:selection.id) { if let item=selection.preferredEvidence { await media.open(item) } }
            .onDisappear { stopAudio(); media.dismiss() }
            .onChange(of:media.preview?.retainUntil) { _,_ in stopAudio() }
            .onReceive(Timer.publish(every:0.1,on:.main,in:.common).autoconnect()) { now in
                media.expire(now:now)
                if media.preview == nil { stopAudio() }
                else if playing && player?.isPlaying != true { playing=false }
            }

        }.task { await actions.run() }
}
    private func stopAudio() { player?.stop(); player=nil; playing=false; playbackMessage=nil }
}

/// Private simulation evidence has its own local provenance. It never fabricates
/// a native-agent envelope or enters the authenticated native media cache.
struct PatrolEvidenceFile {
    let sha256:String
    let format:String
    let data:Data
    static func load(_ digest:String,session:URL) throws -> PatrolEvidenceFile {
        guard digest.count==64,digest.utf8.allSatisfy({(48...57).contains($0) || (97...102).contains($0)}) else {throw DemoError.invalidFile}
        let base=Darwin.open(session.path,O_RDONLY|O_DIRECTORY|O_NOFOLLOW)
        guard base>=0 else {throw DemoError.invalidFile}
        defer {Darwin.close(base)}
        let directory=openat(base,"evidence",O_RDONLY|O_DIRECTORY|O_NOFOLLOW)
        guard directory>=0 else {throw DemoError.invalidFile}
        defer {Darwin.close(directory)}
        for format in ["png","wav","json","bin"] {
            let fd=openat(directory,digest+"."+format,O_RDONLY|O_NOFOLLOW|O_NONBLOCK)
            if fd<0 {if errno==ENOENT {continue};throw DemoError.invalidFile}
            defer {Darwin.close(fd)}
            var before=stat()
            guard fstat(fd,&before)==0,before.st_mode&S_IFMT==S_IFREG,before.st_uid==geteuid(),before.st_nlink==1,
                  before.st_size>0,before.st_size<=8<<20 else {throw DemoError.invalidFile}
            var data=Data(count:Int(before.st_size))
            let count=data.withUnsafeMutableBytes {buffer -> Int in
                var offset=0
                while offset<buffer.count {
                    let read=Darwin.read(fd,buffer.baseAddress!.advanced(by:offset),buffer.count-offset)
                    if read<=0 {return -1};offset+=read
                }
                return offset
            }
            var after=stat()
            guard count==data.count,fstat(fd,&after)==0,after.st_size==before.st_size,
                  after.st_mtimespec.tv_sec==before.st_mtimespec.tv_sec,after.st_mtimespec.tv_nsec==before.st_mtimespec.tv_nsec,
                  after.st_ctimespec.tv_sec==before.st_ctimespec.tv_sec,after.st_ctimespec.tv_nsec==before.st_ctimespec.tv_nsec,
                  SHA256.hash(data:data).map({String(format:"%02x",$0)}).joined()==digest else {throw DemoError.invalidFile}
            return PatrolEvidenceFile(sha256:digest,format:format,data:data)
        }
        throw DemoError.invalidFile
    }
}

struct PatrolEvidenceSheet:View {
    let digest:String
    let session:URL
    @Environment(\.dismiss) private var dismiss
    @State private var file:PatrolEvidenceFile?
    @State private var message:String?
    @State private var player:AVAudioPlayer?
    init(digest:String,session:URL) {
        self.digest=digest;self.session=session
        do {
            _file=State(initialValue:try PatrolEvidenceFile.load(digest,session:session))
            _message=State(initialValue:nil)
        } catch {
            _file=State(initialValue:nil)
            _message=State(initialValue:"Evidence is unavailable or its original digest could not be verified.")
        }
    }
    var body:some View {
        VStack(alignment:.leading,spacing:18) {
            HStack {
                VStack(alignment:.leading,spacing:5) {
                    Text("Patrol evidence").font(.title2.weight(.semibold))
                    Text(file == nil ? "Captured simulation original":"Captured simulation original · SHA-256 verified locally").font(.caption).foregroundStyle(.secondary)
                }
                Spacer();Button("Done") {dismiss()}.keyboardShortcut(.cancelAction)
            }
            if let file {
                if file.format=="png",let image=DemoInspectionMedia.image(file.data) {
                    Image(nsImage:image).resizable().scaledToFit().frame(maxWidth:.infinity,maxHeight:410)
                        .background(.black.opacity(0.05),in:RoundedRectangle(cornerRadius:10))
                } else if file.format=="wav" {
                    VStack(spacing:18) {
                        Image(systemName:"waveform").font(.system(size:60,weight:.light)).foregroundStyle(.teal)
                        Text("Generated audio captured at the simulated microphone pose.").font(.callout)
                        TimelineView(.periodic(from:.now,by:0.1)) { _ in
                            Button {
                                if player?.isPlaying==true {player?.stop()}
                                else {
                                    do {
                                        let sound=try AVAudioPlayer(data:file.data)
                                        sound.prepareToPlay();player=sound;message=nil
                                        if !sound.play() {message="Audio playback could not start."}
                                    } catch {message="Verified audio could not be decoded."}
                                }
                            } label: {Label(player?.isPlaying==true ? "Stop audio":"Play audio",systemImage:player?.isPlaying==true ? "stop.fill":"play.fill")}
                            .buttonStyle(.borderedProminent).tint(.teal)
                        }
                    }.frame(maxWidth:.infinity,minHeight:270)
                } else if file.format=="json",let text=String(data:file.data,encoding:.utf8) {
                    ScrollView {
                        Text(String(text.prefix(24_000))).font(.system(size:11,design:.monospaced)).textSelection(.enabled)
                            .frame(maxWidth:.infinity,alignment:.leading)
                    }.frame(height:350)
                    if text.count>24_000 {Text("Preview shows the first 24,000 characters. The complete original remains in the session.").font(.caption).foregroundStyle(.secondary)}
                } else {Text("Verified original retained. This format has no visual preview.").font(.callout)}
                Text("\(file.data.count) bytes · \(file.format.uppercased())").font(.caption).foregroundStyle(.secondary)
            }
            if let message {Text(message).font(.callout).foregroundStyle(.orange)}
            Spacer(minLength:0)
            Text(digest).font(.system(size:10,design:.monospaced)).textSelection(.enabled)
            Text("Private simulated capture. This viewer does not command motion or request new sensor data.").font(.caption).foregroundStyle(.secondary)
        }.padding(28).frame(width:820,height:620).background(Color(red:0.97,green:0.97,blue:0.95))
            .onDisappear {player?.stop()}
    }
}
