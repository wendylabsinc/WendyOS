import CUVCControl

/// Created, used and destroyed only on CameraControlExecutor. Ordinary
/// USBDeviceOpen preserves AVFoundation clients; never seize/reset/configure.
final class IOKitUVCTransport: UVCControlTransport {
    private let device: OpaquePointer

    init(identity: UVCIdentity) throws {
        var device: OpaquePointer?
        let status = unsafe wendy_uvc_open(
            identity.location,
            identity.vendor,
            identity.product,
            &device
        )
        guard status == 0, let device else { throw UVCError.transfer(status) }
        self.device = device
    }
    deinit { unsafe wendy_uvc_close(device) }

    func configuration() throws -> [UInt8] {
        try Task.checkCancellation()
        var bytes = [UInt8](repeating: 0, count: 65535)
        var count = 0
        let status = unsafe wendy_uvc_configuration(device, &bytes, bytes.count, &count)
        guard status == 0 else { throw UVCError.transfer(status) }
        guard count >= 9, count <= bytes.count else { throw UVCError.malformedDescriptor }
        return Array(bytes.prefix(count))
    }

    func request(
        _ definition: UVCControlDefinition,
        request: UInt8,
        bytes: [UInt8]
    ) throws -> [UInt8] {
        try Task.checkCancellation()
        var bytes = bytes
        guard bytes.count == (request == 0x86 ? 1 : definition.length) else {
            throw UVCError.invalidValue("Invalid control transfer length")
        }
        let status = unsafe wendy_uvc_request(
            device,
            definition.interface,
            definition.entity,
            definition.selector,
            request,
            &bytes,
            UInt16(bytes.count)
        )
        guard status == 0 else { throw UVCError.transfer(status) }
        return bytes
    }
}
