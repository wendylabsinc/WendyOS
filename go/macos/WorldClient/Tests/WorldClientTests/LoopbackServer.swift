import Darwin
import Foundation

/// A real loopback HTTP endpoint for transport failure tests. Go authorization
/// interoperability is checked separately against the shared Go fixtures.
final class LoopbackServer: @unchecked Sendable {
    struct Request: Sendable {
        let path: String
        let headers: [String: String]
        let body: Data
    }
    struct Response: Sendable {
        var status = 200
        var headers: [String: String] = [:]
        var body = Data()
        var includeLength = true
    }
    let url: URL
    let socketFD: Int32 // Read-only lifecycle diagnostics for fixture regression tests.
    private let lock = NSLock()
    private var stopped = false
    private var captured: [Request] = []
    var requests: [Request] { lock.lock(); defer { lock.unlock() }; return captured }

    init(handler: @escaping @Sendable (Request) -> Response) throws {
        let fd = socket(AF_INET, SOCK_STREAM, 0)
        guard fd >= 0 else { throw URLError(.cannotCreateFile) }
        var address = sockaddr_in()
        address.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
        address.sin_family = sa_family_t(AF_INET)
        address.sin_addr.s_addr = inet_addr("127.0.0.1")
        let bound = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) }
        }
        guard bound == 0, listen(fd, 8) == 0 else { close(fd); throw URLError(.cannotConnectToHost) }
        var length = socklen_t(MemoryLayout<sockaddr_in>.size)
        let named = withUnsafeMutablePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(fd, $0, &length) }
        }
        guard named == 0 else { close(fd); throw URLError(.cannotConnectToHost) }
        self.socketFD = fd
        self.url = URL(string: "http://127.0.0.1:\(UInt16(bigEndian: address.sin_port))")!
        DispatchQueue.global().async { [self] in
            // Only this loop closes the listener. stop() may run while a handler
            // is blocked; closing there lets another fixture reuse fd underneath
            // this loop and receive responses from the wrong handler.
            defer {
                lock.lock(); stopped = true; close(fd); lock.unlock()
            }
            while true {
                lock.lock(); let ending = stopped; lock.unlock()
                if ending { return }
                let client = accept(fd, nil, nil)
                if client < 0 { return }
                lock.lock(); let canceled = stopped; lock.unlock()
                if canceled { close(client); return }
                var timeout = timeval(tv_sec: 5, tv_usec: 0)
                setsockopt(client, SOL_SOCKET, SO_RCVTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size))
                var noSignal: Int32 = 1
                setsockopt(client, SOL_SOCKET, SO_NOSIGPIPE, &noSignal, socklen_t(MemoryLayout<Int32>.size))
                if let request = Self.read(client) {
                    lock.lock(); captured.append(request); lock.unlock()
                    let response = handler(request)
                    var headers = response.headers
                    headers["Connection"] = "close"
                    headers["Content-Type"] = "application/json"
                    if response.includeLength && headers["Content-Length"] == nil { headers["Content-Length"] = String(response.body.count) }
                    var wire = Data("HTTP/1.1 \(response.status) Test\r\n".utf8)
                    for (name, value) in headers { wire.append(Data("\(name): \(value)\r\n".utf8)) }
                    wire.append(Data("\r\n".utf8)); wire.append(response.body)
                    wire.withUnsafeBytes { raw in
                        var offset = 0
                        while offset < raw.count {
                            let count = send(client, raw.baseAddress!.advanced(by: offset), raw.count - offset, 0)
                            if count <= 0 { break }
                            offset += count
                        }
                    }
                }
                close(client)
            }
        }
    }

    func stop() {
        lock.lock(); defer { lock.unlock() }
        if !stopped { stopped = true; shutdown(socketFD, SHUT_RDWR) }
    }

    private static func read(_ fd: Int32) -> Request? {
        var bytes = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)
        while bytes.count <= 256 << 10 {
            let count = recv(fd, &buffer, buffer.count, 0)
            guard count > 0 else { return nil }
            bytes.append(contentsOf: buffer.prefix(count))
            guard let separator = bytes.range(of: Data("\r\n\r\n".utf8)), let text = String(data: bytes[..<separator.lowerBound], encoding: .utf8) else { continue }
            let lines = text.components(separatedBy: "\r\n")
            guard let path = lines.first?.split(separator: " ").dropFirst().first else { return nil }
            var headers: [String: String] = [:]
            for line in lines.dropFirst() {
                let parts = line.split(separator: ":", maxSplits: 1)
                if parts.count == 2 { headers[String(parts[0]).lowercased()] = parts[1].trimmingCharacters(in: .whitespaces) }
            }
            let length = Int(headers["content-length"] ?? "0") ?? 0
            guard length >= 0, length <= 128 << 10 else { return nil }
            if bytes.count - separator.upperBound >= length { return Request(path: String(path), headers: headers, body: Data(bytes[separator.upperBound..<(separator.upperBound + length)])) }
        }
        return nil
    }
}
