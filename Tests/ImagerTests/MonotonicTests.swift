import Testing

@testable import Imager

@Test
func monotonicClamp() async {
    let m = Monotonic()
    var v = await m.next(0.0)
    #expect(v == 0.0)
    v = await m.next(0.3)
    #expect(v == 0.3)
    v = await m.next(0.2)
    #expect(v == 0.3)  // does not go backwards
    v = await m.next(.infinity)
    #expect(v == 0.3)  // ignores non-finite
    v = await m.next(1.0)
    #expect(v == 1.0)
    v = await m.next(0.7)
    #expect(v == 1.0)
}
