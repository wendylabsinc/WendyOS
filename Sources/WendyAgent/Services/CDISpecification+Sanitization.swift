import Foundation

extension CDISpecification {
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
