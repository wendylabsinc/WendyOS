import Foundation
import Network
@preconcurrency import NetworkExtension
import WendyAgentCore
import os

private let providerLog = Logger(
    subsystem: "sh.wendy.WendyAgentMac.WendyNet",
    category: "ProxyProvider"
)

/// Transparent proxy that intercepts TCP and UDP flows to the mesh VIP range.
/// The packet-tunnel profile's scoped resolver sends Wendy DNS queries to a
/// VIP in that range, so the same flow path handles DNS without public DNS.
final class WendyNetProxyProvider: NETransparentProxyProvider, NEAppProxyUDPFlowHandling,
    @unchecked Sendable
{
    private var extensionConfig: ExtensionConfig?
    private let datagramSessions = DatagramSessionCache()

    override func startProxy(
        options: [String: Any]?,
        completionHandler: @escaping ((any Error)?) -> Void
    ) {
        providerLog.notice("starting transparent proxy")
        guard
            let config = ExtensionConfig.load(
                providerConfiguration: (protocolConfiguration as? NETunnelProviderProtocol)?
                    .providerConfiguration,
                options: options
            )
        else {
            let error = NSError(
                domain: "sh.wendy.WendyAgentMac.WendyNet",
                code: 1,
                userInfo: [NSLocalizedDescriptionKey: "WendyNet configuration is unavailable"]
            )
            providerLog.error("failed to load provider configuration")
            completionHandler(error)
            return
        }
        extensionConfig = config

        let settings = NETransparentProxyNetworkSettings(tunnelRemoteAddress: "127.0.0.1")

        // Intercept all TCP to the mesh CIDR.
        let meshEndpoint = NWEndpoint.hostPort(host: "10.99.0.0", port: .any)
        let meshRule = NENetworkRule(
            remoteNetworkEndpoint: meshEndpoint,
            remotePrefix: 16,
            localNetworkEndpoint: nil,
            localPrefix: 0,
            protocol: .TCP,
            direction: .outbound
        )

        // Intercept all UDP to the mesh CIDR (video streams etc.).
        let meshUDPRule = NENetworkRule(
            remoteNetworkEndpoint: meshEndpoint,
            remotePrefix: 16,
            localNetworkEndpoint: nil,
            localPrefix: 0,
            protocol: .UDP,
            direction: .outbound
        )

        settings.includedNetworkRules = [meshRule, meshUDPRule]

        setTunnelNetworkSettings(settings) { error in
            if let error {
                providerLog.error(
                    "failed to apply network settings: \(error.localizedDescription, privacy: .public)"
                )
            } else {
                providerLog.notice("network settings active")
            }
            completionHandler(error)
        }
    }

    override func stopProxy(
        with reason: NEProviderStopReason,
        completionHandler: @escaping () -> Void
    ) {
        providerLog.notice("stopping transparent proxy reason=\(reason.rawValue)")
        extensionConfig = nil
        completionHandler()
    }

    override func handleNewFlow(_ flow: NEAppProxyFlow) -> Bool {
        guard let config = extensionConfig else {
            providerLog.error("cannot handle flow: provider configuration unavailable")
            return false
        }

        if let udp = flow as? NEAppProxyUDPFlow {
            return handleUDPFlow(udp, config: config)
        }

        guard let tcp = flow as? NEAppProxyTCPFlow,
            case .hostPort(let endpointHost, let endpointPort) = tcp.remoteFlowEndpoint
        else {
            return false
        }

        if endpointPort.rawValue == 53 {
            providerLog.debug("handling DNS TCP flow")
            tcp.open(withLocalFlowEndpoint: nil) { [weak self] error in
                guard error == nil, let self else {
                    tcp.closeReadWithError(error)
                    tcp.closeWriteWithError(error)
                    return
                }
                self.pumpTCPDNS(tcp, config: config, buffer: Data())
            }
            return true
        }

        guard let assetID = WendyMeshAddressPlan.deviceID(for: endpointHost.debugDescription) else {
            return false
        }
        let port = Int(endpointPort.rawValue)

        tcp.open(withLocalFlowEndpoint: nil) { [weak self] error in
            guard error == nil, let self else {
                tcp.closeReadWithError(error)
                tcp.closeWriteWithError(error)
                return
            }
            Task { await self.openTunnel(assetID: assetID, port: port, flow: tcp, config: config) }
        }
        return true
    }

    func handleNewUDPFlow(
        _ flow: NEAppProxyUDPFlow,
        initialRemoteFlowEndpoint remoteEndpoint: Network.NWEndpoint
    ) -> Bool {
        guard let config = extensionConfig else {
            providerLog.error("cannot handle UDP flow: provider configuration unavailable")
            return false
        }
        return handleUDPFlow(flow, config: config)
    }

    /// Routes each datagram by destination: mesh-DNS queries are answered
    /// locally; datagrams to a mesh VIP relay over the device's datagram
    /// session; anything else is dropped. One NEAppProxyUDPFlow can carry
    /// datagrams to multiple endpoints, so routing is per-datagram, with a
    /// per-(endpoint) flow-id map scoped to this NE flow.
    private func handleUDPFlow(_ udp: NEAppProxyUDPFlow, config: ExtensionConfig) -> Bool {
        udp.open(withLocalFlowEndpoint: nil) { [weak self] error in
            guard error == nil, let self else {
                udp.closeReadWithError(error)
                udp.closeWriteWithError(error)
                return
            }
            let flowMap = UDPFlowMap()
            let writeQueue = UDPWriteQueue(flow: udp)
            self.pumpUDP(udp, config: config, flowMap: flowMap, writeQueue: writeQueue)
        }
        return true
    }

    /// The single insertion point for LAN-direct (follow-up plan). Today: broker only.
    private func openTunnel(
        assetID: Int32,
        port: Int,
        flow: NEAppProxyTCPFlow,
        config: ExtensionConfig
    ) async {
        providerLog.notice("opening relay asset=\(assetID) port=\(port)")
        do {
            try await WendyCloudTunnel.relay(
                assetID: assetID,
                host: "localhost",
                port: port,
                cloudGRPC: config.cloudGRPC,
                credentials: config.credentials,
                readFlow: FlowByteBridge.read(flow),
                writeFlow: FlowByteBridge.write(flow)
            )
            providerLog.notice("relay completed asset=\(assetID) port=\(port)")
            flow.closeReadWithError(nil)
            flow.closeWriteWithError(nil)
        } catch {
            providerLog.error(
                "relay failed asset=\(assetID) port=\(port) error=\(error.localizedDescription, privacy: .public)"
            )
            flow.closeReadWithError(error)
            flow.closeWriteWithError(error)
        }
    }

    /// Answers `device-<N>.mesh.wendy.internal` A queries from the enrolled-device directory
    /// snapshot locally (byte-for-byte the old `pumpDNS` behavior — everything else the
    /// resolver can't answer becomes NXDOMAIN per `MeshDNS.answer`'s contract: unknown
    /// `device-<N>` names resolve to `nil` here); datagrams addressed to a mesh VIP relay
    /// over that device's `CloudDatagramSession`; anything else is dropped.
    ///
    /// Every outbound write on `udp` — DNS replies here and relayed replies in
    /// `relayUDPDatagram`'s handler — goes through `writeQueue` rather than calling
    /// `udp.writeDatagrams` directly, so this loop no longer needs to wait for a write's
    /// completion before reading again (the old DNS-only recursion-on-completion): it can
    /// always re-read immediately, same as it already did on every non-DNS batch.
    private func pumpUDP(
        _ udp: NEAppProxyUDPFlow,
        config: ExtensionConfig,
        flowMap: UDPFlowMap,
        writeQueue: UDPWriteQueue
    ) {
        udp.readDatagrams { [weak self] datagrams, error in
            guard let self, error == nil, let datagrams else {
                udp.closeReadWithError(error)
                udp.closeWriteWithError(error)
                // The local flow is gone: unregister its handlers now rather than waiting for
                // the 60s idle sweep to notice.
                Task { await flowMap.closeAll() }
                return
            }
            for (payload, endpoint) in datagrams {
                guard case .hostPort(let host, let port) = endpoint else { continue }
                if port.rawValue == 53 {
                    if let resp = WendyMeshDNS.answer(
                        payload,
                        resolve: { id in
                            // Only answer for devices actually in this org's directory.
                            guard config.directory.devices.contains(where: { $0.assetID == id })
                            else { return nil }
                            return WendyMeshAddressPlan.address(for: id)
                        }
                    ) {
                        Task { await writeQueue.enqueue(resp, endpoint: endpoint) }
                    }
                    continue  // non-mesh name: drop (resolver only matches the mesh DNS suffixes)
                }
                guard let assetID = WendyMeshAddressPlan.deviceID(for: "\(host)") else { continue }
                Task {
                    await self.relayUDPDatagram(
                        payload,
                        assetID: assetID,
                        port: UInt32(port.rawValue),
                        endpoint: endpoint,
                        udp: udp,
                        config: config,
                        flowMap: flowMap,
                        writeQueue: writeQueue
                    )
                }
            }
            self.pumpUDP(udp, config: config, flowMap: flowMap, writeQueue: writeQueue)  // keep reading
        }
    }

    /// Relays one datagram over the destination device's `CloudDatagramSession`, opening the
    /// session lazily and registering the return-path handler before the first send for a
    /// given (endpoint) so no reply can race the handler registration. `flowMap.id(for:)` also
    /// records the activity timestamp used for 60s idle expiry (see `UDPFlowMap`); on open
    /// failure the session cache evicts its memoized attempt so the next datagram retries.
    ///
    /// The handler funnels every write through `writeQueue` instead of calling
    /// `udp.writeDatagrams` itself: `NEAppProxyUDPFlow` allows only one outstanding write per
    /// flow, but one NE flow can be shared by several devices' `CloudDatagramSession`s (each
    /// its own actor, each free to deliver inbound datagrams whenever its own frame arrives),
    /// so nothing here otherwise prevents two of those deliveries from overlapping. A sustained
    /// inbound stream (the spec's video use case, §5a) makes that overlap likely, not just
    /// theoretical.
    private func relayUDPDatagram(
        _ payload: Data,
        assetID: Int32,
        port: UInt32,
        endpoint: Network.NWEndpoint,
        udp: NEAppProxyUDPFlow,
        config: ExtensionConfig,
        flowMap: UDPFlowMap,
        writeQueue: UDPWriteQueue
    ) async {
        do {
            let session = try await datagramSessions.session(for: assetID, config: config)
            let (flowID, isNew) = await flowMap.id(
                for: endpoint,
                assetID: assetID,
                session: session,
                cache: datagramSessions
            )
            if isNew {
                await session.setDatagramHandler(flowID: flowID) { data in
                    Task { await writeQueue.enqueue(data, endpoint: endpoint) }
                    Task { await flowMap.touch(endpoint: endpoint) }
                }
            }
            await session.sendDatagram(flowID: flowID, port: port, payload: payload)
        } catch {
            providerLog.error(
                "udp relay failed asset=\(assetID) error=\(error.localizedDescription, privacy: .public)"
            )
        }
    }

    private func pumpTCPDNS(_ tcp: NEAppProxyTCPFlow, config: ExtensionConfig, buffer: Data) {
        tcp.readData { [weak self] data, error in
            guard let self, error == nil, let data, !data.isEmpty else {
                tcp.closeReadWithError(error)
                tcp.closeWriteWithError(error)
                return
            }

            var pending = buffer
            pending.append(data)
            let queries = WendyMeshDNS.extractTCPMessages(from: &pending)
            let responses = queries.compactMap { query in
                WendyMeshDNS.answer(
                    query,
                    resolve: { id in
                        guard config.directory.devices.contains(where: { $0.assetID == id }) else {
                            return nil
                        }
                        return WendyMeshAddressPlan.address(for: id)
                    }
                )
            }

            guard !responses.isEmpty else {
                self.pumpTCPDNS(tcp, config: config, buffer: pending)
                return
            }
            let framed = responses.reduce(into: Data()) {
                $0.append(WendyMeshDNS.frameForTCP($1))
            }
            let remaining = pending
            tcp.write(framed) { writeError in
                guard writeError == nil else {
                    tcp.closeReadWithError(writeError)
                    tcp.closeWriteWithError(writeError)
                    return
                }
                self.pumpTCPDNS(tcp, config: config, buffer: remaining)
            }
        }
    }
}

