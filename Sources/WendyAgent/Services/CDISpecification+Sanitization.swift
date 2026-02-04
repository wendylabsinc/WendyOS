import Foundation

extension CDISpecification {
    func removingEnvKeys(_ keys: Set<String>) -> CDISpecification {
        func filteredEnv(_ env: [String]?) -> [String]? {
            guard let env else { return nil }
            let filtered = env.filter { entry in
                let key = entry.split(separator: "=", maxSplits: 1).first.map(String.init) ?? entry
                return !keys.contains(key)
            }
            return filtered.isEmpty ? nil : filtered
        }

        func filteredEdits(_ edits: CDIContainerEdits) -> CDIContainerEdits {
            CDIContainerEdits(
                deviceNodes: edits.deviceNodes,
                mounts: edits.mounts,
                env: filteredEnv(edits.env),
                hooks: edits.hooks
            )
        }

        let newContainerEdits = containerEdits.map(filteredEdits)
        let newDevices = devices.map { device in
            CDIDevice(name: device.name, containerEdits: filteredEdits(device.containerEdits))
        }

        return CDISpecification(
            devices: newDevices,
            cdiVersion: cdiVersion,
            kind: kind,
            containerEdits: newContainerEdits
        )
    }

    func removingHooks(_ shouldRemove: (CDIHook) -> Bool) -> CDISpecification {
        func filteredHooks(_ hooks: [CDIHook]?) -> [CDIHook]? {
            guard let hooks else { return nil }
            let filtered = hooks.filter { !shouldRemove($0) }
            return filtered.isEmpty ? nil : filtered
        }

        func filteredEdits(_ edits: CDIContainerEdits) -> CDIContainerEdits {
            CDIContainerEdits(
                deviceNodes: edits.deviceNodes,
                mounts: edits.mounts,
                env: edits.env,
                hooks: filteredHooks(edits.hooks)
            )
        }

        let newContainerEdits = containerEdits.map(filteredEdits)
        let newDevices = devices.map { device in
            CDIDevice(name: device.name, containerEdits: filteredEdits(device.containerEdits))
        }

        return CDISpecification(
            devices: newDevices,
            cdiVersion: cdiVersion,
            kind: kind,
            containerEdits: newContainerEdits
        )
    }

    func removingMounts(_ shouldRemove: (CDIMount) -> Bool) -> CDISpecification {
        func filteredMounts(_ mounts: [CDIMount]?) -> [CDIMount]? {
            guard let mounts else { return nil }
            let filtered = mounts.filter { !shouldRemove($0) }
            return filtered.isEmpty ? nil : filtered
        }

        func filteredEdits(_ edits: CDIContainerEdits) -> CDIContainerEdits {
            CDIContainerEdits(
                deviceNodes: edits.deviceNodes,
                mounts: filteredMounts(edits.mounts),
                env: edits.env,
                hooks: edits.hooks
            )
        }

        let newContainerEdits = containerEdits.map(filteredEdits)
        let newDevices = devices.map { device in
            CDIDevice(name: device.name, containerEdits: filteredEdits(device.containerEdits))
        }

        return CDISpecification(
            devices: newDevices,
            cdiVersion: cdiVersion,
            kind: kind,
            containerEdits: newContainerEdits
        )
    }
}
