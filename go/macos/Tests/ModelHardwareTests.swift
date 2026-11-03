import XCTest
@testable import WendyDesktop

final class ModelHardwareTests: XCTestCase {
    private let gib: Int64 = 1_073_741_824
    private func hardware(available: Int64, gpu: Int64 = 0, shared: Bool = false, diskKnown: Bool = true) -> ModelHardware {
        ModelHardware(os: "linux", architecture: "arm64", gpu: "nvidia", jetpack: shared ? "6.2" : "", warning: "", memoryTotal: 16 * gib, memoryAvailable: available, diskFree: 24 * gib, gpuTotal: gpu, gpuAvailable: gpu, diskKnown: diskKnown, cuda: true, sharedMemory: shared)
    }
    func testComputeSelectionUsesCUDAIncludingThorWithoutSeparateVRAM() {
        for (cuda, jetpack, expected) in [(true, "7.2", UInt32(1)), (true, "6.2", UInt32(3)), (true, "5.1", UInt32(2)), (true, "", UInt32(1)), (false, "", UInt32(0))] {
            let device = ModelHardware(os: "linux", architecture: "arm64", gpu: cuda ? "nvidia" : "", jetpack: jetpack, warning: "", memoryTotal: 128 * gib, memoryAvailable: 100 * gib, diskFree: 300 * gib, gpuTotal: 0, gpuAvailable: 0, diskKnown: true, cuda: cuda, sharedMemory: !jetpack.isEmpty)
            XCTAssertEqual(device.suggestedAccelerator, expected)
        }
    }

    func testFitTracksAvailableMemoryAndUnknownGPU() {
        let file = HubModelFile(name: "model.gguf", quantization: "Q4_K_M", bytes: gib)
        XCTAssertEqual(hardware(available: 12 * gib).fit(file, accelerator: 0, install: true), .comfortable)
        XCTAssertEqual(hardware(available: 3 * gib).fit(file, accelerator: 0, install: true), .tooLarge)
        XCTAssertEqual(hardware(available: 6 * gib).fit(file, accelerator: 0, install: true), .tight)
        XCTAssertEqual(hardware(available: 12 * gib).fit(file, accelerator: 1, install: true), .unknown)
        XCTAssertEqual(hardware(available: 12 * gib, gpu: 2 * gib).fit(file, accelerator: 1, install: true), .tooLarge)
        XCTAssertEqual(hardware(available: 12 * gib, shared: true).fit(file, accelerator: 3, install: true), .comfortable)
        XCTAssertEqual(hardware(available: 12 * gib, diskKnown: false).fit(file, accelerator: 0, install: true), .unknown)
    }
}
