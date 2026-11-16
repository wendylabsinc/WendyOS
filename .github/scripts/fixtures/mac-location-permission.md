# Mac Location and profile regression gates (2243, 2004, 2189)

Use the existing macOS 15 Swift core row with its Swift 6.2+ compiler. The native
APIs used by this fixture are available on macOS 15; no extra runner or app build
is required. Run:

```sh
python3 .github/scripts/run-owned-media-test.py --timeout 60 -- bash swift/Scripts/TestLocationPermissionRequest.sh
python3 .github/scripts/test-mac-location-permission.py "$RUNNER_TEMP/location-permission"
python3 swift/Scripts/ValidateMacProvisioning_test.py
```

The first command runs the existing helper/source checks (three helper cases
and live-source Observation notifications). The last runs the existing seven
profile tests already registered in swift-tests.yml. Keep them registered once.
The new lifecycle gate compiles the actual WelcomeAndPermissions model and
LocationPermissionRequest with three native facts replaced in a disposable
source copy: usage description, global Location enabled state and app-active
state. A CLLocationManager subclass controls authorization and overrides request
submission, so no real TCC request is invoked. Other permission states are only
read by the model's existing initializer. No signed app is launched.

Normal acceptance includes pending submission/unchanged delegate callbacks,
duplicate request suppression, actual denied callback, Settings-style refresh
recovery, missing usage description/disabled services/restricted guards, the
unchanged sixty-second unanswered timeout/message, and a successful retry with
duplicate resolved callbacks. The early-completion control must fail the
pending-decision assertion. The missing-timeout control must first reach native
submission, then hit the 75-second owned-process deadline without a pass marker.
Each compile is bounded 120 seconds. Typical total runtime is about 150 seconds
including both controls, not including cold compiler startup. Preserve logs and
source hashes; no shortened timer is accepted as original timeout coverage.

The physical generator separates actual OS-dialog results from these controlled
native inputs. Audit2189's current AppDelegate disables the mesh-extension entry
point. Shipped signature/AMFI launch and nonactivation can be tested; extension
loading requires a separately reviewed signed gate-enabled validation build and
cannot be presented as shipped enabled-path acceptance. No trust/TCC changes are
performed by these CI commands.
