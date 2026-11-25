import RealityKit
import SwiftUI
import UniformTypeIdentifiers

/// Reconstruction runs locally and returns an actual textured model. Hardware
/// support and reconstruction failures remain explicit; there is no generated
/// or demonstration geometry on failure.
@MainActor
public struct SitePhotoReconstruction: View {
    public static var isSupported: Bool { PhotogrammetrySession.isSupported }
    let completion: (Result<URL, Error>) -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var selecting = false
    @State private var input: URL?
    @State private var progress = 0.0
    @State private var running = false
    @State private var session: PhotogrammetrySession?

    public init(completion: @escaping (Result<URL, Error>) -> Void) { self.completion = completion }

    public var body: some View {
        NavigationStack {
            VStack(alignment: .leading, spacing: 18) {
                Text("Build a Textured Digital Twin").font(.title2.bold())
                Text(
                    "Choose a folder of overlapping photographs taken from different positions. Keep lighting consistent and cover each surface from several viewpoints. A reconstruction can take several minutes."
                )
                Text(
                    "Check the finished model for missing surfaces. Photograph-based reconstruction does not establish measurement accuracy."
                )
                .foregroundStyle(.secondary)
                if running {
                    ProgressView("Reconstructing…", value: progress)
                    Text(progress, format: .percent.precision(.fractionLength(0))).monospacedDigit()
                } else {
                    Button("Choose Photo Folder") { selecting = true }.buttonStyle(
                        .borderedProminent
                    )
                }
                Spacer()
            }.padding().frame(minWidth: 280, minHeight: 300)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) {
                        Button("Cancel") {
                            session?.cancel()
                            dismiss()
                        }
                    }
                }
        }
        .fileImporter(isPresented: $selecting, allowedContentTypes: [.folder]) { result in
            switch result {
            case .success(let url): input = url
            case .failure(let error):
                if (error as NSError).code != NSUserCancelledError { completion(.failure(error)) }
            }
        }
        .task(id: input) {
            guard let input else { return }
            running = true
            let access = input.startAccessingSecurityScopedResource()
            defer {
                if access { input.stopAccessingSecurityScopedResource() }
                running = false
                session?.cancel()
                session = nil
            }
            var outputDirectory: URL?
            do {
                let directory = FileManager.default.temporaryDirectory.appendingPathComponent(
                    UUID().uuidString,
                    isDirectory: true
                )
                outputDirectory = directory
                try FileManager.default.createDirectory(
                    at: directory,
                    withIntermediateDirectories: true
                )
                let output = directory.appendingPathComponent("Site Scan.usdz")
                var configuration = PhotogrammetrySession.Configuration()
                configuration.sampleOrdering = .unordered
                configuration.ignoreBoundingBox = true
                let session = try PhotogrammetrySession(input: input, configuration: configuration)
                self.session = session
                try session.process(requests: [.modelFile(url: output, detail: .reduced)])
                for try await event in session.outputs {
                    try Task.checkCancellation()
                    switch event {
                    case .requestProgress(_, let fraction): progress = fraction
                    case .requestError(_, let error): throw error
                    case .requestComplete(_, .modelFile(let url)):
                        outputDirectory = nil
                        completion(.success(url))
                        return
                    case .processingCancelled: throw CancellationError()
                    default: break
                    }
                }
                throw ReconstructionError.noModel
            } catch is CancellationError {
                if let outputDirectory { try? FileManager.default.removeItem(at: outputDirectory) }
            } catch {
                if let outputDirectory { try? FileManager.default.removeItem(at: outputDirectory) }
                completion(.failure(error))
            }
        }
        .onDisappear { session?.cancel() }
    }
}

private enum ReconstructionError: Error { case noModel }
