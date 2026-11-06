import SwiftUI
import UIKit
import UniformTypeIdentifiers

struct SpatialSheet: Identifiable {
    enum Kind { case connection, importFile, exportTwin(URL) }
    let id = UUID()
    let kind: Kind
}

/// Keep cancellation in our own view hierarchy even if the Files service fails
/// to load its browser. The workspace dismisses AR before presenting this sheet.
struct SpatialDocumentSheet: View {
    let exportURL: URL?
    var preparing = false
    let completion: (URL?) -> Void

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                VStack(alignment: .leading, spacing: 6) {
                    Text(exportURL == nil ? "Import a room or recording" : "Export twin to Files").font(.title2)
                    Text(exportURL == nil ? "Choose a Wendy site, USDZ model, or JSON recording." : "Choose where to save a copy of your twin.")
                        .font(.callout).foregroundStyle(.secondary)
                }
                Spacer()
                if preparing { ProgressView("Preparing import…") }
                Button("Cancel") { completion(nil) }.accessibilityIdentifier("cancel-document-picker")
            }.padding(24)
            Divider()
            SpatialDocumentPicker(exportURL: exportURL, completion: completion)
        }.frame(width: 800, height: 650)
    }
}

struct SpatialDocumentPicker: UIViewControllerRepresentable {
    let exportURL: URL?
    let completion: (URL?) -> Void

    func makeCoordinator() -> Coordinator { Coordinator(completion: completion) }
    func makeUIViewController(context: Context) -> UIDocumentPickerViewController {
        let picker: UIDocumentPickerViewController
        if let exportURL { picker = UIDocumentPickerViewController(forExporting: [exportURL], asCopy: true) }
        else {
            picker = UIDocumentPickerViewController(forOpeningContentTypes: [.data, .usdz, .json], asCopy: true)
        }
        picker.allowsMultipleSelection = false
        picker.shouldShowFileExtensions = true
        picker.delegate = context.coordinator
        return picker
    }
    func updateUIViewController(_ picker: UIDocumentPickerViewController, context: Context) {}

    @MainActor final class Coordinator: NSObject, UIDocumentPickerDelegate {
        private let completion: (URL?) -> Void
        private var completed = false
        init(completion: @escaping (URL?) -> Void) { self.completion = completion }
        func finish(_ url: URL?) {
            guard !completed else { return }
            completed = true; completion(url)
        }
        func documentPicker(_ controller: UIDocumentPickerViewController, didPickDocumentsAt urls: [URL]) { finish(urls.first) }
        func documentPickerWasCancelled(_ controller: UIDocumentPickerViewController) { finish(nil) }
    }
}
