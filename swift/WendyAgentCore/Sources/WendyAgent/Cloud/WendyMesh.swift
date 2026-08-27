public import Foundation

/// Cloud identity loaded from the Wendy CLI configuration and passed to the
/// network extension only for the lifetime of a tunnel connection.
public struct WendyCloudCredentials: Codable, Equatable, Sendable {
    public let pemCertificate: String
    public let pemCertificateChain: String
    public let pemPrivateKey: String
    public let organizationID: Int32
    public let userID: String?

    public init(
        pemCertificate: String,
        pemCertificateChain: String,
        pemPrivateKey: String,
        organizationID: Int32,
        userID: String?
    ) {
        self.pemCertificate = pemCertificate
        self.pemCertificateChain = pemCertificateChain
        self.pemPrivateKey = pemPrivateKey
        self.organizationID = organizationID
        self.userID = userID
    }

    enum CodingKeys: String, CodingKey {
        case pemCertificate = "pem_certificate"
        case pemCertificateChain = "pem_certificate_chain"
        case pemPrivateKey = "pem_private_key"
        case organizationID = "organization_id"
        case userID = "user_id"
    }
}

public struct WendyMeshDevice: Codable, Equatable, Sendable {
    public let assetID: Int32
    public let name: String
    public let organizationID: Int32
    public let online: Bool

    public init(assetID: Int32, name: String, organizationID: Int32, online: Bool) {
        self.assetID = assetID
        self.name = name
        self.organizationID = organizationID
        self.online = online
    }

    enum CodingKeys: String, CodingKey {
        case assetID = "asset_id"
        case name
        case organizationID = "org_id"
        case online
    }
}

public struct WendyMeshDirectory: Codable, Equatable, Sendable {
    public let devices: [WendyMeshDevice]

    public init(devices: [WendyMeshDevice]) {
        self.devices = devices
    }

    public static func encode(_ directory: WendyMeshDirectory) throws -> Data {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        return try encoder.encode(directory)
    }

    public static func decode(_ data: Data) throws -> WendyMeshDirectory {
        try JSONDecoder().decode(WendyMeshDirectory.self, from: data)
    }
}

/// Deterministic mesh VIP scheme shared with WendyOS: device N maps to
/// `10.99.(N >> 8).(N & 0xff)`.
public enum WendyMeshAddressPlan {
    public static let serviceCIDR = "10.99.0.0/16"
    public static let minimumDeviceID: Int32 = 1
    public static let maximumDeviceID: Int32 = 65_534

    public static func address(for deviceID: Int32) -> (UInt8, UInt8, UInt8, UInt8)? {
        guard deviceID >= minimumDeviceID, deviceID <= maximumDeviceID else { return nil }
        return (10, 99, UInt8((deviceID >> 8) & 0xff), UInt8(deviceID & 0xff))
    }

    public static func addressString(for deviceID: Int32) -> String? {
        guard let address = address(for: deviceID) else { return nil }
        return "\(address.0).\(address.1).\(address.2).\(address.3)"
    }

    public static func deviceID(for address: String) -> Int32? {
        let parts = address.split(separator: ".", omittingEmptySubsequences: false)
        guard parts.count == 4,
            let first = UInt8(parts[0]),
            let second = UInt8(parts[1]),
            let third = UInt8(parts[2]),
            let fourth = UInt8(parts[3]),
            first == 10,
            second == 99
        else {
            return nil
        }
        let deviceID = Int32(third) << 8 | Int32(fourth)
        guard deviceID >= minimumDeviceID, deviceID <= maximumDeviceID else { return nil }
        return deviceID
    }
}

/// Minimal DNS codec for `device-<id>.mesh.wendy.internal`.
public enum WendyMeshDNS {
    private static let suffixes = [".mesh.wendy.internal", ".cloud.wendy.dev"]

