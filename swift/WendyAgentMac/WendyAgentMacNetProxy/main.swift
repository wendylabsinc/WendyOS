import Foundation
import NetworkExtension

// System-extension entry point. Registers the provider classes declared in
// Info.plist's NEProviderClasses and runs the extension's run loop.
autoreleasepool {
    NEProvider.startSystemExtensionMode()
}

dispatchMain()
