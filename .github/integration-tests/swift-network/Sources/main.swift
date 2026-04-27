// Verify network connectivity by resolving DNS and opening a TCP connection.
#if canImport(Glibc)
import Glibc
#elseif canImport(Darwin)
import Darwin
#endif

var hints = addrinfo()
hints.ai_family = AF_INET
#if canImport(Glibc)
hints.ai_socktype = Int32(SOCK_STREAM.rawValue)
#else
hints.ai_socktype = SOCK_STREAM
#endif

var res: UnsafeMutablePointer<addrinfo>?
let host = "captive.apple.com"

guard getaddrinfo(host, "80", &hints, &res) == 0, let addrInfo = res else {
    print("FAIL: DNS resolution failed for \(host)")
    exit(1)
}
defer { freeaddrinfo(res) }

let fd = socket(addrInfo.pointee.ai_family, addrInfo.pointee.ai_socktype, addrInfo.pointee.ai_protocol)
guard fd >= 0 else {
    print("FAIL: socket() failed")
    exit(1)
}
defer { close(fd) }

guard connect(fd, addrInfo.pointee.ai_addr, addrInfo.pointee.ai_addrlen) == 0 else {
    print("FAIL: connect() to \(host):80 failed")
    exit(1)
}

print("PASS: Network connectivity verified (TCP \(host):80)")