    public static func deviceID(forName name: String) -> Int32? {
        let normalized = name.hasSuffix(".") ? String(name.dropLast()) : name
        guard let suffix = suffixes.first(where: { normalized.hasSuffix($0) }) else { return nil }
        let label = normalized.dropLast(suffix.count)
        guard label.hasPrefix("device-") else { return nil }
        let digits = label.dropFirst("device-".count)
        guard !digits.isEmpty, digits.count <= 5, digits.allSatisfy(\.isNumber) else { return nil }
        return Int32(digits)
    }

    public static func answer(
        _ data: Data,
        resolve: (Int32) -> (UInt8, UInt8, UInt8, UInt8)?
    ) -> Data? {
        guard let query = parseQuery(data) else { return nil }
        let normalized = query.name.hasSuffix(".") ? String(query.name.dropLast()) : query.name
        guard suffixes.contains(where: { normalized.hasSuffix($0) }) else { return nil }
        guard let deviceID = deviceID(forName: query.name), let address = resolve(deviceID) else {
            return response(to: query, answer: nil, responseCode: 3)
        }
        guard query.type == 1 else {
            return response(to: query, answer: nil, responseCode: 0)
        }
        return response(to: query, answer: address, responseCode: 0)
    }

    public static func frameForTCP(_ message: Data) -> Data {
        precondition(message.count <= Int(UInt16.max))
        let count = UInt16(message.count)
        var result = Data([UInt8(count >> 8), UInt8(count & 0xff)])
        result.append(message)
        return result
    }

    public static func extractTCPMessages(from buffer: inout Data) -> [Data] {
        var messages: [Data] = []
        var offset = 0
        while buffer.count - offset >= 2 {
            let count = Int(buffer[offset]) << 8 | Int(buffer[offset + 1])
            guard buffer.count - offset - 2 >= count else { break }
            let start = offset + 2
            messages.append(buffer.subdata(in: start..<(start + count)))
            offset = start + count
        }
        if offset > 0 { buffer.removeSubrange(0..<offset) }
        return messages
    }

    private struct Query {
        let id: UInt16
        let name: String
        let type: UInt16
    }

    private static func parseQuery(_ data: Data) -> Query? {
        let bytes = [UInt8](data)
        guard bytes.count >= 12, (UInt16(bytes[4]) << 8 | UInt16(bytes[5])) >= 1 else { return nil }
        var offset = 12
        var labels: [String] = []
        while offset < bytes.count {
            let count = Int(bytes[offset])
            offset += 1
            if count == 0 { break }
            guard count <= 63, offset + count <= bytes.count else { return nil }
            labels.append(String(decoding: bytes[offset..<(offset + count)], as: UTF8.self))
            offset += count
        }
        guard offset + 4 <= bytes.count else { return nil }
        return Query(
            id: UInt16(bytes[0]) << 8 | UInt16(bytes[1]),
            name: labels.joined(separator: "."),
            type: UInt16(bytes[offset]) << 8 | UInt16(bytes[offset + 1])
        )
    }

    private static func response(
        to query: Query,
        answer: (UInt8, UInt8, UInt8, UInt8)?,
        responseCode: UInt8
    ) -> Data {
        var result = Data([
            UInt8(query.id >> 8), UInt8(query.id & 0xff),
            0x81, 0x80 | (responseCode & 0x0f),
            0, 1, 0, answer == nil ? 0 : 1, 0, 0, 0, 0,
        ])
        appendName(query.name, to: &result)
        result.append(contentsOf: [UInt8(query.type >> 8), UInt8(query.type & 0xff), 0, 1])
        if let answer {
            result.append(contentsOf: [
                0xc0, 0x0c, 0, 1, 0, 1,
                0, 0, 0, 60,
                0, 4,
                answer.0, answer.1, answer.2, answer.3,
            ])
        }
        return result
    }

    private static func appendName(_ name: String, to data: inout Data) {
        for label in name.split(separator: ".") {
            let bytes = Array(label.utf8)
            data.append(UInt8(bytes.count))
            data.append(contentsOf: bytes)
        }
        data.append(0)
    }
}

