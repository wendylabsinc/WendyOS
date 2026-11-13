import Foundation

/// AVFoundation USB UIDs encode location, vendor and product. Never use a
/// camera name or a guessed USB address to select a control endpoint.
struct UVCIdentity: Equatable, Sendable {
    let location: UInt32
    let vendor: UInt16
    let product: UInt16

    init(uniqueID: String) throws {
        guard uniqueID.hasPrefix("0x"), uniqueID.count <= 18,
            let bits = UInt64(uniqueID.dropFirst(2), radix: 16), bits >> 32 != 0,
            (bits >> 16) & 0xffff != 0, bits & 0xffff != 0
        else { throw UVCError.unsupported("Camera has no exact USB identity") }
        location = UInt32(bits >> 32)
        vendor = UInt16((bits >> 16) & 0xffff)
        product = UInt16(bits & 0xffff)
    }
}

enum UVCError: Error, CustomStringConvertible {
    case unsupported(String)
    case malformedDescriptor
    case transfer(Int32)
    case invalidValue(String)

    var description: String {
        switch self {
        case .unsupported(let detail), .invalidValue(let detail): return detail
        case .malformedDescriptor: return "Malformed or ambiguous USB video-control descriptor"
        case .transfer(let status): return "USB control request failed (\(status))"
        }
    }
}

/// Only scalar standard UVC controls are mapped. Unknown bits and extension
/// units are not guessed at and cannot be addressed through this API.
struct UVCControlDefinition: Sendable {
    enum Kind: Sendable { case integer, boolean, exposureMode }
    let name: String
    let selector: UInt8
    let length: Int
    let signed: Bool
    let kind: Kind
    let entity: UInt8
    let interface: UInt8
}

