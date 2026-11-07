# Wendy CLI / Agent gaps and mobile integration checklist

Updated: 2026-11-07

Working checklist from mobile development. These findings concern the APIs and
installed agent versions we inspected; recheck current support before starting
each item. An unfinished mobile flow is not automatically a missing Agent API.
Check an item only after its acceptance criteria are verified.

Corrected using the user’s review of current WendyOS main on 2026-11-07.
The corrections below are supplied code-review findings, not an independent
reverification of main in this documentation update. They do not identify new bugs.

## Recommended order

### 1. Camera source identity
- [ ] Fix and verify camera identity end to end.

**Type:** Confirmed Agent bug. Fix prepared in [draft PR #2727](https://github.com/wendylabsinc/WendyOS/pull/2727); not deployed at the time of this checklist.

**Problem:** Persisted virtual-camera names can disagree with reassigned source
identities. A camera displayed under one name can stream another source, and
numeric campaign selectors can target a different camera after reassignment.

**Done when:** Camera names, stable identities, previews, and campaign targets
agree across reconnects, Agent restarts, and unpair/re-pair. Missing sources fail
clearly rather than silently switching to another camera. Verify on hardware
after reviewing and deploying the fix. Review legacy numeric campaign selectors.

### 2. Fast episode metadata and filtering
- [ ] Provide complete episode kind/label summaries without per-episode inspection.

**Type:** Recording-index/API limitation.

**Problem:** Complete labels and kinds require inspecting individual episodes.
First-load filters and counts such as “Person” stay incomplete while details load.
The episode list has no labels or kinds; only in-progress recordings identify
the campaign that started them. The mobile memory cache helps repeat visits,
but does not solve the first load.

**Done when:** The index or a summary API supplies reliable kinds, labels and
counts; filtered results and exports remain correct as episodes change. Measure
first-load request count and latency on a large library. Preserve explicit
unknown/partial states for older agents or unavailable metadata.

### 3. Stop a specific campaign recording
- [ ] Add or expose targeted campaign-clip stop support.

**Type:** Missing targeted-stop API.

**Existing behavior:** CampaignTrigger creates a timed clip. DataService.Stop
stops a manual recording if one is running. When no manual recording is running
and exactly one campaign clip is active, Stop can end that clip. It refuses when
multiple campaign clips are active and cannot select a particular clip.

**Gap:** The caller cannot explicitly target the intended campaign clip. Home
currently offers timed recording with automatic completion. Its explanatory copy
must distinguish this app behavior from the Agent’s single-clip stop capability.
- [ ] Correct any mobile copy that claims campaign clips can never stop early.

**Done when:** A caller can identify and stop its intended campaign recording,
read its final state, and leave unrelated recordings untouched. Verify concurrent
campaigns, repeated stop requests, and reconnects.

### 4. Camera configuration
- [ ] Expose supported camera modes and validate resolution selection.
- [ ] Define supported remote-camera controls separately from local V4L2 controls.

**Type:** Camera API capability gaps.

**Problem:** The Agent reports pixel formats but does not enumerate valid
width/height/frame-rate combinations. Streaming accepts requested width, height,
and frame rate; network cameras ignore height and frame rate. Local V4L2 controls
on a SensorLink virtual node do not necessarily configure the remote physical camera.

**Done when:** The app offers only supported modes/controls for the selected stable
camera identity, verifies changes by readback, and explains unsupported controls.
Test local cameras and SensorLink cameras separately.

### 5. Device management
- [ ] Agent API: add standalone device reboot / Agent restart support.
- [ ] Mobile integration: use UpdateOS and GetOSUpdateStatus for OS updates.
- [ ] Mobile integration: use Unprovision with local_only = true for local reset.

**Type:** One missing Agent API plus two mobile integration tasks.

**Existing support:** UpdateOS and GetOSUpdateStatus already implement OS updates;
status reports whether an update committed or rolled back. Unprovision with
local_only = true already supports local enrollment reset. Agent update also
exists. Cloud removal is not a local device reset.

**Gap:** No reboot or Agent-restart call exists; restart currently uses the device
shell. OS update and local reset need complete mobile flows using the existing
APIs, not new backend implementations.

**Done when:** Each action has a supported backend path, clear consequences,
appropriate confirmation, observable progress/recovery, and post-action
verification. OS updates need preflight, reboot/reconnect, version verification,
and a defined failure/rollback path. Reset must accurately describe what it erases
or revokes and verify the resulting enrollment state.

### 6. Wi-Fi changes after enrollment
- [ ] Mobile integration: use authenticated Wi-Fi management after enrollment.

**Type:** App work; existing Agent capability.

**Existing support:** The authenticated Wi-Fi service supports ConnectToWiFi,
forget, and priority operations on enrolled devices through the normal secure
connection, including Cloud access.

**Gap:** The mobile flow must use that service. Bluetooth deliberately rejects
post-enrollment Wi-Fi changes because it is a first-time setup path; this is not
a missing Agent capability or a new bug.

**Done when:** An authorized owner can change Wi-Fi after enrollment through a
supported management path, with credential protection, clear connection feedback,
and recovery if the new network cannot be joined. Verify on real hardware.

### 7. Mesh signal strength
- [ ] Provide signal-strength data required by the proposed connectivity view.

**Type:** Missing telemetry for the proposed UI.

**Problem:** The inspected mesh API does not provide the dBm measurements needed
for that view. Route cost must not be presented as radio signal strength.

**Done when:** Supported links expose measured signal strength with units,
freshness, and unavailable states. The UI distinguishes routing metrics from
radio measurements and does not invent phone-to-device links.

## Tracking each fix

For each item, record the owner, implementation PR, supported Agent/CLI versions,
test evidence, and deployment status here. Keep “implemented,” “tested,” and
“deployed” separate; a passing fixture test alone does not verify physical hardware.
