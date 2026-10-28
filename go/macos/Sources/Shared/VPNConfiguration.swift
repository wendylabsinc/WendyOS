import Foundation

struct VPNRoute: Codable, Identifiable {
    var selector: String
    var name: String
    var address: String
    var host: String
    var id: String { selector }
}
struct VPNConfiguration: Codable {
    let endpoint: String
    let token: String
    let routes: [VPNRoute]

    // Borrowed C pointers remain valid until the synchronous call returns.
    func withCValue<T>(_ body: (UnsafeMutablePointer<WDVPNConfig>) throws -> T) rethrows -> T {
        let strings = CStringStorage()
            defer { withExtendedLifetime(strings) {} }
        var cRoutes = routes.map {
            WDRoute(selector: strings.add($0.selector), name: strings.add($0.name),
                    address: strings.add($0.address), host: strings.add($0.host))
        }
        return try cRoutes.withUnsafeMutableBufferPointer { buffer in
            var config = WDVPNConfig(endpoint: strings.add(endpoint), token: strings.add(token),
                                     routes: buffer.baseAddress, route_count: buffer.count)
            return try body(&config)
        }
    }
}
final class CStringStorage {
    private var pointers: [UnsafeMutablePointer<CChar>] = []
    func add(_ value: String) -> UnsafeMutablePointer<CChar> {
        let pointer = strdup(value)!
        pointers.append(pointer)
        return pointer
    }
    deinit { pointers.forEach { free($0) } }
}
