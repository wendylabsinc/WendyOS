import Foundation

struct WendyApp: Codable {
    struct NativeMetadata: Codable, Equatable {
        var directory: String
        var binaryName: String
        var args: [String]
        var currentDirectory: String?
        /// Optional for compatibility with applications saved by older agents.
        var executablePath: String? = nil
        var environment: [String: String]? = nil
    }

    struct ContainerMetadata: Codable, Equatable {
        var imageName: String
        var appConfig: WendyAppConfig?
    }

    var info: WendyAppInfo
    var native: NativeMetadata?
    var container: ContainerMetadata?

    /// Persisted restart policy. Defaults to unless-stopped for `info.json`
    /// files written before this field existed.
    var restartPolicy: PersistedRestartPolicy
    /// Durable "the user asked this app to stop" flag. Set by the
    /// `StopContainer` RPC, cleared by `StartContainer`; agent-shutdown stops
    /// (`stopApp`/`stopAllApps`) must never set it, since a quit/update isn't
    /// user intent to keep the app stopped.
    ///
    /// Decodes as `true` when the key is absent — see `init(from:)`, and do not
    /// "correct" that to `false`.
    var stoppedByUser: Bool

    var process: Foundation.Process?
    var launchToken: UUID?

    /// Non-persisted supervisor bookkeeping (Task 2 consumes these); reset on
    /// every agent restart and on an explicit user start.
    var failureCount: Int = 0
    var lastRestart: Date? = nil
    var lastExitCode: Int32? = nil

    /// The pid `info.json` carried when this app was loaded, if any. `info.pid`
    /// is deliberately scrubbed on load (the agent has no handle on that
    /// process, and reporting it would attribute stats to a possibly-recycled
    /// pid), but reconcile still needs it to recognize a native app that
    /// survived a disorderly agent exit. Set only by `ContainerService.loadApps`
    /// and cleared once reconcile has considered it.
    var persistedPID: Int32? = nil
    /// Kernel process birth time (microseconds since epoch). Unlike a PID or
    /// executable path this also identifies scripts and survives exec().
    var pidBirthTime: UInt64? = nil

    enum CodingKeys: String, CodingKey {
        case info
        case native
        case container
        case restartPolicy
        case stoppedByUser
        case pidBirthTime
    }

    init(
        info: WendyAppInfo,
        native: NativeMetadata? = nil,
        container: ContainerMetadata? = nil,
        restartPolicy: PersistedRestartPolicy = .default,
        stoppedByUser: Bool = false,
        process: Foundation.Process? = nil,
        launchToken: UUID? = nil,
        failureCount: Int = 0,
        lastRestart: Date? = nil,
        lastExitCode: Int32? = nil,
        persistedPID: Int32? = nil,
        pidBirthTime: UInt64? = nil
    ) {
        self.info = info
        self.native = native
        self.container = container
        self.restartPolicy = restartPolicy
        self.stoppedByUser = stoppedByUser
        self.process = process
        self.launchToken = launchToken
        self.failureCount = failureCount
        self.lastRestart = lastRestart
        self.lastExitCode = lastExitCode
        self.persistedPID = persistedPID
        self.pidBirthTime = pidBirthTime
    }

    /// Custom decode so `restartPolicy`/`stoppedByUser` default sensibly when
    /// reading `info.json` files written before those keys existed, and so
    /// the non-persisted runtime fields always start fresh on load.
    /// `encode(to:)` is left to synthesis.
    ///
    /// **A missing `stoppedByUser` decodes as `true`, not `false`.** That looks
    /// backwards, so: the key is absent only in an `info.json` written by an
    /// agent from before restart parity existed, which makes its absence the
    /// legacy marker itself — no marker file needed, since `encode(to:)` always
    /// writes the key. Defaulting it to `false` would make the first reconcile
    /// after an agent update start every app the device has ever deployed,
    /// resurrecting arbitrary old workloads unbidden. Defaulting to `true`
    /// leaves them exactly as the user last saw them; an explicit
    /// `StartContainer` clears the flag, after which the app takes part in
    /// restart parity normally.
    ///
    /// The Linux agent solves this differently (`MigrateStoppedByUserOnce`,
    /// `go/internal/agent/containerd/client.go:3417`): a one-shot pass that
    /// marks every *not currently running* container stopped-by-user, which
    /// works because containerd still holds live task state at upgrade time.
    /// That doesn't transfer — the Mac agent's quit path stops every app and
    /// persists `status: stopped` for all of them before the new binary ever
    /// runs, so by the time this decoder sees the file, "was running" and "was
    /// stopped" are indistinguishable.
    ///
    /// A missing `restartPolicy` still defaults to unless-stopped: it only
    /// decides what happens to an app that is *eligible* to be restarted.
    init(from decoder: any Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        self.info = try values.decode(WendyAppInfo.self, forKey: .info)
        self.native = try values.decodeIfPresent(NativeMetadata.self, forKey: .native)
        self.container = try values.decodeIfPresent(ContainerMetadata.self, forKey: .container)
        self.restartPolicy =
            try values.decodeIfPresent(PersistedRestartPolicy.self, forKey: .restartPolicy)
            ?? .default
        self.stoppedByUser = try values.decodeIfPresent(Bool.self, forKey: .stoppedByUser) ?? true
        self.process = nil
        self.launchToken = nil
        self.failureCount = 0
        self.lastRestart = nil
        self.lastExitCode = nil
        self.persistedPID = nil
        self.pidBirthTime = try values.decodeIfPresent(UInt64.self, forKey: .pidBirthTime)
    }
}
