# Plan: TCC Inheritance for the Agent Launcher

## Goal
Enable deployed binaries launched by the agent to use the agent's TCC permissions through macOS responsible-process inheritance, matching terminal-style behavior.

## Success Criteria
- The agent is the responsible process for launched binaries.
- Camera, microphone, and Bluetooth access work in child processes when granted to the agent.
- TCC grants persist across agent updates because bundle ID and team ID remain stable.
- The launch path preserves inheritance by using subprocess execution and not using `open` / LaunchServices.

## Implementation Plan

### 1. Stabilize app identity and signing
- Use `sh.wendy.agent.macos` as the permanent code signing identifier for the macOS agent.
- Sign the agent with an **Apple Development** certificate during development.
- Plan for **Developer ID** signing for broader distribution.
- Keep the same code signing identifier and team ID across updates so TCC grants persist.

### 2. Add required entitlements to the agent
- Include TCC-related entitlements in the agent's signed app/binary:
  - `com.apple.security.device.camera`
  - `com.apple.security.device.microphone`
  - `com.apple.security.device.bluetooth`
- Ensure entitlements are embedded in the final signature, not added ad hoc after the fact.

### 3. Add a `wendy-agent setup` onboarding flow
- Add a `wendy-agent setup` subcommand for one-time local setup and permission onboarding.
- Support `setup` on macOS only in v1.
- On unsupported platforms, fail with a clear message that `setup` is currently macOS-only.
- Trigger the necessary TCC prompts from the agent itself during `setup`.
- During `setup`, perform best-effort permission status checks for camera, microphone, and Bluetooth.
- If a permission is already granted, report it as such.
- If a permission is missing, attempt to trigger the corresponding prompt.
- If a permission's status cannot be determined reliably ahead of time, continue with best-effort prompting/reporting instead of failing.
- Keep `setup` safe to re-run so users can recover from partial setup or revoked permissions.
- If permissions remain missing after `setup`, print warnings but still exit successfully.
- Print a final per-permission summary for camera, microphone, and Bluetooth, one line per permission, using `granted`, `missing`, or `unknown`.
- Surface equivalent warnings later when an app is run and required permissions are still missing or unknown, including on the CLI side.
- Document which permissions are required, what `setup` does, and when users should expect prompts.

### 4. Use a single v1 launch policy
- Always launch resolved targets as subprocesses of Wendy in v1.
- For non-`.app` targets, launch with plain `Process()` / normal `fork`-`exec` behavior.
- For `.app` bundles, resolve `Contents/Info.plist`, prefer `CFBundleExecutable`, and exec `Contents/MacOS/<CFBundleExecutable>` directly.
- If `CFBundleExecutable` cannot be resolved, fall back to the bundle directory basename without `.app`.
- If that still does not resolve cleanly, inspect `.app/Contents/MacOS/`; if there is exactly one executable, use it.
- If multiple plausible executables are present in `.app/Contents/MacOS/`, fail with a descriptive error.
- Always preserve inherited TCC from the agent in v1.
- Do not use `responsibility_spawnattrs_setdisclaim` or other private SPI.
- Do not use `open` or LaunchServices, since they break inherited responsibility.
- Defer configurable launch modes or separate-TCC policies to future work.

### 5. Keep deployed app requirements minimal
- Do not require special entitlements in deployed child binaries solely for inherited TCC access.
- Do not rely on the child binary having its own TCC identity unless explicitly designed that way.
- Treat the agent as the single permission-bearing launcher for all launched targets in v1.
- Preserve the current working directory and environment variables when launching child targets.
- Pass `run.args` to both CLI targets and `.app` bundle executables in the same way.

### 6. Verify responsible-process behavior
- Add a validation step to inspect launched child processes with:
  - `sudo launchctl procinfo <pid> | grep responsible`
- Confirm the agent, not `launchd` or another parent, is shown as responsible for both non-`.app` targets and `.app` bundle executables launched as subprocesses.
- Test with binaries that exercise camera, microphone, and Bluetooth access.

### 7. Validate persistence across updates
- Reinstall or update the agent without changing bundle ID or team ID.
- Confirm previously granted permissions still apply after update.
- Verify the expected state in `~/Library/Application Support/com.apple.TCC/TCC.db` if troubleshooting is needed.

## Example CLI and Config

### Setup
```bash
wendy-agent setup
```

### `run` config for a CLI target
```json
{
  "run": {
    "args": ["--verbose"]
  }
}
```

### `run` config for an app bundle
```json
{
  "run": {
    "args": ["--headless"]
  }
}
```

Note: the examples above only illustrate `run.args` usage. They do not propose a new top-level target-path field in `wendy.json`.

## Test Matrix
- `wendy-agent setup` on macOS triggers the expected permission flow from the agent itself.
- `wendy-agent setup` performs best-effort status checks before prompting.
- `wendy-agent setup` requests only currently missing permissions when status is knowable.
- `wendy-agent setup` tolerates permissions whose status cannot be determined reliably ahead of time.
- `wendy-agent setup` is safe to re-run after partial setup or permission revocation.
- `wendy-agent setup` exits 0 with warnings when permissions remain missing.
- `wendy-agent setup` prints a final one-line-per-permission summary for camera, microphone, and Bluetooth.
- `wendy-agent setup` on unsupported platforms fails with a clear macOS-only message.
- Non-`.app` target launches as a subprocess of Wendy.
- `.app` target launches its inner executable directly as a subprocess of Wendy.
- `.app` target falls back to the bundle-name heuristic if `CFBundleExecutable` cannot be resolved.
- `.app` target falls back to the single executable found in `.app/Contents/MacOS/` if the first two resolution paths fail.
- `.app` target fails with a descriptive error when `.app/Contents/MacOS/` contains multiple plausible executables.
- Child launched as a subprocess is responsible-process-linked to the agent.
- Child launched after agent upgrade with same bundle ID/team ID.
- Child launched when agent permissions were granted interactively.
- Runtime warnings are driven only by `camera`, `audio`, and `bluetooth` entitlements.
- Runtime warnings map `audio` to microphone permission checks.
- Runtime warnings are computed locally by the CLI and shown before launch.
- Runtime warnings use one warning per permission and cover both `missing` and `unknown` states.
- Runtime warnings recommend running `wendy-agent setup`.
- Warning propagation test: setup warnings are printed by the agent; runtime warnings are computed and shown by the CLI.
- Negative test: a target launched via `open` / LaunchServices should not be treated as an inherited-TCC path.

## Risks / Open Questions
- Whether every target permission behaves identically under inheritance in the exact deployment format used by the agent.
- Whether any child binaries are accidentally launched through wrappers that re-parent under `launchd`.
- Whether packaging or signing workflows might unintentionally change bundle ID, team ID, or entitlements between releases.

## Future Work
- For managed environments, support an MDM profile using `com.apple.TCC.configuration-profile-policy` to pre-grant access.
- Add validation coverage for MDM-granted permissions once managed deployment is in scope.
- Consider configurable launch modes or separate-TCC policies if v1 proves too restrictive.
- Add `setup` support to the Go-based `wendy-agent`.

## Deliverables
- Signed standalone macOS `wendy-agent` build with stable identity `sh.wendy.agent.macos` and required entitlements.
- `wendy-agent setup` subcommand for local onboarding and permission prompting.
- Launcher implementation that always uses subprocess launch and inherited TCC in v1, for both CLI targets and app bundles.
- CLI-side runtime permission warning logic based on `camera`, `audio`, and `bluetooth` entitlements.
- Validation notes showing responsible-process inheritance and warning propagation.
