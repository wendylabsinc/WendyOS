//
//  DeviceDetailsPage.swift
//  Companion
//
//  Created by Maximilian Alexander on 7/20/25.
//

import OSLog
import SwiftUI

struct DeviceDetailsPage: View {
    private let logger = Logger(subsystem: "sh.wendy.companion", category: "DeviceDetails")
    let device: Device
    @State private var ssid = ""
    @State private var password = ""
    @State private var isHiddenNetwork = false
    @State private var securityType = "WPA2"

    var body: some View {
        Form {
            Section("Info") {
                HStack {
                    Text("Device ID")
                        .foregroundColor(.secondary)
                    Spacer()
                    Text(device.id)
                        .font(.system(.body, design: .monospaced))
                }

                HStack {
                    Text("Device Name")
                        .foregroundColor(.secondary)
                    Spacer()
                    Text(device.name)
                }
            }

            Section("Wi-Fi") {
                TextField("SSID", text: $ssid)
                    #if os(iOS)
                        .textInputAutocapitalization(.never)
                    #endif
                    .autocorrectionDisabled()

                SecureField("Password", text: $password)
                    #if os(iOS)
                        .textInputAutocapitalization(.never)
                    #endif
                    .autocorrectionDisabled()

                Picker("Security", selection: $securityType) {
                    Text("None").tag("None")
                    Text("WEP").tag("WEP")
                    Text("WPA").tag("WPA")
                    Text("WPA2").tag("WPA2")
                    Text("WPA3").tag("WPA3")
                }

                Toggle("Hidden Network", isOn: $isHiddenNetwork)

                Button(action: saveChanges) {
                    Text("Save Changes")
                        .frame(maxWidth: .infinity)
                }
                #if os(macOS)
                    .controlSize(.large)
                #endif
            }
        }
        #if os(macOS)
            .formStyle(.grouped)
            .padding()
            .navigationTitle(device.name)
        #else
            .navigationTitle(device.name)
            .navigationBarTitleDisplayMode(.inline)
        #endif
    }

    private func saveChanges() {
        // TODO: Implement save logic
        logger.debug("Saving Wi-Fi settings for \(device.name)")
        logger.debug("SSID: \(ssid)")
        logger.debug("Security: \(securityType)")
        logger.debug("Hidden: \(isHiddenNetwork)")
    }
}

#Preview {
    NavigationStack {
        DeviceDetailsPage(device: Device(id: "1", name: "NVIDIA Jetson Orin Nano Alpha"))
    }
}
