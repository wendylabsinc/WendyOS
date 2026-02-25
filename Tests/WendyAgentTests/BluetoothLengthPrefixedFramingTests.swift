import NIOCore
import Testing

@Suite("Bluetooth Length-Prefixed Framing Tests")
struct BluetoothLengthPrefixedFramingTests {

    @Test("readLengthPrefixedSlice returns nil when buffer is empty")
    func readFromEmptyBuffer() {
        var buffer = ByteBuffer()
        let result = buffer.readLengthPrefixedSlice(endianness: .big, as: UInt16.self)
        #expect(result == nil)
    }

    @Test("readLengthPrefixedSlice returns nil when only length prefix present")
    func readWithOnlyLengthPrefix() {
        var buffer = ByteBuffer()
        buffer.writeInteger(UInt16(10), endianness: .big)  // Length prefix for 10 bytes
        let result = buffer.readLengthPrefixedSlice(endianness: .big, as: UInt16.self)
        #expect(result == nil)
        // Buffer should be unchanged (reader index reset)
        #expect(buffer.readableBytes == 2)
    }

    @Test("readLengthPrefixedSlice returns nil when partial data present")
    func readWithPartialData() {
        var buffer = ByteBuffer()
        buffer.writeInteger(UInt16(10), endianness: .big)  // Length prefix for 10 bytes
        buffer.writeBytes([1, 2, 3, 4, 5])  // Only 5 bytes of data
        let result = buffer.readLengthPrefixedSlice(endianness: .big, as: UInt16.self)
        #expect(result == nil)
        // Buffer should be unchanged (reader index reset)
        #expect(buffer.readableBytes == 7)  // 2 + 5
    }

    @Test("readLengthPrefixedSlice returns complete message")
    func readCompleteMessage() {
        var buffer = ByteBuffer()
        let payload: [UInt8] = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]
        buffer.writeInteger(UInt16(payload.count), endianness: .big)
        buffer.writeBytes(payload)

        let result = buffer.readLengthPrefixedSlice(endianness: .big, as: UInt16.self)
        #expect(result != nil)
        #expect(result?.readableBytes == 10)
        #expect(Array(result!.readableBytesView) == payload)
        #expect(buffer.readableBytes == 0)
    }

    @Test("readLengthPrefixedSlice handles multiple messages in buffer")
    func readMultipleMessages() {
        var buffer = ByteBuffer()

        // First message: [1, 2, 3]
        buffer.writeInteger(UInt16(3), endianness: .big)
        buffer.writeBytes([1, 2, 3])

        // Second message: [4, 5, 6, 7]
        buffer.writeInteger(UInt16(4), endianness: .big)
        buffer.writeBytes([4, 5, 6, 7])

        // Third message: [8]
        buffer.writeInteger(UInt16(1), endianness: .big)
        buffer.writeBytes([8])

        // Read first message
        let first = buffer.readLengthPrefixedSlice(endianness: .big, as: UInt16.self)
        #expect(first != nil)
        #expect(Array(first!.readableBytesView) == [1, 2, 3])

        // Read second message
        let second = buffer.readLengthPrefixedSlice(endianness: .big, as: UInt16.self)
        #expect(second != nil)
        #expect(Array(second!.readableBytesView) == [4, 5, 6, 7])

        // Read third message
        let third = buffer.readLengthPrefixedSlice(endianness: .big, as: UInt16.self)
        #expect(third != nil)
        #expect(Array(third!.readableBytesView) == [8])

        // No more messages
        let fourth = buffer.readLengthPrefixedSlice(endianness: .big, as: UInt16.self)
        #expect(fourth == nil)
        #expect(buffer.readableBytes == 0)
    }

    @Test("readLengthPrefixedSlice handles zero-length message")
    func readZeroLengthMessage() {
        var buffer = ByteBuffer()
        buffer.writeInteger(UInt16(0), endianness: .big)

        let result = buffer.readLengthPrefixedSlice(endianness: .big, as: UInt16.self)
        #expect(result != nil)
        #expect(result?.readableBytes == 0)
        #expect(buffer.readableBytes == 0)
    }

    @Test("writeLengthPrefixed writes correct format")
    func writeLengthPrefixed() throws {
        var buffer = ByteBuffer()
        let payload: [UInt8] = [1, 2, 3, 4, 5]

        try buffer.writeLengthPrefixed(endianness: .big, as: UInt16.self) { innerBuffer in
            innerBuffer.writeBytes(payload)
        }

        // Should have 2 bytes length prefix + 5 bytes payload
        #expect(buffer.readableBytes == 7)

        // Read back the length prefix
        let lengthPrefix = buffer.readInteger(endianness: .big, as: UInt16.self)
        #expect(lengthPrefix == 5)

        // Read back the payload
        let readPayload = buffer.readBytes(length: 5)
        #expect(readPayload == payload)
    }

    @Test("Round-trip write and read")
    func roundTrip() throws {
        var buffer = ByteBuffer()
        let messages: [[UInt8]] = [
            [0xDE, 0xAD, 0xBE, 0xEF],
            [0x01],
            [0x12, 0x34, 0x56, 0x78, 0x9A, 0xBC, 0xDE, 0xF0],
        ]

        // Write all messages
        for message in messages {
            try buffer.writeLengthPrefixed(endianness: .big, as: UInt16.self) { innerBuffer in
                innerBuffer.writeBytes(message)
            }
        }

        // Read all messages back
        var readMessages: [[UInt8]] = []
        while let slice = buffer.readLengthPrefixedSlice(endianness: .big, as: UInt16.self) {
            readMessages.append(Array(slice.readableBytesView))
        }

        #expect(readMessages == messages)
    }

    @Test("Incremental data arrival simulation")
    func incrementalDataArrival() throws {
        // Simulate BLE data arriving in chunks
        var buffer = ByteBuffer()

        // Prepare a complete message to send in chunks
        var messageBuffer = ByteBuffer()
        try messageBuffer.writeLengthPrefixed(endianness: .big, as: UInt16.self) { innerBuffer in
            innerBuffer.writeBytes([1, 2, 3, 4, 5, 6, 7, 8])
        }
        let fullMessage = Array(messageBuffer.readableBytesView)

        // Send first chunk (partial length prefix)
        buffer.writeBytes(Array(fullMessage[0..<1]))
        #expect(buffer.readLengthPrefixedSlice(endianness: .big, as: UInt16.self) == nil)

        // Send second chunk (complete length prefix, no data)
        buffer.writeBytes(Array(fullMessage[1..<2]))
        #expect(buffer.readLengthPrefixedSlice(endianness: .big, as: UInt16.self) == nil)

        // Send third chunk (partial data)
        buffer.writeBytes(Array(fullMessage[2..<6]))
        #expect(buffer.readLengthPrefixedSlice(endianness: .big, as: UInt16.self) == nil)

        // Send final chunk (remaining data)
        buffer.writeBytes(Array(fullMessage[6...]))
        let result = buffer.readLengthPrefixedSlice(endianness: .big, as: UInt16.self)
        #expect(result != nil)
        #expect(Array(result!.readableBytesView) == [1, 2, 3, 4, 5, 6, 7, 8])
    }
}
