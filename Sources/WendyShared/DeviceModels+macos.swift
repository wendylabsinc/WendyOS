#if os(macOS)
    import Foundation
    import Logging
    import IOKit
    import IOKit.usb
    import Network
    import SystemConfiguration

    // macOS specific extension for USBDevice
    extension USBDevice {
        public static func fromIORegistryEntry(
            _ device: io_service_t,
            provider: IOServiceProvider? = nil
        ) -> USBDevice? {
            let ioProvider = provider ?? DefaultIOServiceProvider()

            // Get required device properties using the provided IOServiceProvider
            guard
                let deviceName = ioProvider.getRegistryEntryProperty(
                    device: device,
                    key: "USB Product Name" as CFString
                ) as? String,
                let vendorId = ioProvider.getRegistryEntryProperty(
                    device: device,
                    key: "idVendor" as CFString
                ) as? Int,
                let productId = ioProvider.getRegistryEntryProperty(
                    device: device,
                    key: "idProduct" as CFString
                ) as? Int
            else {
                return nil
            }

            // Get optional USB version from bcdUSB (BCD-encoded, e.g., 0x0200 = USB 2.0)
            let usbVersion: String?
            if let bcdUSB = ioProvider.getRegistryEntryProperty(
                device: device,
                key: "bcdUSB" as CFString
            ) as? Int {
                usbVersion = Self.formatUSBVersion(bcdUSB)
            } else {
                usbVersion = nil
            }

            // Get optional serial number
            let serialNumber =
                ioProvider.getRegistryEntryProperty(
                    device: device,
                    key: "kUSBSerialNumberString" as CFString
                ) as? String ?? ioProvider.getRegistryEntryProperty(
                    device: device,
                    key: "USB Serial Number" as CFString
                ) as? String

            // Get optional max power (in 2mA units for USB 2.0, 8mA for USB 3.0)
            let maxPowerMilliamps: Int?
            if let maxPower = ioProvider.getRegistryEntryProperty(
                device: device,
                key: "bMaxPower" as CFString
            ) as? Int {
                // bMaxPower is in 2mA units for USB 2.0
                maxPowerMilliamps = maxPower * 2
            } else {
                maxPowerMilliamps = nil
            }

            return USBDevice(
                name: deviceName,
                vendorId: vendorId,
                productId: productId,
                usbVersion: usbVersion,
                serialNumber: serialNumber,
                maxPowerMilliamps: maxPowerMilliamps
            )
        }

        /// Converts BCD-encoded USB version to a human-readable string
        private static func formatUSBVersion(_ bcdUSB: Int) -> String {
            let major = (bcdUSB >> 8) & 0xFF
            let minor = (bcdUSB >> 4) & 0x0F
            let patch = bcdUSB & 0x0F

            if patch == 0 {
                return "USB \(major).\(minor)"
            } else {
                return "USB \(major).\(minor).\(patch)"
            }
        }
    }
#endif
