import Foundation

#if canImport(Darwin)
    import Darwin
#elseif canImport(Glibc)
    import Glibc
#endif

extension WendyE2ECoverage {
    package static func agentControl(
        _ path: String,
        operation: String,
        directory: String = ""
    ) throws -> Int {
        #if os(macOS) || os(Linux)
            #if os(macOS)
                let fd = socket(AF_UNIX, SOCK_STREAM, 0)
            #else
                let fd = socket(AF_UNIX, Int32(SOCK_STREAM.rawValue), 0)
            #endif
            guard fd >= 0 else {
                throw failure("Agent coverage socket: \(String(cString: strerror(errno)))")
            }
            defer { _ = close(fd) }
            var timeout = timeval(tv_sec: 30, tv_usec: 0)
            _ = setsockopt(
                fd,
                SOL_SOCKET,
                SO_RCVTIMEO,
                &timeout,
                socklen_t(MemoryLayout<timeval>.size)
            )
            _ = setsockopt(
                fd,
                SOL_SOCKET,
                SO_SNDTIMEO,
                &timeout,
                socklen_t(MemoryLayout<timeval>.size)
            )
            #if os(macOS)
                var noPipe: Int32 = 1
                _ = setsockopt(
                    fd,
                    SOL_SOCKET,
                    SO_NOSIGPIPE,
                    &noPipe,
                    socklen_t(MemoryLayout<Int32>.size)
                )
            #endif
            var address = sockaddr_un()
            address.sun_family = sa_family_t(AF_UNIX)
            let bytes = Array(path.utf8) + [0]
            guard bytes.count <= MemoryLayout.size(ofValue: address.sun_path) else {
                throw failure("Agent coverage socket path too long")
            }
            withUnsafeMutableBytes(of: &address.sun_path) { buffer in buffer.copyBytes(from: bytes)
            }
            let connected = withUnsafePointer(to: &address) { pointer in
                pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                    connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
                }
            }
            guard connected == 0 else {
                throw failure("Agent coverage connect: \(String(cString: strerror(errno)))")
            }
            var request = try JSONSerialization.data(withJSONObject: [
                "Operation": operation, "Directory": directory,
            ])
            request.append(10)
            try request.withUnsafeBytes { buffer in
                var sent = 0
                while sent < buffer.count {
                    #if os(macOS)
                        let count = send(
                            fd,
                            buffer.baseAddress!.advanced(by: sent),
                            buffer.count - sent,
                            0
                        )
                    #else
                        let count = send(
                            fd,
                            buffer.baseAddress!.advanced(by: sent),
                            buffer.count - sent,
                            Int32(MSG_NOSIGNAL)
                        )
                    #endif
                    guard count > 0 else {
                        throw failure(
                            "Agent coverage write: \(String(cString: strerror(errno)))"
                        )
                    }
                    sent += count
                }
            }
            var response = Data()
            var buffer = [UInt8](repeating: 0, count: 4096)
            while !response.contains(10) {
                let count = recv(fd, &buffer, buffer.count, 0)
                guard count > 0 else {
                    throw failure(
                        "Agent coverage response missing: \(String(cString: strerror(errno)))"
                    )
                }
                response.append(contentsOf: buffer.prefix(count))
                guard response.count < 1_048_576 else {
                    throw failure("Invalid Agent coverage response")
                }
            }
            guard let value = try JSONSerialization.jsonObject(with: response) as? [String: Any],
                let error = value["error"] as? String, let pid = value["pid"] as? Int
            else { throw failure("Invalid Agent coverage response") }
            guard error.isEmpty else {
                throw failure("Agent coverage \(operation): \(error)")
            }
            return pid
        #else
            throw failure("Managed Go coverage requires an isolated Unix host")
        #endif
    }
}
