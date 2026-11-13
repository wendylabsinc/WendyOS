#if os(macOS)
    import SwiftUI
    import AVKit

    struct InstallGuidanceView: View {
        let guide: InstallGuide
        var boot = false

        var body: some View {
            VStack(alignment: .leading, spacing: 18) {
                if InstallHardwareView.supports(guide.board) {
                    InstallHardwareView(guide: guide, boot: boot)
                }
                if !guide.method.isMedia && guide.board == .orinNano {
                    OrinRecoveryDiagram(boot: boot)
                } else if guide.board == .iq8275 || guide.board == .iq9075 {
                    DragonwingSwitchDiagram(
                        boot: boot,
                        pole: guide.edlPole,
                        number: guide.edlSwitchNumber
                    )
                } else if !boot && guide.method.isMedia {
                    MediaInstallDiagram(sd: guide.method.isSD)
                }
                if let video = boot ? guide.bootVideo : guide.preparationVideo {
                    InstallVideo(name: video)
                        .id(video)
                    Text(videoCaption).font(.caption).foregroundStyle(.secondary)
                }
                ForEach(
                    Array((boot ? guide.afterInstall : guide.preparation).enumerated()),
                    id: \.offset
                ) { index, text in
                    HStack(alignment: .top, spacing: 12) {
                        Text("\(index + 1)").font(.callout.weight(.semibold))
                            .frame(width: 26, height: 26).background(
                                .tint.opacity(0.12),
                                in: Circle()
                            )
                        Text(text).frame(maxWidth: .infinity, alignment: .leading).padding(.top, 3)
                    }
                }
                if guide.board == .orinNano && !guide.method.isMedia {
                    Link(
                        "NVIDIA's recovery instructions",
                        destination: URL(
                            string:
                                "https://docs.nvidia.com/jetson/orin-nano-devkit/user-guide/latest/howto.html#force-recovery-mode"
                        )!
                    )
                    .font(.caption)
                }
                if guide.board == .unoQ {
                    Link(
                        "See Arduino's photo of the JCTL EDL pins",
                        destination: URL(
                            string: "https://docs.arduino.cc/software/app-lab/configure/flash/"
                        )!
                    )
                }
                if guide.board == .other {
                    Link(
                        "Unitree G1 agent installation guide",
                        destination: URL(
                            string:
                                "https://docs.wendy.dev/latest/installation/wendy-agent-unitree-g1"
                        )!
                    )
                }
            }
        }
        private var videoCaption: String {
            if guide.board == .thor {
                return "From the docs: Power on, hold Force Recovery, tap Reset, then release."
            }
            if boot {
                return guide.method.isSD
                    ? "Pi 5 shown. Use your board's microSD slot."
                    : "Install the NVMe with the Jetson powered off."
            }
            return guide.method.isSD
                ? "From the docs: fit a microSD card into an SD adapter for your reader."
                : "From the docs: connect the NVMe enclosure to your Mac."
        }
    }

    private struct InstallVideo: View {
        let name: String
        @State private var player: AVQueuePlayer?
        @State private var looper: AVPlayerLooper?
        var body: some View {
            Group {
                if let player {
                    InstallVideoPlayer(player: player).aspectRatio(16 / 9, contentMode: .fit)
                        .clipShape(RoundedRectangle(cornerRadius: 12))
                        .accessibilityLabel(
                            "Installation video: \(name.replacingOccurrences(of: "-", with: " "))"
                        )
                } else {
                    Label(
                        "The guide video is unavailable in this app build.",
                        systemImage: "video.slash"
                    )
                    .foregroundStyle(.secondary)
                }
            }
            .onAppear {
                guard player == nil else { return }
                guard
                    let url = Bundle.main.url(
                        forResource: name,
                        withExtension: "mp4",
                        subdirectory: "installation"
                    )
                else { return }
                let video = AVQueuePlayer()
                video.isMuted = true
                looper = AVPlayerLooper(player: video, templateItem: AVPlayerItem(url: url))
                player = video
                video.play()
            }
            .onDisappear {
                player?.pause()
                looper?.disableLooping()
                looper = nil
                player?.removeAllItems()
                player = nil
            }
        }
    }

    // SwiftUI VideoPlayer can abort in _AVKit_SwiftUI while resolving
    // AVPlayerView superclass metadata on macOS. Mount AppKit's player directly.
    struct InstallVideoPlayer: NSViewRepresentable {
        let player: AVPlayer
        func makeNSView(context: Context) -> AVPlayerView {
            let view = InstallGuidePlayerView()
            view.controlsStyle = .inline
            view.player = player
            return view
        }
        func updateNSView(_ view: AVPlayerView, context: Context) {
            if view.player !== player { view.player = player }
        }
        static func dismantleNSView(_ view: AVPlayerView, coordinator: ()) {
            view.player?.pause()
            view.player = nil
        }
    }

    private final class InstallGuidePlayerView: AVPlayerView {
        override func hitTest(_ point: NSPoint) -> NSView? {
            let hit = super.hitTest(point)
            // Route scrolling over playback controls through the guide as well.
            if hit != nil, NSApp.currentEvent?.type == .scrollWheel {
                return self
            }
            return hit
        }

        override func scrollWheel(with event: NSEvent) {
            if let scrollView = enclosingScrollView {
                scrollView.scrollWheel(with: event)
            } else {
                nextResponder?.scrollWheel(with: event)
            }
        }
    }

    struct MediaInstallDiagram: View {
        let sd: Bool
        var body: some View {
            HStack(spacing: 18) {
                endpoint(sd ? "sdcard" : "internaldrive", sd ? "microSD" : "NVMe SSD")
                Image(systemName: "arrow.right").foregroundStyle(.secondary)
                endpoint(sd ? "sdcard.fill" : "externaldrive", sd ? "Card reader" : "USB enclosure")
                Image(systemName: "arrow.right").foregroundStyle(.secondary)
                endpoint("laptopcomputer", "Your Mac")
            }.frame(maxWidth: .infinity).padding(24)
                .background(.quaternary.opacity(0.45), in: RoundedRectangle(cornerRadius: 14))
                .accessibilityElement(children: .combine)
        }
        private func endpoint(_ symbol: String, _ label: String) -> some View {
            VStack(spacing: 10) {
                Image(systemName: symbol).font(.system(size: 32)).foregroundStyle(.tint)
                Text(label).font(.caption.weight(.medium))
            }.frame(maxWidth: .infinity)
        }
    }

    // J14 is a SINGLE row of 12 pins. With the USB/ethernet edge facing the viewer,
    // pin 1 is at the right of this top view. See NVIDIA SP-11324-001 v1.3,
    // figure 1-4 and table 3-4. This is a labeled schematic, not a board photograph.
    struct OrinRecoveryDiagram: View {
        var boot = false
        var body: some View {
            VStack(alignment: .leading, spacing: 16) {
                Text(
                    boot ? "Remove the jumper before normal boot" : "Bridge only J14 pins 9 and 10"
                ).font(.headline)
                HStack(alignment: .center, spacing: 20) {
                    VStack(spacing: 8) {
                        Text("J14 • under the module").font(.caption.weight(.semibold))
                            .foregroundStyle(.orange)
                        HStack(spacing: 3) {
                            ForEach((1...12).reversed(), id: \.self) { pin in
                                RoundedRectangle(cornerRadius: 1).fill(
                                    pin == 9 || pin == 10 ? Color.orange : Color.secondary
                                )
                                .frame(width: 6, height: 12)
                            }
                        }
                        RoundedRectangle(cornerRadius: 8).fill(.green.opacity(0.12))
                            .overlay {
                                Text("Jetson module").font(.caption).foregroundStyle(.secondary)
                            }.frame(height: 44)
                        HStack {
                            Text("DC power").padding(5).background(
                                .quaternary,
                                in: RoundedRectangle(cornerRadius: 4)
                            )
                            Spacer()
                            Text("USB-C data").padding(5).background(
                                .tint.opacity(0.15),
                                in: RoundedRectangle(cornerRadius: 4)
                            )
                        }.font(.system(size: 10, weight: .medium))
                    }.padding(14).frame(width: 230)
                        .background(.green.opacity(0.06), in: RoundedRectangle(cornerRadius: 12))
                        .overlay { RoundedRectangle(cornerRadius: 12).stroke(.green.opacity(0.4)) }
                    VStack(alignment: .leading, spacing: 8) {
                        Label("9 · GND", systemImage: "circle.fill")
                        Label("10 · FC REC", systemImage: "circle.fill")
                        Text(
                            boot
                                ? "No bridge for normal boot."
                                : "Fit the jumper with DC power unplugged. Keep it in place while connecting DC power."
                        )
                        .font(.caption).foregroundStyle(.secondary)
                    }.font(.callout.weight(.semibold))
                }
                HStack(spacing: 0) {
                    ForEach((1...12).reversed(), id: \.self) { pin in
                        VStack(spacing: 8) {
                            Circle().fill(
                                pin == 9 || pin == 10 ? Color.orange : Color.secondary.opacity(0.45)
                            )
                            .frame(width: 12, height: 12)
                            Text("\(pin)").font(.system(.caption, design: .monospaced))
                        }.frame(maxWidth: .infinity).padding(.vertical, 12)
                            .background(
                                !boot && (pin == 9 || pin == 10)
                                    ? Color.orange.opacity(0.25) : Color.clear
                            )
                    }
                }.background(.primary.opacity(0.05), in: RoundedRectangle(cornerRadius: 6))
                    .overlay {
                        if !boot {
                            GeometryReader { geometry in
                                Path { path in
                                    path.move(to: CGPoint(x: geometry.size.width * 2.5 / 12, y: 18))
                                    path.addLine(
                                        to: CGPoint(x: geometry.size.width * 3.5 / 12, y: 18)
                                    )
                                }.stroke(.orange, style: StrokeStyle(lineWidth: 5, lineCap: .round))
                            }.allowsHitTesting(false)
                        }
                    }
                Text(
                    "J14 close-up, top view with the USB / Ethernet connectors facing you. The highlighted pair is 10–9. Confirm FC REC and GND on your board; J14 is separate from the 40-pin GPIO header."
                )
                .font(.caption).foregroundStyle(.secondary)
            }.padding(18).background(
                .quaternary.opacity(0.35),
                in: RoundedRectangle(cornerRadius: 14)
            )
            .accessibilityElement(children: .combine)
        }
    }

    struct DragonwingSwitchDiagram: View {
        var boot = false
        var pole = "SW2-3"
        var number = "3"
        var body: some View {
            HStack(spacing: 28) {
                VStack(spacing: 8) {
                    Text("ON").font(.caption.bold()).foregroundStyle(
                        boot ? Color.secondary : Color.orange
                    )
                    RoundedRectangle(cornerRadius: 6).fill(.primary.opacity(0.12))
                        .frame(width: 44, height: 76)
                        .overlay(alignment: boot ? .bottom : .top) {
                            RoundedRectangle(cornerRadius: 4).fill(
                                boot ? Color.accentColor : Color.orange
                            )
                            .frame(width: 34, height: 26).padding(5)
                        }
                    Text(number).font(.title3.monospaced().bold())
                    Text("OFF").font(.caption.bold()).foregroundStyle(
                        boot ? Color.accentColor : Color.secondary
                    )
                }
                VStack(alignment: .leading, spacing: 10) {
                    Text(boot ? "Return \(pole) to OFF" : "Set \(pole) to ON").font(.headline)
                    Text(
                        boot
                            ? "Power off, move \(pole) away from ON, then power on to boot WendyOS."
                            : "Power off, move \(pole) toward the printed ON label, then power on to enter EDL."
                    )
                    Text(
                        "\(pole) only. Leave the other switches unchanged. Match the ON marking on your EVK; this drawing shows the switch upright."
                    )
                    .font(.caption).foregroundStyle(.secondary)
                    Label("Mac ↔ USB0 (USB-C)", systemImage: "cable.connector").font(.callout)
                }
            }.padding(22).frame(maxWidth: .infinity, alignment: .leading)
                .background(.quaternary.opacity(0.35), in: RoundedRectangle(cornerRadius: 14))
                .accessibilityElement(children: .combine)
        }
    }
#endif