/// Maps remote endpoints seen on one NE UDP flow to session flow IDs, and expires idle entries
/// (unregistering their datagram handler) so a long-lived `CloudDatagramSession` doesn't
/// accumulate handler closures — each one strongly capturing an `NEAppProxyUDPFlow` — forever.
/// Scoped to a single `NEAppProxyUDPFlow`: one flow can carry datagrams to multiple mesh
/// endpoints (e.g. several devices, or several ports on one device), each needing its own flow
/// ID for the return path.
actor UDPFlowMap {
    private static let idleTimeout: Duration = .seconds(60)
    private static let sweepInterval: Duration = .seconds(15)

    private struct Entry {
        let flowID: UInt32
        let session: WendyCloudDatagramSession
        var lastActivity: ContinuousClock.Instant
    }

    private var entries: [String: Entry] = [:]
    private var sweepTask: Task<Void, Never>?

    /// Returns the flow ID for `endpoint`, allocating one via the cache's synchronous counter
    /// if this is the first datagram to it — or if the entry we have is bound to a session
    /// that is no longer the live one for this asset (see below). Allocation happens with no
    /// suspension between the membership check and recording the new ID (`cache.nextFlowID()`
    /// is `nonisolated` and synchronous — see its doc comment), so concurrent calls for a
    /// brand-new endpoint can't each allocate their own ID and orphan a handler registration.
    /// Also records `session` so idle-expiry and flow-close cleanup can unregister its handler
    /// later, and refreshes the activity timestamp on every call (this is the "touch on
    /// outbound datagram" side of the 60s idle expiry; the "touch on inbound delivery" side is
    /// `touch(endpoint:)`, called from the datagram handler itself).
    ///
    /// Spec §6 requires client edges to tear down flow tables on datagram-session death and
    /// lazily re-open. `DatagramSessionCache` already drops its memoized session on close, so
    /// the *next* call to `cache.session(for:)` returns a brand-new `CloudDatagramSession`
    /// instance — but a warm entry here, keyed only by endpoint, would otherwise keep handing
    /// out its old flow ID and never re-register a handler on that new session (up to 60s of
    /// one-way UDP after a broker restart). Comparing `session` by identity against what's
    /// already recorded catches exactly that case and takes the fresh-flow path instead, which
    /// re-registers a handler on the session that's actually live.
    func id(
        for endpoint: Network.NWEndpoint,
        assetID: Int32,
        session: WendyCloudDatagramSession,
        cache: DatagramSessionCache
    ) -> (UInt32, Bool) {
        let key = "\(endpoint)"
        let now = ContinuousClock.now
        ensureSweeping()
        if var existing = entries[key], existing.session === session {
            existing.lastActivity = now
            entries[key] = existing
            return (existing.flowID, false)
        }
        let id = cache.nextFlowID()
        entries[key] = Entry(flowID: id, session: session, lastActivity: now)
        return (id, true)
    }

    /// Refreshes the activity timestamp for `endpoint` when a reply is delivered through its
    /// handler, so a flow that's actively receiving data isn't expired out from under it.
    func touch(endpoint: Network.NWEndpoint) {
        let key = "\(endpoint)"
        guard var entry = entries[key] else { return }
        entry.lastActivity = ContinuousClock.now
        entries[key] = entry
    }

    /// Unregisters every still-live handler immediately and stops sweeping. Called when the
    /// owning NE flow's read loop terminates (error or close) so a closed local flow releases
    /// its session-side handlers right away instead of waiting for the next idle sweep.
    ///
    /// Clears `entries` synchronously — *before* awaiting any unregistration — by taking the old
    /// map by value. A `relayUDPDatagram` task racing this teardown (already past the point where
    /// it fetched a `CloudDatagramSession` and about to call `id(for:)`) will therefore see an
    /// empty map once it runs: it takes the fresh-flow path, which calls `ensureSweeping()` again
    /// and restarts a sweep task, so that late entry is still bounded by the normal 60s idle
    /// expiry rather than surviving unregistered forever. The alternative — snapshot-then-await-
    /// then-removeAll — has a lost-update window: a for-in loop over `entries` captures its state
    /// at the start, so a racing insert during one of the loop's awaits would never be observed
    /// by the loop, yet `removeAll()` afterwards would still wipe that entry's bookkeeping without
    /// ever unregistering its handler — leaking it on the session for good.
    func closeAll() async {
        sweepTask?.cancel()
        sweepTask = nil
        let snapshot = entries
        entries = [:]
        for (_, entry) in snapshot {
            await entry.session.setDatagramHandler(flowID: entry.flowID, nil)
        }
    }

    private func ensureSweeping() {
        guard sweepTask == nil else { return }
        sweepTask = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: Self.sweepInterval)
                guard !Task.isCancelled, let self else { return }
                await self.sweep()
            }
        }
    }

    /// Expires idle entries. Removes each stale entry from `entries` synchronously (no `await`
    /// in between) *before* unregistering its handler — the mirror image of the reasoning in
    /// `closeAll()`'s doc comment. If a datagram for the same endpoint races in between, `id
    /// (for:)` sees a miss (not a stale-but-still-present entry it would otherwise refresh and
    /// reuse without a handler registration), so it takes the fresh-flow path — allocating a new
    /// ID and registering a fresh handler — rather than silently sending on an entry that's about
    /// to have its handler torn down. A reply arriving in the brief gap between removal here and
    /// this method's `setDatagramHandler(nil)` call is simply dropped, which is ordinary,
    /// acceptable UDP loss.
    private func sweep() async {
        let now = ContinuousClock.now
        let staleKeys = entries.filter { now - $0.value.lastActivity >= Self.idleTimeout }.map(
            \.key
        )
        guard !staleKeys.isEmpty else { return }
        let removed = staleKeys.compactMap { entries.removeValue(forKey: $0) }
        for entry in removed {
            await entry.session.setDatagramHandler(flowID: entry.flowID, nil)
        }
    }
}

