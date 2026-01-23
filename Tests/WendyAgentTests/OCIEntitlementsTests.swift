import AppConfig
import Foundation
import Testing

@testable import wendy_agent

@Suite("OCI Entitlements")
struct OCIEntitlementsTests {

    // MARK: - GPU Entitlement Tests

    @Test("GPU entitlement adds video group")
    func applyGPUEntitlement_AddsVideoGroup() {
        // Given: An OCI spec with no additional groups
        var ociSpec = createBaseOCISpec()
        #expect(ociSpec.process.user.additionalGids == [])

        // When: Applying GPU entitlement
        let entitlements: [Entitlement] = [.gpu(GPUEntitlements())]
        let dependencies = ociSpec.applyEntitlements(
            entitlements: entitlements,
            appName: "test-app",
            persistenceBasePath: URL(filePath: "/tmp/wendy-agent-persistence")
        )

        // Then: Video group (gid 44) should be added
        #expect(ociSpec.process.user.additionalGids.contains(44))
        #expect(ociSpec.process.user.additionalGids.count == 1)
        #expect(dependencies.directoriesToCreate.isEmpty)
    }

    @Test("GPU entitlement does not duplicate video group")
    func applyGPUEntitlement_DoesNotDuplicateVideoGroup() {
        // Given: An OCI spec that already has video group
        var ociSpec = createBaseOCISpec()
        ociSpec.process.user.additionalGids = [44]

        // When: Applying GPU entitlement again
        let entitlements: [Entitlement] = [.gpu(GPUEntitlements())]
        let dependencies = ociSpec.applyEntitlements(
            entitlements: entitlements,
            appName: "test-app",
            persistenceBasePath: URL(filePath: "/tmp/wendy-agent-persistence")
        )

        // Then: Video group should not be duplicated
        #expect(ociSpec.process.user.additionalGids.filter { $0 == 44 }.count == 1)
        #expect(dependencies.directoriesToCreate.isEmpty)
    }

    @Test("GPU entitlement preserves existing groups")
    func applyGPUEntitlement_PreservesExistingGroups() {
        // Given: An OCI spec with other groups
        var ociSpec = createBaseOCISpec()
        ociSpec.process.user.additionalGids = [100, 200]

        // When: Applying GPU entitlement
        let entitlements: [Entitlement] = [.gpu(GPUEntitlements())]
        let dependencies = ociSpec.applyEntitlements(
            entitlements: entitlements,
            appName: "test-app",
            persistenceBasePath: URL(filePath: "/tmp/wendy-agent-persistence")
        )

        // Then: Existing groups should be preserved and video group added
        #expect(ociSpec.process.user.additionalGids.contains(44))
        #expect(ociSpec.process.user.additionalGids.contains(100))
        #expect(ociSpec.process.user.additionalGids.contains(200))
        #expect(ociSpec.process.user.additionalGids.count == 3)
        #expect(dependencies.directoriesToCreate.isEmpty)
    }

    // MARK: - Multiple Entitlements Tests

    @Test("Multiple entitlements are all applied")
    func applyMultipleEntitlements_AppliesAll() {
        // Given: An OCI spec with no entitlements
        var ociSpec = createBaseOCISpec()

        // When: Applying multiple entitlements
        let entitlements: [Entitlement] = [
            .gpu(GPUEntitlements()),
            .network(NetworkEntitlements(mode: .host)),
        ]
        let dependencies = ociSpec.applyEntitlements(
            entitlements: entitlements,
            appName: "test-app",
            persistenceBasePath: URL(filePath: "/tmp/wendy-agent-persistence")
        )

        // Then: All entitlements should be applied
        #expect(ociSpec.process.user.additionalGids.contains(44))
        #expect(dependencies.directoriesToCreate.isEmpty)
    }

    // MARK: - Network Entitlement Tests

    @Test("Network entitlement sets host mode")
    func applyNetworkEntitlement_HostMode() {
        // Given: An OCI spec
        var ociSpec = createBaseOCISpec()

        // When: Applying host network entitlement
        let entitlements: [Entitlement] = [.network(NetworkEntitlements(mode: .host))]
        let dependencies = ociSpec.applyEntitlements(
            entitlements: entitlements,
            appName: "test-app",
            persistenceBasePath: URL(filePath: "/tmp/wendy-agent-persistence")
        )

        // Then: Network namespace should NOT be present (host networking)
        #expect(!ociSpec.linux.namespaces.contains(where: { $0.type == "network" }))
        #expect(dependencies.directoriesToCreate.isEmpty)
    }

