#if os(macOS)
    import Foundation

    enum InstallBoard: String, CaseIterable, Identifiable {
        case pi5 = "raspberry-pi-5"
        case pi4 = "raspberry-pi-4"
        case pi3 = "raspberry-pi-3"
        case orinNano = "jetson-orin-nano"
        case agxOrin = "jetson-agx-orin"
        case thor = "jetson-agx-thor"
        case iq8275 = "dragonwing-iq-8275"
        case iq9075 = "dragonwing-iq-9075"
        case unoQ = "arduino-uno-q"
        case other
        var id: String { rawValue }
        var title: String {
            switch self {
            case .pi5: "Raspberry Pi 5"
            case .pi4: "Raspberry Pi 4"
            case .pi3: "Raspberry Pi 3"
            case .orinNano: "Jetson Orin Nano developer kit"
            case .agxOrin: "Jetson AGX Orin developer kit"
            case .thor: "Jetson AGX Thor developer kit"
            case .iq8275: "Dragonwing IQ-8275 EVK"
            case .iq9075: "Dragonwing IQ-9075 EVK"
            case .unoQ: "Arduino UNO Q"
            case .other: "Wendy Lite, existing Linux / Mac, or another device"
            }
        }
        var methods: [InstallMethod] {
            switch self {
            case .pi3, .pi4: [.sd]
            case .pi5: [.sd, .nvme]
            case .orinNano: [.recoveryNVMe, .rootfsNVMe, .rootfsSD]
            case .agxOrin: [.recoveryNVMe, .recoveryEMMC, .rootfsNVMe]
            case .thor: [.thorRecovery]
            case .iq8275, .iq9075, .unoQ: [.edl]
            case .other: [.guided]
            }
        }
    }

    enum InstallMethod: String, Identifiable {
        case sd, nvme, recoveryNVMe, recoveryEMMC, rootfsNVMe, rootfsSD, thorRecovery, edl, guided
        var id: String { rawValue }
        var title: String {
            switch self {
            case .sd: "microSD card"
            case .nvme: "NVMe in a USB enclosure"
            case .recoveryNVMe: "Full USB recovery to installed NVMe"
            case .recoveryEMMC: "Full USB recovery to onboard eMMC"
            case .rootfsNVMe: "Rootfs only, NVMe in a USB enclosure"
            case .rootfsSD: "Rootfs only, microSD card"
            case .thorRecovery: "USB recovery to internal NVMe"
            case .edl: "USB Emergency Download mode"
            case .guided: "Choose in the installer"
            }
        }
        var isSD: Bool { self == .sd || self == .rootfsSD }
        var isMedia: Bool { [.sd, .nvme, .rootfsNVMe, .rootfsSD].contains(self) }
        var rootfsOnly: Bool { self == .rootfsNVMe || self == .rootfsSD }
    }

    struct InstallGuide {
        let board: InstallBoard
        let method: InstallMethod
        var arguments: [String] {
            guard board != .other else { return ["install"] }
            var args = ["install", "--device-type", board.rawValue]
            switch method {
            case .sd, .rootfsSD: args += ["--storage", "sd"]
            case .nvme, .rootfsNVMe, .recoveryNVMe: args += ["--storage", "nvme"]
            case .recoveryEMMC: args += ["--storage", "emmc"]
            default: break
            }
            if method.rootfsOnly {
                args += ["--rootfs-only"]
            } else if board == .orinNano || board == .agxOrin {
                args += ["--rootfs-only=false"]
            }
            // Leave version, target, networking, enrollment and erase confirmation to
            // the interactive CLI. Never add --force or choose the first host disk.
            return args
        }
        var edlPole: String { board == .iq8275 ? "S2-8" : "SW2-3" }
        var edlSwitchNumber: String { board == .iq8275 ? "8" : "3" }
        var eraseScope: String {
            if method.isMedia {
                return
                    "Erases every partition on the drive you select.\(method.rootfsOnly ? " QSPI boot firmware stays unchanged and must already support this WendyOS release." : "")"
            }
            switch board {
            case .iq8275, .iq9075:
                return
                    "Factory reset of internal UFS, including both OS slots, device identity, enrollment, Wi-Fi and app data."
            case .unoQ:
                return
                    "Factory reset of internal eMMC and boot firmware, including both OS slots, device identity, enrollment, Wi-Fi and app data."
            case .other:
                return
                    "The installer shows the selected device's write scope before you confirm. Agent installs keep the existing operating system."
            default:
                return
                    "Replaces QSPI boot firmware and erases all partitions on the selected OS storage, including /data."
            }
        }
        var preparation: [String] {
            if method.isSD {
                return [
                    "Put the microSD card in a reader attached to your Mac. Use a microSD-to-SD adapter if needed.",
                    "In the installer, match the card's drive name, path and capacity before confirming the write.",
                ]
            }
            if method.isMedia {
                return [
                    "With the board powered off, remove the NVMe SSD and fit it into a USB NVMe enclosure.",
                    "Connect the enclosure to your Mac. Match the SSD's drive name, path and capacity in the installer before confirming the write.",
                ]
            }
            switch board {
            case .orinNano:
                return [
                    "Install the NVMe SSD in the developer kit with power disconnected. Leave it in the Jetson for USB recovery.",
                    "Connect the Jetson's USB-C data port to your Mac. Keep the DC power supply unplugged.",
                    "Find J14, the 12-pin button header under the module at the carrier board's edge. Bridge pin 9 (GND) and pin 10 (FC REC) with a jumper cap or jumper wire. Match the board's labels before connecting.",
                    "While those two pins are bridged, plug in the DC power supply. Remove the jumper after the installer detects the Jetson in recovery mode.",
                    "Keep power and USB attached through final success. Disconnect any other recovery-mode Jetsons.",
                ]
            case .agxOrin:
                return [
                    "Use the NVIDIA developer kit carrier. For NVMe, install the SSD with power disconnected; eMMC is onboard.",
                    "Connect your Mac to the recovery USB-C port next to the 40-pin header using a data cable.",
                    "Hold Force Recovery, tap Power if off or Reset if already on, then release Force Recovery. Keep only the intended recovery-mode Jetson attached.",
                ]
            case .thor:
                return [
                    "Connect the right USB-C port to your Mac with a data cable. Connect the power supply to the left USB-C port.",
                    "Facing the front, press the left Power button. Hold the middle Force Recovery button, tap the right Reset button, then release Force Recovery.",
                    "Keep power and the recovery USB cable connected through final success. Disconnect other recovery-mode Jetsons.",
                ]
            case .iq8275, .iq9075:
                return [
                    "Power off the EVK. Connect its USB0 USB-C port to your Mac using a data cable.",
                    "Find \(edlPole) and move only that pole toward the switch bank's printed ON marking. Leave the other switches unchanged.",
                    "Power on the board. A blank display in EDL is expected. The switch stays on until you move it back.",
                ]
            case .unoQ:
                return [
                    "Unplug the board. Find the 10-pin JCTL header near USB-C. The EDL pair is at the end farthest from USB-C.",
                    "Bridge the EDL pair with a jumper cap or female-to-female jumper wire, then connect USB-C to your Mac while the pair is shorted.",
                    "A blank display in EDL is expected. Remove the short once the installer detects EDL.",
                ]
            case .other:
                return [
                    "For Wendy Lite, connect the board with a USB data cable and select its exact board and serial port in the installer.",
                    "For an existing Linux machine or headless Mac, choose that option in the installer and follow its target-host instructions.",
                    "Unitree G1 PC2 keeps vendor Ubuntu. Use the Unitree agent guide instead of a generic Jetson image.",
                ]
            default: return []
            }
        }
        var afterInstall: [String] {
            if method.isSD {
                return [
                    "After a successful write, eject the card from your Mac and insert it into the powered-off board's microSD slot.",
                    "Connect networking and power, then allow first boot to finish.",
                ]
            }
            if method.isMedia {
                return [
                    "After a successful write, eject the SSD and disconnect the USB enclosure.",
                    board == .pi5
                        ? "With power off, install the SSD in the Pi 5's M.2 HAT, then connect networking and power."
                        : "With power off, install the SSD in the Jetson's M.2 slot, then connect networking and power.",
                ]
            }
            switch board {
            case .iq8275, .iq9075:
                return [
                    "After final success, power off the EVK, return \(edlPole) to OFF, then power it on again.",
                    "If \(edlPole) stays ON, the board returns to EDL instead of booting WendyOS.",
                ]
            case .unoQ:
                return [
                    "After final success, unplug the board, remove the EDL jumper if still attached, and reconnect USB-C."
                ]
            case .other:
                return [
                    "Follow the selected device's completion instructions. Discover the expected device by name or address."
                ]
            default:
                return [
                    "After final success, remove any recovery jumper and follow the installer's reboot or power-cycle instructions.",
                    "Keep the USB data cable connected if you want to reach WendyOS over USB.",
                ]
            }
        }
        var preparationVideo: String? {
            if method.isSD { return "pi-5-micro-sd-to-sd-card-adapter" }
            if method.isMedia { return "jetson-nvme-mac-adapter" }
            if board == .thor { return "jetson-thor-recovery-mode" }
            return nil
        }
        var bootVideo: String? {
            if method.isSD && [.pi3, .pi4, .pi5].contains(board) {
                return "pi-5-insert-micro-sd-card-to-pi"
            }
            if method.isMedia && !method.isSD && board != .pi5 {
                return "jetson-nvme-bottom-insertion"
            }
            return nil
        }
        var documentation: URL {
            let path: String
            switch board {
            case .pi3, .pi4, .pi5: path = "installation/wendyos-raspberry-pi-5"
            case .orinNano, .agxOrin: path = "installation/wendyos-nvidia-jetson-orin-nano"
            case .thor: path = "installation/wendyos-nvidia-jetson-agx-thor"
            default: path = "advanced/clients/wendy-cli/commands/install"
            }
            return URL(string: "https://docs.wendy.dev/latest/\(path)")!
        }
    }
#endif
