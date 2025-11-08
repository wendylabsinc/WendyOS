import Testing

@testable import Imager

@Test
func linuxDDStatusProgress() {
    let line = "123456789 bytes (123 MB, 118 MiB) copied, 10 s, 12 MB/s"
    #expect(parseBytesTransferred(from: line) == 123_456_789)
}

@Test
func macOSDDSiginfoProgress() {
    let line = "657297408 bytes transferred in 13.928358 secs (47185 bytes/sec)"
    #expect(parseBytesTransferred(from: line) == 657_297_408)
}

@Test
func ignoresNonProgressLines() {
    #expect(parseBytesTransferred(from: "1280+0 records in") == nil)
    #expect(parseBytesTransferred(from: "1280+0 records out") == nil)
}