struct UVCDescriptor {
    static func controls(_ bytes: [UInt8]) throws -> [UVCControlDefinition] {
        guard bytes.count >= 9, bytes.count <= 65535, bytes[0] == 9, bytes[1] == 2,
            Int(bytes[2]) | (Int(bytes[3]) << 8) == bytes.count
        else { throw UVCError.malformedDescriptor }
        var interface: UInt8?
        var interfaces = Set<UInt8>()
        var terminals: [(UInt8, UInt8, [UInt8])] = []
        var units: [(UInt8, UInt8, UInt8, [UInt8])] = []
        var offset = 0
        while offset < bytes.count {
            guard offset + 2 <= bytes.count else { throw UVCError.malformedDescriptor }
            let length = Int(bytes[offset])
            guard length >= 2, offset + length <= bytes.count else {
                throw UVCError.malformedDescriptor
            }
            let d = Array(bytes[offset..<offset + length])
            if d[1] == 4 {
                guard length >= 9 else { throw UVCError.malformedDescriptor }
                interface = d[5] == 14 && d[6] == 1 && d[3] == 0 ? d[2] : nil
                if let interface { interfaces.insert(interface) }
            } else if d[1] == 0x24, let interface {
                guard length >= 3 else { throw UVCError.malformedDescriptor }
                if d[2] == 2 {
                    guard length >= 8 else { throw UVCError.malformedDescriptor }
                    if d[4] == 1 && d[5] == 2 {
                        guard length >= 15, 15 + Int(d[14]) <= length, d[3] != 0 else {
                            throw UVCError.malformedDescriptor
                        }
                        terminals.append((interface, d[3], Array(d[15..<15 + Int(d[14])])))
                    }
                } else if d[2] == 5 {
                    guard length >= 8, 8 + Int(d[7]) <= length, d[3] != 0 else {
                        throw UVCError.malformedDescriptor
                    }
                    units.append((interface, d[3], d[4], Array(d[8..<8 + Int(d[7])])))
                }
            }
            offset += length
        }
        guard interfaces.count == 1, terminals.count == 1, units.count <= 1,
            let terminal = terminals.first,
            units.allSatisfy({ $0.0 == terminal.0 && $0.2 == terminal.1 && $0.1 != terminal.1 })
        else { throw UVCError.malformedDescriptor }
        typealias Spec = (String, Int, UInt8, Int, Bool, UVCControlDefinition.Kind)
        let camera: [Spec] = [
            ("auto_exposure", 1, 2, 1, false, .exposureMode),
            ("exposure_time_absolute", 3, 4, 4, false, .integer),
            ("exposure_dynamic_framerate", 2, 3, 1, false, .boolean),
            ("focus_absolute", 5, 6, 2, false, .integer),
            ("focus_automatic_continuous", 17, 8, 1, false, .boolean),
            ("zoom_absolute", 9, 11, 2, false, .integer),
        ]
        let processing: [Spec] = [
            ("brightness", 0, 2, 2, true, .integer),
            ("contrast", 1, 3, 2, false, .integer),
            ("hue", 2, 6, 2, true, .integer),
            ("saturation", 3, 7, 2, false, .integer),
            ("sharpness", 4, 8, 2, false, .integer),
            ("gamma", 5, 9, 2, false, .integer),
            ("white_balance_temperature", 6, 10, 2, false, .integer),
            ("backlight_compensation", 8, 1, 2, false, .integer),
            ("gain", 9, 4, 2, false, .integer),
            ("power_line_frequency", 10, 5, 1, false, .integer),
            ("hue_automatic", 11, 16, 1, false, .boolean),
            ("white_balance_automatic", 12, 11, 1, false, .boolean),
        ]
        func definitions(
            _ specs: [Spec],
            interface: UInt8,
            entity: UInt8,
            bitmap: [UInt8]
        ) -> [UVCControlDefinition] {
            specs.compactMap { name, bit, selector, length, signed, kind in
                guard bit / 8 < bitmap.count, bitmap[bit / 8] & (1 << (bit % 8)) != 0 else {
                    return nil
                }
                return UVCControlDefinition(
                    name: name,
                    selector: selector,
                    length: length,
                    signed: signed,
                    kind: kind,
                    entity: entity,
                    interface: interface
                )
            }
        }
        var result = definitions(
            camera,
            interface: terminal.0,
            entity: terminal.1,
            bitmap: terminal.2
        )
        if let unit = units.first {
            result += definitions(processing, interface: unit.0, entity: unit.1, bitmap: unit.3)
        }
        return result.sorted { $0.name < $1.name }
    }
}

protocol UVCControlTransport {
    func configuration() throws -> [UInt8]
    func request(
        _ definition: UVCControlDefinition,
        request: UInt8,
        bytes: [UInt8]
    ) throws -> [UInt8]
}

struct UVCCameraControl: Sendable {
    let name: String
    let value: Int32
    let minimum: Int32
    let maximum: Int32
    let step: Int32
    let defaultValue: Int32
    let mutable: Bool
    let supportedModes: UInt8?
}

struct UVCControlSession {
    let transport: any UVCControlTransport
    let definitions: [UVCControlDefinition]

    init(transport: any UVCControlTransport) throws {
        self.transport = transport
        definitions = try UVCDescriptor.controls(transport.configuration())
    }

