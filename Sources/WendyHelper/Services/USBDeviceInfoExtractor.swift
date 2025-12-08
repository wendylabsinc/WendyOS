#if os(macOS)

    import Foundation
    import IOKit
    import IOKit.usb
    import Logging
    import WendyShared

    /// Protocol for extracting USB device information from properties
    protocol USBDeviceInfoExtractorProtocol {
        func extractUSBDeviceInfo(from properties: [String: Any]) -> USBDeviceInfo?
    }

    /// Extracts USB device information from IOKit properties
    struct USBDeviceInfoExtractor: USBDeviceInfoExtractorProtocol {
        private let logger: Logger

        init(logger: Logger) {
            self.logger = logger
        }

        func extractUSBDeviceInfo(from properties: [String: Any]) -> USBDeviceInfo? {
            // Extract vendor ID
            guard let vendorIdNum = properties["idVendor"] as? NSNumber else {
                logger.debug("No vendor ID found")
                return nil
            }
            let vendorId = String(format: "%04X", vendorIdNum.uint16Value)

            // Extract product ID
            guard let productIdNum = properties["idProduct"] as? NSNumber else {
                logger.debug("No product ID found")
                return nil
            }
            let productId = String(format: "%04X", productIdNum.uint16Value)

            // Extract device name (try multiple possible keys)
            let deviceName =
                (properties["USB Product Name"] as? String)
                ?? (properties["kUSBProductString"] as? String)
                ?? (properties["Product Name"] as? String) ?? "Unknown USB Device"

            // Create USBDevice to check if it's Wendy
            let usbDevice = USBDevice(
                name: deviceName,
                vendorId: Int(vendorId, radix: 16) ?? 0,
                productId: Int(productId, radix: 16) ?? 0
            )

            return USBDeviceInfo(from: usbDevice)
        }
    }

#endif  // os(macOS)
