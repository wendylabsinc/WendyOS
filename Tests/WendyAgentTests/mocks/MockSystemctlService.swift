import Foundation
@testable import wendy_agent

/// Mock implementation of SystemctlService for testing
actor MockSystemctlService: SystemctlService {
    private var stateToReturn: String = "active"
    private var shouldFailStart = false
    private var shouldFailStop = false
    private var shouldFailGetState = false

    private var startCallCount = 0
    private var stopCallCount = 0
    private var stateCallCount = 0

    private var lastServiceName: String?

    // Public setters
    func setStateToReturn(_ value: String) {
        stateToReturn = value
    }

    func setShouldFailStart(_ value: Bool) {
        shouldFailStart = value
    }

    func setShouldFailStop(_ value: Bool) {
        shouldFailStop = value
    }

    func setShouldFailGetState(_ value: Bool) {
        shouldFailGetState = value
    }

    // Public getters
    func getStartCallCount() -> Int {
        return startCallCount
    }

    func getStopCallCount() -> Int {
        return stopCallCount
    }

    func getStateCallCount() -> Int {
        return stateCallCount
    }

    func getLastServiceName() -> String? {
        return lastServiceName
    }

    func start(_ serviceName: String) async throws {
        startCallCount += 1
        lastServiceName = serviceName
        if shouldFailStart {
            throw RegistryContainerService.RegistryError.commandFailed(
                "mock start failure",
                stdout: "",
                stderr: "mock error"
            )
        }
    }

    func stop(_ serviceName: String) async throws {
        stopCallCount += 1
        lastServiceName = serviceName
        if shouldFailStop {
            throw RegistryContainerService.RegistryError.commandFailed(
                "mock stop failure",
                stdout: "",
                stderr: "mock error"
            )
        }
    }

    func getState(_ serviceName: String) async throws -> String {
        stateCallCount += 1
        lastServiceName = serviceName
        if shouldFailGetState {
            throw RegistryContainerService.RegistryError.commandFailed(
                "mock getState failure",
                stdout: "",
                stderr: "mock error"
            )
        }
        return stateToReturn
    }

    func reset() {
        stateToReturn = "active"
        shouldFailStart = false
        shouldFailStop = false
        shouldFailGetState = false
        startCallCount = 0
        stopCallCount = 0
        stateCallCount = 0
        lastServiceName = nil
    }
}