    @Test("Host network mode mounts systemd-resolved resolv.conf for DNS")
    func applyNetworkEntitlement_HostMode_MountsSystemdResolvedConf() {
        // Given: An OCI spec
        var ociSpec = createBaseOCISpec()

        // When: Applying host network entitlement
        let entitlements: [Entitlement] = [.network(NetworkEntitlements(mode: .host))]
        let dependencies = ociSpec.applyEntitlements(
            entitlements: entitlements,
            appName: "test-app",
            persistenceBasePath: URL(filePath: "/tmp/wendy-agent-persistence")
        )

        // Then: /run/systemd/resolve/resolv.conf should be bind-mounted to /etc/resolv.conf
        let resolvConfMount = ociSpec.mounts.first(where: { $0.destination == "/etc/resolv.conf" })
        #expect(resolvConfMount != nil)
        #expect(resolvConfMount?.type == "bind")
        #expect(resolvConfMount?.source == "/run/systemd/resolve/resolv.conf")
        #expect(resolvConfMount?.options?.contains("rbind") == true)
        #expect(resolvConfMount?.options?.contains("ro") == true)
        #expect(dependencies.directoriesToCreate.isEmpty)
    }

    @Test("Host network mode does not mount /etc/hosts")
    func applyNetworkEntitlement_HostMode_DoesNotMountHosts() {
        // Given: An OCI spec
        var ociSpec = createBaseOCISpec()

        // When: Applying host network entitlement
        let entitlements: [Entitlement] = [.network(NetworkEntitlements(mode: .host))]
        let dependencies = ociSpec.applyEntitlements(
            entitlements: entitlements,
            appName: "test-app",
            persistenceBasePath: URL(filePath: "/tmp/wendy-agent-persistence")
        )

        // Then: /etc/hosts should NOT be mounted (containerd manages its own)
        let hostsMount = ociSpec.mounts.first(where: { $0.destination == "/etc/hosts" })
        #expect(hostsMount == nil)
        #expect(dependencies.directoriesToCreate.isEmpty)
    }

    @Test("Host network mode does not duplicate resolv.conf mount")
    func applyNetworkEntitlement_HostMode_DoesNotDuplicateResolvConf() {
        // Given: An OCI spec that already has /etc/resolv.conf mounted
        var ociSpec = createBaseOCISpec()
        ociSpec.mounts.append(
            .init(
                destination: "/etc/resolv.conf",
                type: "bind",
                source: "/run/systemd/resolve/resolv.conf",
                options: ["rbind", "ro"]
            )
        )

        // When: Applying host network entitlement
        let entitlements: [Entitlement] = [.network(NetworkEntitlements(mode: .host))]
        let dependencies = ociSpec.applyEntitlements(
            entitlements: entitlements,
            appName: "test-app",
            persistenceBasePath: URL(filePath: "/tmp/wendy-agent-persistence")
        )

        // Then: /etc/resolv.conf should not be duplicated
        let resolvConfMounts = ociSpec.mounts.filter { $0.destination == "/etc/resolv.conf" }
        #expect(resolvConfMounts.count == 1)
        #expect(dependencies.directoriesToCreate.isEmpty)
    }

    @Test("Host network mode removes network namespace if present")
    func applyNetworkEntitlement_HostMode_RemovesNetworkNamespace() {
        // Given: An OCI spec with a network namespace already added
        var ociSpec = createBaseOCISpec()
        ociSpec.linux.namespaces.append(.init(type: "network"))
        #expect(ociSpec.linux.namespaces.contains(where: { $0.type == "network" }))

        // When: Applying host network entitlement
        let entitlements: [Entitlement] = [.network(NetworkEntitlements(mode: .host))]
        let dependencies = ociSpec.applyEntitlements(
            entitlements: entitlements,
            appName: "test-app",
            persistenceBasePath: URL(filePath: "/tmp/wendy-agent-persistence")
        )

        // Then: Network namespace should be removed
        #expect(!ociSpec.linux.namespaces.contains(where: { $0.type == "network" }))
        #expect(dependencies.directoriesToCreate.isEmpty)
    }

    @Test("Network entitlement sets none mode")
    func applyNetworkEntitlement_NoneMode() {
        // Given: An OCI spec
        var ociSpec = createBaseOCISpec()

        // When: Applying none network entitlement
        let entitlements: [Entitlement] = [.network(NetworkEntitlements(mode: .none))]
        let dependencies = ociSpec.applyEntitlements(
            entitlements: entitlements,
            appName: "test-app",
            persistenceBasePath: URL(filePath: "/tmp/wendy-agent-persistence")
        )

        // Then: Network namespace should be added (isolated networking)
        #expect(ociSpec.linux.namespaces.contains(where: { $0.type == "network" }))
        #expect(dependencies.directoriesToCreate.isEmpty)
    }

    @Test("Isolated network mode does not mount resolv.conf")
    func applyNetworkEntitlement_NoneMode_DoesNotMountDNSFiles() {
        // Given: An OCI spec
        var ociSpec = createBaseOCISpec()

        // When: Applying none (isolated) network entitlement
        let entitlements: [Entitlement] = [.network(NetworkEntitlements(mode: .none))]
        let dependencies = ociSpec.applyEntitlements(
            entitlements: entitlements,
            appName: "test-app",
            persistenceBasePath: URL(filePath: "/tmp/wendy-agent-persistence")
        )

        // Then: /etc/resolv.conf should NOT be mounted
        // (Containerd runtime manages DNS for isolated networking)
        let resolvConfMount = ociSpec.mounts.first(where: { $0.destination == "/etc/resolv.conf" })
        #expect(resolvConfMount == nil)
        #expect(dependencies.directoriesToCreate.isEmpty)
    }

