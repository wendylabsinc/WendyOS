#if os(macOS)
    import Foundation
    import SystemConfiguration
    import WendyShared
    @testable import wendy

    // Unique identifiers for mock interfaces that we can use as opaque SCNetworkInterface values
    private final class MockInterfaceID {}

    /// Mock implementation of NetworkInterfaceProvider for testing
    final class MockNetworkInterfaceProvider: NetworkInterfaceProvider, @unchecked Sendable {
        /// Mock network interfaces to return during testing
        var mockInterfaces: [MockNetworkInterfaceData] = []

        /// Internal mapping of mock interfaces to their IDs
        private var interfaceIDs: [SCNetworkInterface] = []

        /// Track method calls for verification
        var copyAllNetworkInterfacesCalls: Int = 0
        var getInterfaceTypeCalls: [(SCNetworkInterface)] = []
        var getBSDNameCalls: [(SCNetworkInterface)] = []
        var getLocalizedDisplayNameCalls: [(SCNetworkInterface)] = []
        var getHardwareAddressStringCalls: [(SCNetworkInterface)] = []
        var getAllLinkSpeedsCalls: Int = 0

        func copyAllNetworkInterfaces() -> [SCNetworkInterface]? {
            copyAllNetworkInterfacesCalls += 1

            if mockInterfaces.isEmpty {
                return nil
            }

            // Create mock interface identifiers that will be treated as SCNetworkInterface objects
            interfaceIDs = []

            // We can use any arbitrary object as the "SCNetworkInterface" opaque reference
            // For simplicity, we'll just use any object that can be distinguished from others
            for _ in mockInterfaces {
                // We use unsafeBitCast to avoid the compiler warning about object types
                // In Swift, we can cast any AnyObject to SCNetworkInterface for storage
                // This works because SCNetworkInterface is just an opaque reference type from ObjC
                let mockID = MockInterfaceID()
                let interface = unsafeBitCast(mockID, to: SCNetworkInterface.self)
                interfaceIDs.append(interface)
            }

            return interfaceIDs
        }

        func getInterfaceType(interface: SCNetworkInterface) -> String? {
            getInterfaceTypeCalls.append(interface)

            // Find the index of this interface in our array
            if let index = interfaceIDs.firstIndex(where: { $0 === interface }),
                index < mockInterfaces.count
            {
                return mockInterfaces[index].interfaceType
            }

            return nil
        }

        func getBSDName(interface: SCNetworkInterface) -> String? {
            getBSDNameCalls.append(interface)

            if let index = interfaceIDs.firstIndex(where: { $0 === interface }),
                index < mockInterfaces.count
            {
                return mockInterfaces[index].bsdName
            }

            return nil
        }

        func getLocalizedDisplayName(interface: SCNetworkInterface) -> String? {
            getLocalizedDisplayNameCalls.append(interface)

            if let index = interfaceIDs.firstIndex(where: { $0 === interface }),
                index < mockInterfaces.count
            {
                return mockInterfaces[index].displayName
            }

            return nil
        }

        func getHardwareAddressString(interface: SCNetworkInterface) -> String? {
            getHardwareAddressStringCalls.append(interface)

            if let index = interfaceIDs.firstIndex(where: { $0 === interface }),
                index < mockInterfaces.count
            {
                return mockInterfaces[index].macAddress
            }

            return nil
        }

        func getAllLinkSpeeds() -> [String: String] {
            getAllLinkSpeedsCalls += 1

            // Build dictionary from all mock interfaces that have link speeds
            var speeds: [String: String] = [:]
            for mockInterface in mockInterfaces {
                if let speed = mockInterface.linkSpeed {
                    speeds[mockInterface.bsdName] = speed
                }
            }
            return speeds
        }

        /// Reset all tracking data for a new test
        func reset() {
            mockInterfaces = []
            interfaceIDs = []
            copyAllNetworkInterfacesCalls = 0
            getInterfaceTypeCalls = []
            getBSDNameCalls = []
            getLocalizedDisplayNameCalls = []
            getHardwareAddressStringCalls = []
            getAllLinkSpeedsCalls = 0
        }
    }

    /// Mock data model for network interfaces that will be used by the mock provider
    struct MockNetworkInterfaceData {
        let bsdName: String
        let displayName: String
        let interfaceType: String
        let macAddress: String?
        let linkSpeed: String?

        init(
            bsdName: String,
            displayName: String,
            interfaceType: String,
            macAddress: String?,
            linkSpeed: String? = nil
        ) {
            self.bsdName = bsdName
            self.displayName = displayName
            self.interfaceType = interfaceType
            self.macAddress = macAddress
            self.linkSpeed = linkSpeed
        }
    }
#endif
