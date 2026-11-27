# Native World integration

This local package is linked into WendyDesktop. `WorldClient` contains the typed
gateway SDK, `WorldOperator` contains the profile-based operations workspace, and
`WorldTwin` contains the native SceneKit simulation/session and review views.
The existing Sites digital twin and device/Go2 controls remain separate app routes.

Open **World** in the Wendy sidebar. Operations imports an explicit profile and
shows observations, missions, human notes, and areas. Digital twin sessions opens
an existing viewer folder, camera profile, sensor-fusion session, retained patrol
report, or prepared schedule/inspection/offline-kit review. It does not start a
simulator or choose a physical device. Chat World cards only navigate to a section.

The app accepts these explicit launch inputs:

```sh
WendyDesktop --world-profile /absolute/native-profile.json
WendyDesktop --world-session /absolute/viewer-directory
WendyDesktop --world-session /absolute/viewer-directory --world-replay
WendyDesktop --world-timeline /absolute/timeline \
  --world-render-output /absolute/fresh-output \
  --world-render-duration 12 --world-render-fps 12
```

`--profile` and `--session` remain aliases. The original camera, gas, incident,
patrol debrief, schedule, inspection, and offline-kit launch flags are parsed by
`WorldTwinLaunchRequest`. Review launchers must wait for their output JSON, not
for the Wendy process to exit. Closing an unfinished review writes
`{"action":"cancel"}` without replacing an approval. Mission/evidence profiles
are optional explicit inputs. Replay drops authority profiles, disables commands,
and does not write readiness, stop, or QA files into the retained run.

Timeline export renders recorded states through the native model, SceneKit
renderer and SwiftUI interface. It writes `frames/%06d.png` and an atomic
`render.json` completion receipt. Output must be outside the input timeline and
must not already contain frames or a completion receipt. Bounds are 60 seconds,
24 fps, and 3,600 input states. The app stays open; export is cancelled when its
view leaves the screen.

Failed or cancelled exports write a bounded, atomic `render-error.json` receipt.
The original `WorldClientProbe` and `ZoneClientProbe` executable targets remain
available for HTTP and SDK acceptance. DEBUG operator launches retain the bounded
annotation, mission, amendment, proposal-inbox and area model-test stages when an
explicit profile is supplied. Mobile/visionOS standalone app hosts are not part of
this macOS integration; their runtime checks require an explicitly provided host.
The cross-platform SDK remains available, but this change does not integrate
World screens into the Wendy mobile app or claim mobile UI acceptance.

Run package tests with `swift test --package-path go/macos/WorldClient` from the
repository root. Optional native replay checks accept
`WENDY_WORLD_REPLAY_INPUT` or `WENDY_WORLD_TIMELINE_INPUT`, and write private
review output under `WENDY_WORLD_REVIEW_OUTPUT`. No runtime images or profiles
belong in Git. Full app compilation uses the WendyDesktop Xcode scheme.

Validation covers the SDK, lifecycle/cancellation, launch routing, readonly
commands, review receipts, original localization/fire/patrol checks, and native
rendering of retained simulation data. The integrated WendyDesktop executable
completed a 48-second export with 576 native frames at 12 fps, encoded into a
1080p, 24 fps MP4. Validation checked the completion receipt, original timeline
and executable hashes, and representative rendered frames. Export stays selected
independently of the restored Operations picker state; cancellation and failure
produce explicit receipts. Review media remains outside Git.

The real two-node Go/Swift investigation acceptance also passed all 15 retention
and fault checks. Full interactive gateway, sensor, review and physical robot
workflows remain unverified. Native rendering and SDK acceptance do not establish
that those live UI workflows succeeded.
