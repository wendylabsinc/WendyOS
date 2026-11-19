import Testing

@testable import LinuxVideo

@Suite struct NodeOrderTests {
    @Test func videoNodesSortByNumber() {
        #expect(
            videoNodesInNumberOrder(["video254", "video0", "video10", "video2"])
                == ["video0", "video2", "video10", "video254"]
        )
    }
}
