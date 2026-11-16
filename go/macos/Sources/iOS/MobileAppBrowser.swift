import SwiftUI
import WebKit

struct MobileAppWebpage: Identifiable {
    let url: URL
    let app: String
    let device: String
    var id: URL { url }
}

struct MobileAppBrowser: View {
    let target: MobileAppWebpage
    @Environment(\.dismiss) private var dismiss
    @State private var page = WebPage()
    @State private var error: String?
    @State private var attempt = 0
    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 12) {
                Button("Done") { dismiss() }.fontWeight(.semibold).accessibilityIdentifier("app.browser.done")
                Text((page.url ?? target.url).absoluteString)
                    .font(.caption).lineLimit(1).truncationMode(.middle)
                    .frame(maxWidth: .infinity).padding(8).background(.quaternary, in: .capsule)
                    .accessibilityLabel("Web address").accessibilityValue((page.url ?? target.url).absoluteString)
                    .accessibilityIdentifier("app.browser.address")
                Link(destination: page.url ?? target.url) { Image(systemName: "globe") }.accessibilityLabel("Open in Safari")
            }.padding(.horizontal, 16).padding(.vertical, 10).background(.bar)
            VStack(alignment: .leading, spacing: 4) {
                Text("Dashboard").font(.title2.bold())
                Text(target.app + " · " + target.device).font(.subheadline).foregroundStyle(.secondary)
            }.frame(maxWidth: .infinity, alignment: .leading).padding(.horizontal, 18).padding(.vertical, 12)
            if let error {
                ContentUnavailableView {
                    Label("Couldn’t load web page", systemImage: "globe")
                } description: { Text(error) } actions: { Button("Try Again") { attempt += 1 } }
            } else {
                WebView(page).overlay(alignment: .top) {
                    if page.isLoading { ProgressView(value: page.estimatedProgress).progressViewStyle(.linear) }
                }
            }
        }
            .task(id: attempt) {
                error = nil
                do { for try await _ in page.load(target.url) { try Task.checkCancellation() } }
                catch { if !Task.isCancelled { self.error = error.localizedDescription } }
            }
            .onDisappear { page.stopLoading() }
    }
}