    // MARK: - Audio Entitlement Tests

    @Test("Audio entitlement adds device allowance")
    func applyAudioEntitlement_AddsDeviceAllowance() {
        // Given: An OCI spec
        var ociSpec = createBaseOCISpec()

        // When: Applying audio entitlement
        let entitlements: [Entitlement] = [.audio]
        let dependencies = ociSpec.applyEntitlements(
            entitlements: entitlements,
            appName: "test-app",
            persistenceBasePath: URL(filePath: "/tmp/wendy-agent-persistence")
        )

        // Then: Audio device allowance should be added
        #expect(ociSpec.linux.resources != nil)
        #expect(ociSpec.linux.resources?.devices != nil)

        let audioDeviceAllowance = ociSpec.linux.resources?.devices?.first(where: { device in
            device.type == "c" && device.major == 116
        })

        #expect(audioDeviceAllowance != nil)
        #expect(audioDeviceAllowance?.access == "rw")

        // Check that /dev/snd mount is added
        let sndMount = ociSpec.mounts.first(where: { $0.destination == "/dev/snd" })
        #expect(sndMount != nil)
        #expect(sndMount?.type == "bind")
        #expect(dependencies.directoriesToCreate.isEmpty)
    }

    // MARK: - Video Entitlement Tests

    @Test("Video entitlement adds device and mount")
    func applyVideoEntitlement_AddsDeviceAndMount() {
        // Given: An OCI spec
        var ociSpec = createBaseOCISpec()

        // When: Applying video entitlement
        let entitlements: [Entitlement] = [.video(VideoEntitlements())]
        let devices = OCI.AvailableDevices(devices: [
            Device(
                path: "/dev/video0",
                type: "c",
                major: 81,
                minor: 17,
                fileMode: 0o666,
                uid: 0,
                gid: 0
            )
        ])
        let dependencies = ociSpec.applyEntitlements(
            entitlements: entitlements,
            appName: "test-app",
            availableDevices: devices,
            persistenceBasePath: URL(filePath: "/tmp/wendy-agent-persistence")
        )

        // Then: Video device should be added
        let videoDevice = ociSpec.linux.devices.first(where: { $0.path == "/dev/video0" })
        #expect(videoDevice != nil)
        #expect(videoDevice?.major == 81)
        #expect(videoDevice?.minor == 17)

        // Video device mount should be added
        let videoMount = ociSpec.mounts.first(where: { $0.destination == "/dev/video0" })
        #expect(videoMount != nil)
        #expect(dependencies.directoriesToCreate.isEmpty)
    }

    // MARK: - Empty Entitlements Tests

    @Test("Empty entitlements make no changes")
    func applyEmptyEntitlements_NoChanges() {
        // Given: An OCI spec
        var ociSpec = createBaseOCISpec()
        let originalGids = ociSpec.process.user.additionalGids

        // When: Applying empty entitlements array
        let dependencies = ociSpec.applyEntitlements(
            entitlements: [],
            appName: "test-app",
            persistenceBasePath: URL(filePath: "/tmp/wendy-agent-persistence")
        )

        // Then: No changes should be made
        #expect(ociSpec.process.user.additionalGids == originalGids)
        #expect(dependencies.directoriesToCreate.isEmpty)
    }

    @Test("Persist entitlement creates directory")
    func applyPersistEntitlement_CreatesDirectory() {
        // Given: An OCI spec
        var ociSpec = createBaseOCISpec()

        // When: Applying persist entitlement
        let entitlements: [Entitlement] = [
            .persist(
                PersistenceEntitlements(name: "test-volume", path: "/tmp/wendy-agent-persistence")
            )
        ]
        let dependencies = ociSpec.applyEntitlements(
            entitlements: entitlements,
            appName: "test-app",
            persistenceBasePath: URL(filePath: "/tmp/wendy-agent-persistence")
        )

        // Then: Directory should be created
        #expect(
            dependencies.directoriesToCreate.contains(
                URL(filePath: "/tmp/wendy-agent-persistence/test-volume")
            )
        )
    }

    // MARK: - Helper Methods

    private func createBaseOCISpec() -> OCI {
        return OCI(
            args: ["/bin/sh"],
            env: ["PATH=/usr/bin:/bin"],
            workingDir: "/",
            appName: "test-app"
        )
    }
}

extension OCI {
    @discardableResult
    mutating func applyEntitlements(
        entitlements: [Entitlement],
        appName: String,
        persistenceBasePath: URL
    ) -> OCIDependencies {
        let availableDevices = OCI.AvailableDevices(devices: [])
        return self.applyEntitlements(
            entitlements: entitlements,
            appName: appName,
            availableDevices: availableDevices,
            persistenceBasePath: persistenceBasePath
        )
    }
}