    func read(_ d: UVCControlDefinition) throws -> UVCCameraControl {
        let infoBytes = try transport.request(d, request: 0x86, bytes: [0])
        guard infoBytes.count == 1 else { throw UVCError.transfer(-1) }
        let info = infoBytes[0]
        guard info & 1 != 0 else { throw UVCError.unsupported("\(d.name) is not readable") }
        func scalar(_ request: UInt8) throws -> Int32 {
            let bytes = try transport.request(
                d,
                request: request,
                bytes: Array(repeating: 0, count: d.length)
            )
            guard bytes.count == d.length else { throw UVCError.transfer(-1) }
            let value = bytes.enumerated().reduce(UInt32(0)) {
                $0 | (UInt32($1.element) << (8 * $1.offset))
            }
            if d.signed {
                let shift = 32 - d.length * 8
                return Int32(bitPattern: value << shift) >> shift
            }
            guard value <= Int32.max else {
                throw UVCError.invalidValue("\(d.name) exceeds the API integer range")
            }
            return Int32(value)
        }
        let current = try scalar(0x81)
        let defaultValue = try scalar(0x87)
        if case .exposureMode = d.kind {
            let modes = try scalar(0x84)
            guard modes > 0, modes <= 15 else {
                throw UVCError.invalidValue("Invalid exposure-mode bitmap")
            }
            let value = try Self.v4l2Exposure(current)
            let defaultMode = try Self.v4l2Exposure(defaultValue)
            let allowed = (0...3).filter { Int32(modes) & Self.uvcExposure(Int32($0)) != 0 }
            guard let minimum = allowed.first, let maximum = allowed.last,
                Int32(modes) & current != 0, Int32(modes) & defaultValue != 0
            else { throw UVCError.invalidValue("Unsupported reported exposure mode") }
            return UVCCameraControl(
                name: d.name,
                value: value,
                minimum: Int32(minimum),
                maximum: Int32(maximum),
                step: 1,
                defaultValue: defaultMode,
                mutable: info & 6 == 2,
                supportedModes: UInt8(modes)
            )
        }
        let minimum: Int32
        let maximum: Int32
        let step: Int32
        if case .boolean = d.kind {
            minimum = 0
            maximum = 1
            step = 1  // the standard UVC boolean domain
        } else {
            minimum = try scalar(0x82)
            maximum = try scalar(0x83)
            step = try scalar(0x84)
        }
        guard minimum <= maximum, step > 0, (minimum...maximum).contains(current),
            (minimum...maximum).contains(defaultValue)
        else { throw UVCError.invalidValue("Invalid camera range for \(d.name)") }
        return UVCCameraControl(
            name: d.name,
            value: current,
            minimum: minimum,
            maximum: maximum,
            step: step,
            defaultValue: defaultValue,
            mutable: info & 6 == 2,
            supportedModes: nil
        )
    }

    func write(name: String, value: Int32) throws {
        guard let d = definitions.first(where: { $0.name == name }) else {
            throw UVCError.unsupported("Unknown control: \(name)")
        }
        let control = try read(d)  // refresh inactive flags after preceding auto-mode changes
        guard control.mutable else {
            throw UVCError.invalidValue("\(name) is read-only or inactive")
        }
        guard value >= control.minimum, value <= control.maximum,
            (Int64(value) - Int64(control.minimum)) % Int64(control.step) == 0
        else { throw UVCError.invalidValue("\(name) is outside the camera range or step") }
        let raw: Int32
        if let modes = control.supportedModes {
            raw = Self.uvcExposure(value)
            guard raw != 0, Int32(modes) & raw != 0 else {
                throw UVCError.invalidValue("Exposure mode is not supported by this camera")
            }
        } else {
            raw = value
        }
        let bits = UInt32(bitPattern: raw)
        let bytes = (0..<d.length).map { UInt8(truncatingIfNeeded: bits >> (8 * $0)) }
        _ = try transport.request(d, request: 1, bytes: bytes)
        guard try read(d).value == value else {
            throw UVCError.invalidValue("\(name) readback did not match")
        }
    }

    static func uvcExposure(_ value: Int32) -> Int32 {
        switch value {
        case 0: return 2
        case 1: return 1
        case 2: return 4
        case 3: return 8
        default: return 0
        }
    }
    static func v4l2Exposure(_ value: Int32) throws -> Int32 {
        switch value {
        case 2: return 0
        case 1: return 1
        case 4: return 2
        case 8: return 3
        default: throw UVCError.invalidValue("Invalid UVC exposure mode")
        }
    }
    static func ordered(_ values: [(String, Int32)]) -> [(String, Int32)] {
        let auto = Set([
            "auto_exposure", "white_balance_automatic", "hue_automatic",
            "focus_automatic_continuous",
        ])
        return values.enumerated().sorted {
            let left = auto.contains($0.element.0)
            let right = auto.contains($1.element.0)
            return left == right ? $0.offset < $1.offset : left
        }.map(\.element)
    }
}