/// Serializes every `writeDatagrams` call on one `NEAppProxyUDPFlow`, honoring
/// NetworkExtension's one-outstanding-write-per-flow contract for that flow.
///
/// A single NE UDP flow can be shared by several devices' `CloudDatagramSession`s (one
/// per mesh VIP it has talked to — see `UDPFlowMap`), each an independent actor free to
/// call its datagram handler whenever its own inbound frame arrives, plus this file's own
/// DNS-answer path. Nothing about that shape limits how many of those deliveries land at
/// the same instant, and a sustained inbound stream (the spec's video use case, §5a) makes
/// concurrent deliveries likely rather than a rare edge case. Calling `writeDatagrams`
/// without waiting on its completion handler under that concurrency violates the flow's
/// contract; this actor is the single point every writer now goes through instead.
///
/// Only one write is ever in flight: `enqueue` either starts writing immediately (nothing
/// in flight) or appends to `pending`, and the completion handler drains the next queued
/// entry before signaling done. `pending` is capped at `maxBacklog`; once full, the oldest
/// queued datagram is dropped to make room for the newest, on the same reasoning as the
/// rest of this design — plain UDP loss under sustained overload is acceptable, unbounded
/// memory growth from a producer that outpaces NE's write completions is not.
actor UDPWriteQueue {
    private static let maxBacklog = 256

    private let flow: NEAppProxyUDPFlow
    private var pending: [(Data, Network.NWEndpoint)] = []
    private var writing = false

    init(flow: NEAppProxyUDPFlow) {
        self.flow = flow
    }

    func enqueue(_ data: Data, endpoint: Network.NWEndpoint) {
        guard !writing else {
            if pending.count >= Self.maxBacklog {
                pending.removeFirst()
            }
            pending.append((data, endpoint))
            return
        }
        writing = true
        write(data, endpoint)
    }

    private func write(_ data: Data, _ endpoint: Network.NWEndpoint) {
        flow.writeDatagrams([(data, endpoint)]) { [self] _ in
            Task { await self.writeCompleted() }
        }
    }

    private func writeCompleted() {
        guard !pending.isEmpty else {
            writing = false
            return
        }
        let (data, endpoint) = pending.removeFirst()
        write(data, endpoint)
    }
}
