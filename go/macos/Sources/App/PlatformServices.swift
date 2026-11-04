import SwiftUI
#if os(macOS)
import AppKit
#else
import UIKit
#endif

enum PlatformClipboard {
    static func copy(_ text: String) {
        #if os(macOS)
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(text, forType: .string)
        #else
        UIPasteboard.general.string = text
        #endif
    }
}
enum PlatformColors {
    #if os(macOS)
    static let control = Color(nsColor: .controlBackgroundColor)
    static let text = Color(nsColor: .textBackgroundColor)
    static let separator = Color(nsColor: .separatorColor)
    #else
    static let control = Color(uiColor: .secondarySystemGroupedBackground)
    static let text = Color(uiColor: .systemBackground)
    static let separator = Color(uiColor: .separator)
    #endif
}
extension View {
    @ViewBuilder func desktopFrame(width: CGFloat, height: CGFloat? = nil) -> some View {
        #if os(macOS)
        frame(width: width, height: height)
        #else
        frame(maxWidth: .infinity, maxHeight: height == nil ? nil : .infinity)
        #endif
    }
}

struct AdaptiveStack<Content: View>: View {
    #if os(iOS)
    @Environment(\.horizontalSizeClass) private var sizeClass
    #endif
    var spacing: CGFloat = 12
    @ViewBuilder let content: Content
    var body: some View {
        #if os(iOS)
        if sizeClass == .compact { VStack(alignment: .leading, spacing: spacing) { content } }
        else { HStack(spacing: spacing) { content } }
        #else
        HStack(spacing: spacing) { content }
        #endif
    }
}
extension View {
    @ViewBuilder func desktopMinimumWidth(_ width: CGFloat) -> some View {
        #if os(macOS)
        frame(minWidth: width, maxWidth: .infinity)
        #else
        frame(maxWidth: .infinity)
        #endif
    }
    @ViewBuilder func desktopWidth(min: CGFloat, ideal: CGFloat, max: CGFloat) -> some View {
        #if os(macOS)
        frame(minWidth: min, idealWidth: ideal, maxWidth: max)
        #else
        frame(maxWidth: .infinity)
        #endif
    }
    @ViewBuilder func desktopActionWidth(_ width: CGFloat) -> some View {
        #if os(macOS)
        frame(width: width, alignment: .trailing)
        #else
        frame(maxWidth: .infinity, alignment: .trailing)
        #endif
    }
}
extension View {
    @ViewBuilder func mobileListHeight(_ height: CGFloat) -> some View {
        #if os(iOS)
        frame(height: height)
        #else
        self
        #endif
    }
}

// Keep refresh gestures on iOS without changing desktop scrolling.
extension View {
    @ViewBuilder func mobileRefreshable(action: @escaping () async -> Void) -> some View {
        #if os(iOS)
        refreshable { await action() }.scrollBounceBehavior(.always, axes: .vertical)
        #else
        self
        #endif
    }
    @ViewBuilder func mobileRefreshablePlaceholder(action: @escaping () async -> Void) -> some View {
        #if os(iOS)
        GeometryReader { geometry in
            ScrollView {
                self.frame(minHeight: geometry.size.height)
            }.mobileRefreshable(action: action)
        }
        #else
        self
        #endif
    }
}
