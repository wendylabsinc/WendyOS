#if os(macOS)
    import SwiftUI
    import WebKit

    struct InstallHardwareView: View {
        let guide: InstallGuide
        var boot = false
        var body: some View {
            InstallHardwareWebView(
                board: guide.board.rawValue,
                mode: guide.method.isMedia ? (guide.method.isSD ? "sd" : "nvme") : "recovery",
                boot: boot
            )
            .frame(height: 460)
            .clipShape(RoundedRectangle(cornerRadius: 14))
            .accessibilityLabel("Interactive 3D setup guide for \(guide.board.title)")
        }
        static func supports(_ board: InstallBoard) -> Bool {
            [.orinNano, .iq9075, .pi5].contains(board)
        }
    }

    // All JavaScript, models, textures and the Draco decoder are bundled. The
    // custom scheme keeps model fetches local, including when the Mac is offline.
    final class InstallAssetScheme: NSObject, WKURLSchemeHandler {
        let root: URL
        init(root: URL) { self.root = root.standardizedFileURL }
        func webView(_ webView: WKWebView, start task: WKURLSchemeTask) {
            guard let url = task.request.url, url.host == "bundle" else {
                task.didFailWithError(URLError(.badURL))
                return
            }
            let file = root.appendingPathComponent(String(url.path.dropFirst())).standardizedFileURL
            guard file.path.hasPrefix(root.path + "/") else {
                task.didFailWithError(URLError(.noPermissionsToReadFile))
                return
            }
            do {
                let data = try Data(contentsOf: file, options: .mappedIfSafe)
                let mime =
                    [
                        "html": "text/html", "js": "text/javascript", "wasm": "application/wasm",
                        "glb": "model/gltf-binary", "png": "image/png", "jpg": "image/jpeg",
                    ][file.pathExtension]
                    ?? "application/octet-stream"
                task.didReceive(
                    URLResponse(
                        url: url,
                        mimeType: mime,
                        expectedContentLength: data.count,
                        textEncodingName: nil
                    )
                )
                task.didReceive(data)
                task.didFinish()
            } catch { task.didFailWithError(error) }
        }
        func webView(_ webView: WKWebView, stop task: WKURLSchemeTask) {}
    }

    struct InstallHardwareWebView: NSViewRepresentable {
        let board: String
        let mode: String
        let boot: Bool
        func makeCoordinator() -> Coordinator { Coordinator() }
        func makeNSView(context: Context) -> WKWebView {
            let config = WKWebViewConfiguration()
            config.websiteDataStore = .nonPersistent()
            if let root = Bundle.main.resourceURL?.appendingPathComponent("Install3D") {
                config.setURLSchemeHandler(
                    InstallAssetScheme(root: root),
                    forURLScheme: "wendy-install"
                )
            }
            let view = WKWebView(frame: .zero, configuration: config)
            view.navigationDelegate = context.coordinator
            view.setValue(false, forKey: "drawsBackground")
            return view
        }
        func updateNSView(_ view: WKWebView, context: Context) {
            var url = URLComponents(string: "wendy-install://bundle/index.html")!
            url.queryItems = [
                URLQueryItem(name: "board", value: board), URLQueryItem(name: "mode", value: mode),
                URLQueryItem(name: "boot", value: String(boot)),
            ]
            guard context.coordinator.loaded != url.url else { return }
            context.coordinator.loaded = url.url
            view.load(URLRequest(url: url.url!))
        }
        static func dismantleNSView(_ view: WKWebView, coordinator: Coordinator) {
            view.stopLoading()
            view.loadHTMLString("", baseURL: nil)
            view.navigationDelegate = nil
        }
        final class Coordinator: NSObject, WKNavigationDelegate {
            var loaded: URL?
            func webView(
                _ webView: WKWebView,
                decidePolicyFor navigationAction: WKNavigationAction
            ) async -> WKNavigationActionPolicy {
                let url = navigationAction.request.url
                return url?.scheme == "wendy-install" && url?.host == "bundle" ? .allow : .cancel
            }
        }
    }
#endif
