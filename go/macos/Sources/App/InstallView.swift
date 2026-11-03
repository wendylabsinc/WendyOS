#if os(macOS)
    import SwiftUI

    struct InstallView: View {
        @ObservedObject var session: InstallSession
        @Environment(\.dismiss) private var dismiss
        @State private var understood = false
        @State private var stopConfirmation = false
        var discover: () -> Void
        private let stages = ["Choose device", "Prepare", "Install", "Boot & verify"]

        var body: some View {
            VStack(spacing: 0) {
                HStack {
                    VStack(alignment: .leading, spacing: 4) {
                        Text("Install Wendy").font(.title2.weight(.semibold))
                        Text(session.page == 0 ? "Set up a new device" : session.board.title)
                            .foregroundStyle(.secondary)
                    }
                    Spacer()
                    Link("Installation docs", destination: session.guide.documentation)
                    Button("Close") { dismiss() }.disabled(session.running)
                }.padding(22)
                HStack(spacing: 12) {
                    ForEach(Array(stages.enumerated()), id: \.offset) { i, title in
                        HStack(spacing: 7) {
                            Text("\(i + 1)").font(.caption.bold()).frame(width: 24, height: 24)
                                .background(
                                    session.page == i
                                        ? Color.accentColor.opacity(0.18)
                                        : Color.secondary.opacity(0.1),
                                    in: Circle()
                                )
                            Text(title).font(
                                .callout.weight(session.page == i ? .semibold : .regular)
                            )
                        }.foregroundStyle(session.page == i ? Color.primary : Color.secondary)
                        if i < stages.count - 1 {
                            Image(systemName: "chevron.right").font(.caption2).foregroundStyle(
                                .tertiary
                            )
                        }
                    }
                    Spacer()
                }.padding(.horizontal, 22).padding(.bottom, 16)
                Divider()
                Group {
                    switch session.page {
                    case 0: choices
                    case 1: preparation
                    case 2: installation
                    default: boot
                    }
                }.frame(maxWidth: .infinity, maxHeight: .infinity)
                if let error = session.error {
                    Text(error).foregroundStyle(.red).textSelection(.enabled).padding(12)
                }
                Divider()
                footer.padding(18)
            }
            .frame(minWidth: 860, idealWidth: 1040, minHeight: 660, idealHeight: 780)
            .interactiveDismissDisabled(session.running)
            .alert("Stop the installer?", isPresented: $stopConfirmation) {
                Button("Keep installing", role: .cancel) {}
                Button("Send interrupt", role: .destructive) { session.interrupt() }
            } message: {
                Text(
                    "Interrupting a write may leave the board unable to boot. Keep power and USB connected until the installer stops and reports its state."
                )
            }
        }
        private var choices: some View {
            ScrollView {
                VStack(alignment: .leading, spacing: 22) {
                    Text("Choose the board and where WendyOS will be written.").font(.title3)
                    Picker(
                        "Device",
                        selection: Binding(get: { session.board }, set: { session.select($0) })
                    ) {
                        ForEach(InstallBoard.allCases) { Text($0.title).tag($0) }
                    }
                    VStack(alignment: .leading, spacing: 12) {
                        Text("Installation method").font(.headline)
                        ForEach(session.board.methods) { method in
                            Button {
                                session.method = method
                            } label: {
                                HStack {
                                    Image(
                                        systemName: session.method == method
                                            ? "largecircle.fill.circle" : "circle"
                                    )
                                    .foregroundStyle(.tint)
                                    VStack(alignment: .leading, spacing: 5) {
                                        Text(method.title).fontWeight(.medium)
                                        if method.rootfsOnly {
                                            Text(
                                                "Requires compatible boot firmware and a published rootfs image."
                                            ).font(.caption).foregroundStyle(.secondary)
                                        }
                                    }
                                    Spacer()
                                }.padding(16).background(
                                    session.method == method
                                        ? Color.accentColor.opacity(0.08)
                                        : Color.secondary.opacity(0.04),
                                    in: RoundedRectangle(cornerRadius: 10)
                                )
                            }.buttonStyle(.plain).accessibilityAddTraits(
                                session.method == method ? .isSelected : []
                            )
                        }
                    }
                    Text(session.guide.eraseScope).foregroundStyle(.secondary)
                    if [.orinNano, .agxOrin, .thor].contains(session.board) {
                        Label(
                            "These recovery steps are for NVIDIA developer kits. Custom carriers and robots need their vendor's instructions.",
                            systemImage: "info.circle"
                        )
                        .font(.callout)
                    }
                    Text(
                        "The installer checks published releases and available storage images. It will report when a selected combination is unavailable."
                    ).font(.caption).foregroundStyle(.secondary)
                }.padding(28).frame(maxWidth: 740)
                    .frame(maxWidth: .infinity)
            }
        }
        private var preparation: some View {
            ScrollView {
                VStack(alignment: .leading, spacing: 22) {
                    InstallGuidanceView(guide: session.guide)
                    Label(session.guide.eraseScope, systemImage: "exclamationmark.triangle")
                        .padding(16).frame(maxWidth: .infinity, alignment: .leading)
                        .background(.orange.opacity(0.1), in: RoundedRectangle(cornerRadius: 10))
                    Toggle(
                        session.guide.method.rootfsOnly
                            ? "The boot firmware is compatible, and I have prepared the target drive."
                            : "I have prepared the intended board or drive.",
                        isOn: $understood
                    )
                    Text(
                        "Next, select the release and target, configure networking, and review the erase confirmation in the installer. Your administrator password may be requested there."
                    )
                    .font(.callout).foregroundStyle(.secondary)
                }.padding(24).frame(maxWidth: 760).frame(maxWidth: .infinity)
            }
        }
        private var installation: some View {
            VStack(alignment: .leading, spacing: 12) {
                HStack {
                    if session.running { ProgressView().controlSize(.small) }
                    Text(
                        session.result
                            ?? "Use the arrow keys and Return to answer the installer prompts below."
                    )
                    .font(.callout)
                    Spacer()
                    if session.running { Button("Stop…") { stopConfirmation = true } }
                }
                if let terminal = session.terminal {
                    InstallTerminal(terminal: terminal).id(ObjectIdentifier(terminal))
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                        .clipShape(RoundedRectangle(cornerRadius: 8))
                }
                DisclosureGroup("Visual setup guide") {
                    ScrollView {
                        InstallGuidanceView(guide: session.guide).padding(12)
                    }.frame(maxHeight: 240)
                }
                Text(
                    "Keep the app, power and USB connected during flashing. If macOS asks to initialize an unreadable disk, choose Ignore. If disk access is denied, allow Wendy in System Settings → Privacy & Security → Full Disk Access."
                )
                .font(.caption).foregroundStyle(.secondary)
            }.padding(20)
        }
        private var boot: some View {
            ScrollView {
                VStack(alignment: .leading, spacing: 22) {
                    InstallGuidanceView(guide: session.guide, boot: true)
                    Divider()
                    Text("Verify this device").font(.headline)
                    Text(
                        "Allow first boot to finish. Enter the expected device's hostname or IP address. Verification checks its agent and, for WendyOS, the board and release you specify."
                    ).foregroundStyle(.secondary)
                    TextField("Hostname or IP address", text: $session.address).textFieldStyle(
                        .roundedBorder
                    )
                    if session.board != .other {
                        TextField("WendyOS version shown by the installer", text: $session.version)
                            .textFieldStyle(.roundedBorder)
                    }
                    Toggle("Require cloud enrollment", isOn: $session.requireEnrollment)
                    HStack {
                        Button("Verify first boot") { session.verify() }
                            .disabled(
                                session.address.trimmingCharacters(in: .whitespacesAndNewlines)
                                    .isEmpty
                                    || (session.board != .other
                                        && session.version.trimmingCharacters(
                                            in: .whitespacesAndNewlines
                                        ).isEmpty)
                            )
                        Button("Find nearby devices") {
                            dismiss()
                            discover()
                        }
                    }
                    if session.verified {
                        Label("First boot verified", systemImage: "checkmark.circle.fill")
                            .foregroundStyle(.green)
                    }
                }.padding(24).frame(maxWidth: 760).frame(maxWidth: .infinity)
            }
        }
        @ViewBuilder private var footer: some View {
            HStack {
                if session.page == 1 {
                    Button("Back") {
                        session.page = 0
                        understood = false
                    }
                }
                if session.page == 3 { Button("Installer output") { session.page = 2 } }
                if session.page == 2 && !session.running && !session.installed {
                    Button("Review setup") {
                        session.page = 1
                        understood = false
                    }
                }
                if session.page > 1 && !session.running {
                    Button("New installation") {
                        session.reset()
                        understood = false
                    }
                }
                Spacer()
                if session.page == 0 {
                    Button("Continue") {
                        session.page = 1
                        understood = false
                    }.buttonStyle(.borderedProminent)
                }
                if session.page == 1 {
                    Button("Open installer") { session.install() }.buttonStyle(.borderedProminent)
                        .disabled(!understood)
                }
                if session.page == 2 && !session.running && session.installed {
                    Button("Boot & verify") { session.page = 3 }.buttonStyle(.borderedProminent)
                }
            }
        }
    }
#endif
