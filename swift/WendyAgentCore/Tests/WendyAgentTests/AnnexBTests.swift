import Foundation
import Testing

@testable import WendyAgentCore

@Test func annexBPrependsStartCodesAndParamSetsOnKeyframe() {
    let sps = Data([0x67, 0x01])
    let pps = Data([0x68, 0x02])
    // one AVCC NALU: 4-byte big-endian length + payload
    var avcc = Data([0, 0, 0, 2])
    avcc.append(Data([0x65, 0xAA]))  // 0x65 = IDR slice
    let out = annexBFromAVCC(avcc, sps: [sps], pps: [pps], isKeyframe: true)
    let sc = Data([0, 0, 0, 1])
    // keyframe output: SC+SPS, SC+PPS, SC+slice
    var expected = Data()
    expected += sc + sps
    expected += sc + pps
    expected += sc + Data([0x65, 0xAA])
    #expect(out == expected)
}

@Test func annexBNonKeyframeOmitsParamSets() {
    var avcc = Data([0, 0, 0, 2])
    avcc.append(Data([0x41, 0xBB]))  // non-IDR
    let out = annexBFromAVCC(avcc, sps: [Data([0x67])], pps: [Data([0x68])], isKeyframe: false)
    #expect(out == Data([0, 0, 0, 1]) + Data([0x41, 0xBB]))
}

@Test func annexBHandlesMultipleNALUs() {
    // two AVCC NALUs concatenated
    var avcc = Data([0, 0, 0, 1])
    avcc.append(Data([0x41]))
    avcc.append(Data([0, 0, 0, 3]))
    avcc.append(Data([0x01, 0x02, 0x03]))
    let out = annexBFromAVCC(avcc, sps: [], pps: [], isKeyframe: false)
    let sc = Data([0, 0, 0, 1])
    var expected = Data()
    expected += sc + Data([0x41])
    expected += sc + Data([0x01, 0x02, 0x03])
    #expect(out == expected)
}

@Test func annexBTruncatedLengthIsIgnored() {
    // trailing bytes shorter than a 4-byte length prefix are dropped, not crashed
    let avcc = Data([0, 0, 0, 2, 0x41, 0xBB, 0x00, 0x00])
    let out = annexBFromAVCC(avcc, sps: [], pps: [], isKeyframe: false)
    #expect(out == Data([0, 0, 0, 1]) + Data([0x41, 0xBB]))
}