public enum WendyICMPv4 {
    public struct EchoRequest: Equatable, Sendable {
        public let sourceAddress: String
        public let destinationAddress: String
        public let identifier: UInt16
        public let sequence: UInt16
        public let payload: Data

        public init(
            sourceAddress: String,
            destinationAddress: String,
            identifier: UInt16,
            sequence: UInt16,
            payload: Data
        ) {
            self.sourceAddress = sourceAddress
            self.destinationAddress = destinationAddress
            self.identifier = identifier
            self.sequence = sequence
            self.payload = payload
        }
    }

    public static func parseEchoRequest(_ packet: Data) -> EchoRequest? {
        let bytes = [UInt8](packet)
        guard bytes.count >= 20, bytes[0] >> 4 == 4 else { return nil }
        let headerLength = Int(bytes[0] & 0x0f) * 4
        guard headerLength >= 20,
            bytes.count >= headerLength + 8,
            bytes[9] == 1,
            bytes[headerLength] == 8,
            bytes[headerLength + 1] == 0
        else {
            return nil
        }
        return EchoRequest(
            sourceAddress: dotted(bytes[12...15]),
            destinationAddress: dotted(bytes[16...19]),
            identifier: UInt16(bytes[headerLength + 4]) << 8 | UInt16(bytes[headerLength + 5]),
            sequence: UInt16(bytes[headerLength + 6]) << 8 | UInt16(bytes[headerLength + 7]),
            payload: Data(bytes[(headerLength + 8)...])
        )
    }

    public static func makeEchoReply(to request: EchoRequest, payload: Data) -> Data {
        packet(
            from: request.destinationAddress,
            to: request.sourceAddress,
            type: 0,
            identifier: request.identifier,
            sequence: request.sequence,
            payload: payload
        )
    }

    private static func packet(
        from source: String,
        to destination: String,
        type: UInt8,
        identifier: UInt16,
        sequence: UInt16,
        payload: Data
    ) -> Data {
        var icmp: [UInt8] = [
            type, 0, 0, 0,
            UInt8(identifier >> 8), UInt8(identifier & 0xff),
            UInt8(sequence >> 8), UInt8(sequence & 0xff),
        ]
        icmp.append(contentsOf: payload)
        let icmpChecksum = checksum(icmp)
        icmp[2] = UInt8(icmpChecksum >> 8)
        icmp[3] = UInt8(icmpChecksum & 0xff)

        let totalLength = 20 + icmp.count
        var ip: [UInt8] = [
            0x45, 0,
            UInt8(totalLength >> 8), UInt8(totalLength & 0xff),
            0, 0, 0x40, 0,
            64, 1, 0, 0,
        ]
        ip.append(contentsOf: octets(source))
        ip.append(contentsOf: octets(destination))
        let ipChecksum = checksum(ip)
        ip[10] = UInt8(ipChecksum >> 8)
        ip[11] = UInt8(ipChecksum & 0xff)
        return Data(ip + icmp)
    }

    private static func checksum<C: Collection>(_ bytes: C) -> UInt16 where C.Element == UInt8 {
        var sum: UInt32 = 0
        var iterator = bytes.makeIterator()
        while let high = iterator.next() {
            sum &+= UInt32(high) << 8 | UInt32(iterator.next() ?? 0)
        }
        while sum > 0xffff { sum = (sum & 0xffff) &+ (sum >> 16) }
        return ~UInt16(sum)
    }

    private static func dotted<C: Collection>(_ bytes: C) -> String where C.Element == UInt8 {
        bytes.map(String.init).joined(separator: ".")
    }

    private static func octets(_ address: String) -> [UInt8] {
        let octets = address.split(separator: ".").compactMap { UInt8($0) }
        precondition(octets.count == 4)
        return octets
    }
}
