// Verify bluetooth hardware is accessible from within the container.
import Foundation

#if canImport(Glibc)
import Glibc
#elseif canImport(Darwin)
import Darwin
#endif

let fm = FileManager.default
let sysPath = "/sys/class/bluetooth"

if fm.fileExists(atPath: sysPath),
   let controllers = try? fm.contentsOfDirectory(atPath: sysPath),
   !controllers.isEmpty {
    print("Bluetooth controllers: \(controllers.joined(separator: ", "))")
    print("PASS: Bluetooth entitlement verified")
} else {
    // Fallback: try to open a raw Bluetooth HCI socket.
    let AF_BLUETOOTH: Int32 = 31 // PF_BLUETOOTH on Linux
    let BTPROTO_HCI: Int32 = 1
    #if canImport(Glibc)
    let fd = socket(AF_BLUETOOTH, Int32(SOCK_RAW.rawValue), BTPROTO_HCI)
    #else
    let fd = socket(AF_BLUETOOTH, SOCK_RAW, BTPROTO_HCI)
    #endif

    if fd >= 0 {
        close(fd)
        print("PASS: Bluetooth HCI socket opened successfully")
    } else {
        print("FAIL: Bluetooth not accessible")
        print("  /sys/class/bluetooth: not found or empty")
        print("  HCI socket: could not open (errno \(errno))")
        exit(1)
    }
}
