import AppKit

@main
struct WendyAgentApplication {
    static func main() {
        let application = NSApplication.shared
        let delegate = AppDelegate()

        application.setActivationPolicy(.accessory)
        application.delegate = delegate
        application.run()

        withExtendedLifetime(delegate) {}
    }
}
