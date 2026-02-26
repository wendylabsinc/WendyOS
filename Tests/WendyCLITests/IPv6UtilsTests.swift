import Testing
import WendyShared

@Suite("IPv6 Address Formatting")
struct IPv6UtilsTests {

    @Test("Loopback address ::1")
    func testLoopback() {
        // 0000:0000:0000:0000:0000:0000:0000:0001
        #expect(formatIPv6FromHex("00000000000000000000000000000001") == "::1")
    }

    @Test("Link-local fe80::1")
    func testLinkLocal() {
        // fe80:0000:0000:0000:0000:0000:0000:0001
        #expect(formatIPv6FromHex("fe800000000000000000000000000001") == "fe80::1")
    }

    @Test("All zeros ::")
    func testAllZeros() {
        #expect(formatIPv6FromHex("00000000000000000000000000000000") == "::")
    }

    @Test("Full address with no zero groups")
    func testFullAddress() {
        // 2001:0db8:0001:0002:0003:0004:0005:0006
        #expect(formatIPv6FromHex("20010db8000100020003000400050006") == "2001:db8:1:2:3:4:5:6")
    }

    @Test("Link-local with EUI-64 suffix")
    func testLinkLocalEUI64() {
        // fe80:0000:0000:0000:0a00:27ff:fe4e:66a1
        #expect(formatIPv6FromHex("fe800000000000000a0027fffe4e66a1") == "fe80::a00:27ff:fe4e:66a1")
    }

    @Test("Compresses the longest zero run")
    func testLongestZeroRun() {
        // 2001:0db8:0000:0000:0000:0000:0000:0001 -> 2001:db8::1
        #expect(formatIPv6FromHex("20010db8000000000000000000000001") == "2001:db8::1")
    }

    @Test("Single zero group is not compressed")
    func testSingleZeroNotCompressed() {
        // 2001:0db8:0000:0001:0002:0003:0004:0005
        // Only one zero group at position 2 — should NOT use ::
        #expect(formatIPv6FromHex("20010db8000000010002000300040005") == "2001:db8:0:1:2:3:4:5")
    }

    @Test("Two equal-length zero runs: first one wins")
    func testTieBreaking() {
        // 0001:0000:0000:0002:0003:0000:0000:0004
        #expect(formatIPv6FromHex("00010000000000020003000000000004") == "1::2:3:0:0:4")
    }

    @Test("Returns nil for invalid input")
    func testInvalidInput() {
        #expect(formatIPv6FromHex("") == nil)
        #expect(formatIPv6FromHex("short") == nil)
        // Invalid hex char 'g'
        #expect(formatIPv6FromHex("0000000000000000000000000000000g") == nil)
        // 33 chars (too long)
        #expect(formatIPv6FromHex("000000000000000000000000000000001") == nil)
    }

    @Test("Uppercase hex input produces lowercase output")
    func testLowercaseOutput() {
        #expect(formatIPv6FromHex("FE800000000000000000000000000001") == "fe80::1")
    }

    @Test("Zero compression at end of address")
    func testZeroCompressionAtEnd() {
        // 2001:0db8:0001:0000:0000:0000:0000:0000 -> 2001:db8:1::
        #expect(formatIPv6FromHex("20010db8000100000000000000000000") == "2001:db8:1::")
    }

    @Test("Zero compression at start of address")
    func testZeroCompressionAtStart() {
        // 0000:0000:0000:0000:0000:0000:0001:0002 -> ::1:2
        #expect(formatIPv6FromHex("00000000000000000000000000010002") == "::1:2")
    }
}
