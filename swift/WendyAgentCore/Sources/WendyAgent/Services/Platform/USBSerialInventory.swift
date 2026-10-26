import Foundation
import IOKit
import WendyAgentGRPC

/// Enumerates USB-backed serial endpoints without opening the device node.
/// IORegistry parent properties join each callout node to its USB identity;
/// built-in consoles and Bluetooth serial ports have no VID/PID and are omitted.
struct USBSerialInventory: Sendable {
    typealias Device = Wendy_Agent_Services_V2_RobotArm

    func discover() throws -> [Device] {
        var iterator: io_iterator_t = 0
        let result = unsafe IOServiceGetMatchingServices(
            kIOMainPortDefault,
            IOServiceMatching("IOSerialBSDClient"),
            &iterator
        )
        guard result == KERN_SUCCESS else {
            throw CocoaError(.fileReadUnknown)
        }
        defer { IOObjectRelease(iterator) }
        var devices: [Device] = []
        while true {
            let service = IOIteratorNext(iterator)
            if service == 0 { break }
            defer { IOObjectRelease(service) }
            // String descriptors are read from the adapter's own USB device. A
            // parent search would continue past an adapter without a serial
            // number or vendor name and return a hub's instead.
            let usbDevice = Self.usbDevice(above: service)
            defer { if usbDevice != 0 { IOObjectRelease(usbDevice) } }
            var metadata: [String: String] = [:]
            for key in ["idVendor", "idProduct", "locationID", "bInterfaceNumber"] {
                let value = Self.property(key, of: service, searchingParents: true)
                if let value = value as? NSNumber { metadata[key] = value.stringValue }
            }
            if let path = Self.property("IOCalloutDevice", of: service) as? String {
                metadata["IOCalloutDevice"] = path
            }
            if usbDevice != 0 {
                for key in ["USB Serial Number", "USB Vendor Name", "USB Product Name"] {
                    if let value = Self.property(key, of: usbDevice) as? String {
                        metadata[key] = value
                    }
                }
            }
            if let device = Self.device(metadata),
                FileManager.default.fileExists(atPath: device.devicePath)
            {
                devices.append(device)
            }
        }
        return devices.sorted { $0.devicePath < $1.devicePath }
    }

    private static func property(
        _ key: String,
        of entry: io_registry_entry_t,
        searchingParents: Bool = false
    ) -> Any? {
        let parents = IOOptionBits(kIORegistryIterateRecursively | kIORegistryIterateParents)
        return unsafe IORegistryEntrySearchCFProperty(
            entry,
            kIOServicePlane,
            key as CFString,
            kCFAllocatorDefault,
            searchingParents ? parents : 0
        )
    }

    /// The nearest USB device above a serial node, which the caller releases;
    /// 0 when the node isn't USB-backed.
    private static func usbDevice(above service: io_registry_entry_t) -> io_registry_entry_t {
        var parents: io_iterator_t = 0
        let result = unsafe IORegistryEntryCreateIterator(
            service,
            kIOServicePlane,
            IOOptionBits(kIORegistryIterateRecursively | kIORegistryIterateParents),
            &parents
        )
        guard result == KERN_SUCCESS else { return 0 }
        defer { IOObjectRelease(parents) }
        while true {
            let entry = IOIteratorNext(parents)
            if entry == 0 { return 0 }
            if unsafe IOObjectConformsTo(entry, "IOUSBHostDevice") != 0 { return entry }
            IOObjectRelease(entry)
        }
    }

    /// Decimal numeric IORegistry properties; fixture-testable independently of IOKit.
    static func device(_ metadata: [String: String]) -> Device? {
        guard let vid = UInt16(metadata["idVendor"] ?? ""),
            let pid = UInt16(metadata["idProduct"] ?? ""),
            let path = metadata["IOCalloutDevice"], path.hasPrefix("/dev/cu."),
            let location = UInt32(metadata["locationID"] ?? "")
        else { return nil }
        var d = Device()
        d.vendorID = Self.hex(UInt32(vid), width: 4)
        d.productID = Self.hex(UInt32(pid), width: 4)
        d.devicePath = path
        d.usbLocation = Self.hex(location, width: 8)
        d.usbInterface = metadata["bInterfaceNumber"] ?? ""
        d.serialNumber =
            metadata["USB Serial Number"]?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        d.manufacturer = metadata["USB Vendor Name"] ?? ""
        d.product = metadata["USB Product Name"] ?? ""
        d.identityKind = d.serialNumber.isEmpty ? "usb_location" : "usb_serial"
        let key = d.serialNumber.isEmpty ? d.usbLocation : d.serialNumber
        let hex = key.utf8.map { Self.hex(UInt32($0), width: 2) }.joined()
        d.id =
            "usb:\(d.vendorID):\(d.productID):\(d.serialNumber.isEmpty ? "l" : "s"):\(hex):\(d.usbInterface)"
        d.candidate = d.vendorID == "1a86" && ["55d3", "55d4", "7523"].contains(d.productID)
        d.connected = true
        d.connectionStatus = "present"
        d.accessStatus = "unknown"
        d.note =
            "USB serial device; arm model, motor power, port access and busy state are unverified."
        if d.candidate {
            d.note =
                "SO-101 adapter candidate (shared USB adapter IDs); attached arm is unverified."
        }
        if d.identityKind == "usb_location" {
            d.note +=
                " No serial ID: binding follows this USB port; replacement devices cannot be distinguished."
        }
        return d
    }
    private static func hex(_ value: UInt32, width: Int) -> String {
        let digits = String(value, radix: 16)
        return String(repeating: "0", count: max(0, width - digits.count)) + digits
    }

}
