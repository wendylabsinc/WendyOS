# Handoff: Wendy iOS app — lenses, device management, Wendy chat

## Overview
A redesign of the Wendy iPhone app around three **lenses** — Devices (IoT developer), Robots (robotics engineer), Sites (site operator) — sharing one data model (Org › Places › Devices/Robots › Apps, Missions, Sims, Data, Notifications) and one persistent **Wendy chat** assistant that can answer, configure and run things from any screen.

Targets (both must be updated; keep shared logic in one place):
- **Companion app** — `wendy-companion-ios/WendyCompanionApple` (SwiftUI)
- **Shared app** — `wendylabsinc/WendyOS` › `go/macos` (SwiftUI, `Sources/iOS` + `Sources/App`)

## About the design files
The `.dc.html` files are **design references built in HTML** — interactive prototypes of look and behavior, not code to ship. Recreate them natively in **SwiftUI** using each codebase's existing patterns (navigation stacks, `@Observable` models, existing gRPC/agent clients, existing Bluetooth provisioning, SceneKit site twin). Open `WendyOS iPhone v6 Companion.dc.html` in a browser (serve the folder: `npx serve .`) — every tap works and mock data is in the logic class at the bottom of the file.

## Fidelity
**High-fidelity** for layout, hierarchy, copy, colors and interactions. Use native iOS controls (NavigationStack, TabView, segmented Picker, sheets, `.glassEffect`/materials) — the HTML imitates iOS 26 "liquid glass"; prefer the real system components over pixel-copying.

---

## App structure

```
Launch ─▶ Onboarding (first run only: "What do you mainly work on?")
           └─▶ General Home (lens tiles)
                 ├─▶ Devices lens   tabs: Home · Devices · Data · Activity
                 ├─▶ Robots lens    tabs: Home · Robots · Missions · Sim · Data
                 └─▶ Sites lens     tabs: Home · Places · Missions · Alerts
Persistent on every screen: top bar (back / org pill · bell) + Wendy chat bubble
```

- **Lens** = a filtered view onto the same org data, with its own bottom tab bar. The user's **primary lens** is chosen at onboarding and changeable in **Settings › Primary lens**. Primary lens renders as the large tile on General Home; the other two are small tiles (and can be hidden via "Edit Home").
- Each `lens:tab` keeps its **own navigation stack** (switching tabs/lenses preserves position).
- Back from a lens root returns to General Home.
- Cross-lens jumps: a robot's onboard computer opens in the Devices lens; a device's place opens in Sites (`openIn(lens, id)` in the prototype).

## Screens

### 1. Onboarding (first launch)
- Wendy app icon (`assets/AppIcon.svg`, 64pt, 15pt corner radius), title "What do you mainly work on?", 3 option cards: Devices / Robots / Sites with one-line descriptions. Continue button (brand green, full width, 50pt, capsule).
- Sets `primaryLens` (persist in `@AppStorage`). Skippable later via Settings.

### 2. General Home
- Large title "Good morning" + date subtitle (`EEEE, MMMM d`).
- **Primary lens tile** (380pt tall, 30pt radius, white→lens-tint vertical gradient, faint lens illustration in background: mesh graph for Devices, humanoid for Robots, 3D twin for Sites at ~40% opacity). Shows lens name, "N need attention" pill, 2–3 stats.
- **Secondary tiles** — 2-column grid, 22pt radius, diagonal tint, faded large lens icon in the corner.
- Blocks below (user-orderable / hideable via "Edit Home"): Needs you · Happening now · Quick actions.
- Org pill top-left (app icon avatar + "Wendy Labs" + chevron) opens the **Org sheet** (switch org). Bell top-right with unread badge.

### 3. Devices lens
**Home**
- Summary card: big count ("6 devices in Wendy Labs"), status legend (online / weak / offline), segmented status bar (green #34C759 / orange #FF9500 / grey #C7C7CC), two tiles **Local · nearby** and **Cloud** → device lists filtered by connection.
- Dashed "Set up a new device" CTA → Setup flow.
- **Needs attention** rows with inline action pills (e.g. "Update", "Fix"); tapping the row opens the device.
- **Recent data** (3 latest device recordings).

**Devices tab** — segmented `List | Mesh`.
- List: "Set up a new device" row, sections LOCAL · NEARBY NOW and CLOUD.
- Mesh: node graph of the phone + local devices with link quality (weak link −79 dBm highlighted); note that cloud-only devices are not on this mesh. The phone can configure the mesh even if it isn't a member.

**Device detail** — hero (icon, `DEVICE · MODEL`, name, status · place; chips: Online/Weak link, Local/Cloud, Agent version or "Update available", camera count; buttons **Logs** and **Update** (shows `Updating N%`) or **Restart**). Segmented tabs:
- **Apps** — each app: state (RUNNING / STOPPED / **WEB** if it publishes an http port), trailing `:port`. "Deploy an app" row.
- **App detail** — status, published ports; http ports get **Open in browser** (in-app browser sheet with URL bar showing `http://<host>.local:<port>` when local or the cloud tunnel URL when remote), ws ports show the URL to copy; Start/Stop/Restart; logs.
- **Cameras** — live feed tiles (name, resolution, timestamp overlay; tap → full screen), then camera settings rows.
- **Data** — recent recordings + "All recordings" (opens Data tab filtered).
- **Location** — Home place and Now place (both navigable), local vs remote note.
- **Settings** — Wi-Fi (change via Bluetooth), Mesh, **Agent & OS update** (version, check, update with progress), Restart, Unenroll (destructive, confirm).

**Data tab — devices with saved episodes + episode picker**
- Scrollable list of **only devices that have saved data** (footer: "N devices have no saved data").
- Each device card (white, 22pt radius, 12pt padding):
  - Top row: 60pt latest-episode thumbnail (14pt radius, compact — no overlays) · name 16/600 (never truncated) · "model · latest HH:MM · size" 12pt secondary · right: episode count 26/700 + "episodes" 11pt.
  - Second row (indented 72pt, wraps): label pills (24pt, `#F1EEE7`, colored dot + label + count in `#9A978F`), top 3 labels by count.
- Tap card → **Episode picker** for that device. Tap a label pill → picker pre-filtered to that label.
- **Episode picker** (bottom sheet, 640pt, 38pt radius, slides up 0.35s; dimmed backdrop closes it):
  - Header: device name 20/700, "N episodes · size", close ×.
  - Label filter chips: All + each label with count (selected = ink bg, white text).
  - Episodes grouped by day (TODAY / YESTERDAY): 84×54 thumbnail · name 15/600 · "time · duration · type" · label dot + name. Tap selects (checkmark, light green row).
  - Footer: **Export** (tonal) + **Play episode** (primary; disabled grey "Pick an episode" until one is selected).
- **Thumbnails by type**: video = frame (dark `#242B35` placeholder), audio = waveform bars, rosbag = topic lanes (/cam /imu /det), images = stacked frames, dataset = episode grid. Full-size thumbs show duration pill (bottom-right), sync icon (top-right: cloud = synced, chip = on device) and optional trigger pill (top-left).
- Labels = what triggered the recording: Person `#FF3B30`, Door opened `#FF9500`, Motion `#FFD60A`, Schedule `#64D2FF`, Manual `#9FE2BF`, Loud sound `#AF52DE`.

**Activity tab — segmented Alerts · Events · Archived N**
- **Alerts** = notification cards. Header row: "N UNREAD" / "ALL CAUGHT UP" + **Mark all read**.
  - Card: 36pt colored icon (label color for clip alerts) · title (700 if unread, 600 read) · "device · source" · time + green unread dot.
  - **Clip alerts** embed the clip (180pt video / 96pt audio waveform) with a centered 52pt white play button → opens the episode picker on that device with the clip preselected.
  - **Action alerts** (no clip) show **Open device** + primary action (e.g. **Update**, **Fix**). The update alert disappears once the update starts.
  - Tap header → device detail. Any open marks it read.
- **Swipe left to archive**: card follows the finger (vertical scroll still works). Release past ~44pt → snaps open at 96pt revealing a grey (`#5B5A56`) **Archive** action; release past ~200pt → archives immediately (card slides out, toast "Archived · find it in Activity › Archived"). Tapping an open card closes it. Footer hint: "Swipe left on an alert to archive it". Empty state: "No alerts".
- **Archived**: list of archived alerts with **Restore**. Stored on the phone + in Wendy Cloud for 30 days (placeholder retention), then deleted; their clips stay in Data. Archived alerts don't count toward the bell/unread badge.
- **Events**: log grouped TODAY / YESTERDAY — recording started, app started/restarted, Wi-Fi changed (via Bluetooth), mesh link dropped, agent updated, disconnected, enrolled over Bluetooth. Colored icon per kind; tap → device.
- In the Devices lens the **bell** opens Activity › Alerts; its badge includes unread, unarchived alerts.

### 4. Setup a new device (sheet, 4 steps)
`Scan` (Bluetooth spinner, "Power it on and keep it within a few metres") → `1 Choose a device` (found list) → `2 Connect to Wi-Fi` (networks; credentials sent over BLE) → `3 Where does it live?` (place picker) → `4 Enroll` (progress → done; opens the new device). Header shows "Step N of 4" and a close button. Primary button disabled (grey) until a choice is made.

### 5. Robots lens
- **Home**: stats, needs attention, live missions, latest sims.
- **Robot detail**: hero (battery, mission), body health (joint temperatures/torque — see `go/macos/Sources/App/SiteTwin/Go2Model.swift` joint names), compute (links to its onboard device in Devices lens), fleets, missions, data, **Sim** row.
- **Missions**: list + runs; mission detail = where & who, steps (Route · Detect · Goal), runs, data; Run now / Pause / Stop.
- **Sim**: runs per robot/scene with engine (MuJoCo / Isaac Sim) and PASSED/RUNNING/FAILED badges; detail with "Run on robot" once passed.

### 6. Sites lens
- **Home**: 3D twin preview card (dark, room plan with live asset dots; "Open 3D" → full-screen SceneKit twin), stats, needs attention, happening now, your places.
- **Places**: hierarchy Site › Floor › Room/Zone; place detail lists INSIDE, ASSETS BASED HERE, VISITING NOW, MISSIONS HERE, DATA. Plain language, no logs.
- **Alerts**: lens-filtered notifications.

### 7. Wendy chat (persistent)
- **Collapsed**: capsule bar above the tab bar ("Ask Wendy about <context>…" + mic). Shrinks to a 52pt round bubble on scroll-down or after navigation (peek), expands on scroll-up.
- **Open**: bottom sheet with detents **mid** (~55%) and **full**; the app stays interactive behind the mid detent (navigate while chatting). Close (×) collapses back to the bubble.
- **Unread badge**: if the model replies while collapsed, show a red dot/count on the bubble.
- **Context-aware**: header shows the current context chip (device, app, robot, place, mission or lens) and suggestion chips change with it (e.g. device: "Update the agent", "Restart people-counter", "Open the dashboard", "Switch Wi-Fi to Warehouse-IoT").
- **Action cards**: answers run instantly; anything that changes config or moves a robot returns a card with **Confirm / Cancel** (update, Wi-Fi change, app start/stop/restart, setup, open web page, run mission/sim). Confirming performs the same action as the UI button and shows progress inside the card.
- Reach (see Entity Diagram "What chat can do"): Devices — configure Wi-Fi/mesh/apps/updates/peripherals, explain logs; Robots — joints/torque/battery, drive/stop/dock/sim; Places — create rooms, name spots, draw routes; Fleets — create, bulk update; Missions — build from a sentence, run, summarise; Data — find, summarise, export.

### Notifications
Bell → list (Today / Earlier), each routes to its entity; read state greys the icon. Notifications are tagged per lens (`NL` map in prototype).

---

## State (from the prototype logic class)
`epDev, epLabel, epPick (episode picker), actSeg ('alerts'|'events'|'archived'), archived{alertId}, swipe{id,dx,drag}, onboard, primaryLens(focus), home{order,hidden}, editHome, lens, tab, stacks["lens:tab"]: [{t,id}], devTab[deviceId], devSeg, appRun[device|app], upd[deviceId]: progress|'done', setup{step,pick,wifi,place}, browser{url}, chatOpen, chatSize('mid'|'full'), chat[], chatUnread, accMini, read{notifId}, running{missionId: progress}, dataF{place,asset,mission}, org`.

Entity model: see `WendyOS Entity Diagram.dc.html`. Key rules — an asset has **one home place + one live location**; a robot **has** an onboard compute device; data has no home, it is **tagged** with asset, place and run; fleets are groups (rule or manual), not locations.

## Design tokens (Wendy brand)
Colors
- Brand green `#2A7050` (primary buttons, links, Devices lens, selected states); mint accent `#9FE2BF`
- Ink `#171C23` (text, dark surfaces), secondary text `#5B5A56`, tertiary `#9A978F`
- App background `#F1EEE7`, outer/page `#E6E2D8`, cards `#FFFFFF`, hairlines `#D7D6D2` / `#CBCBCB`, dashed edit outline `#CBCBCB`
- Lens colors: Devices `#2A7050`, Robots `#FF9500`, Sites `#4B5C7A`
- Status (iOS system): ok `#34C759` (text `#248A3D`), warn `#FF9500` (text `#C93400`), error `#FF3B30`, offline `#8E8E93`/`#C7C7CC`, location `#FF2D55`, camera `#30B0C7`
- Badge backgrounds = status color at 12–14% opacity

Typography — SF Pro (system). Large title 34/700 (+0.4 tracking); nav title 17/600 (−0.43); hero title 22/700; row title 16/600 (−0.3); body 15; subtitle 12–13 at 60% ink; section header 13/600 uppercase; kicker 11/700 +0.4 tracking; badge 11/700; big stat 44/700 (−1).

Radii — capsule 999; sheet 38; hero/large tile 26–30; cards/list groups 22; inner tiles 16; icon squares 10 (36pt) / 16 (54pt).
Spacing — screen padding 16; section gap 18; row padding 11×16; icon 36pt; hit targets ≥ 44pt; floating chrome (top buttons) 44pt circles with white 75% material.
Shadows — cards `0 1px 2px rgba(0,0,0,.04)`; floating chrome `0 1px 3px rgba(0,0,0,.07), 0 3px 10px rgba(0,0,0,.06)`; tab bar 62pt capsule, inset 16, bottom 25.
Motion — progress bars animate width 0.6s; chat bubble width 0.3s `cubic-bezier(.3,.8,.3,1)`; press state scale 0.97.

## Code map (where to build)
| Area | go/macos | wendy-companion-ios |
|---|---|---|
| Root, tabs, lenses | `Sources/iOS/MobileRootView.swift` | `Sources/` root/tab views |
| Device detail, apps, cameras | `Sources/App/DeviceExperience.swift`, `DeviceOverview.swift` | `DeviceDetailScreen.swift` (live/apps/settings), `DeviceDetailAction.swift` |
| Device settings, update | `DeviceOverview.swift` | `DeviceSettingsAction.swift`, `AppSettingsAction.swift` |
| Setup over Bluetooth | Bluetooth/provisioning code | existing provisioning flow |
| Sites, 3D twin | `Sources/App/SitesView.swift`, `SiteTwin/NativeSiteView.swift`, `Site.swift`, `SiteRoomCapture.swift` | — |
| Robot health | `SiteTwin/Go2Model.swift`, `SiteRobotPanel` | — |
| Notifications | — | `NotificationsAction.swift` |
| Activity | `DeviceOverview.swift` (`ExperienceActivityView`) | — |
| CLI parity reference | `go/internal/cli/commands/{root,device,data,bluetooth_radio}.go` | — |

**New in this design** (no existing screen): lenses + General Home + onboarding, persistent context-aware chat with action cards, Missions, Sim, Places hierarchy, in-app browser for app web ports.
**Exists in code, not yet in this design** (keep/merge): App Market, port forwarding (Datagrams), mTLS provisioning, Mesh VPN toggle, Campaigns, data visualizer, camera wall, model library, install guidance, cloud members.

## Assets
- `assets/AppIcon.svg` — from `wendy-companion-ios/WendyCompanionApple/Design/AppIcon.svg`
- Icons in the prototype are inline SVG line icons — use **SF Symbols** equivalents (`cpu`, `figure.walk`/robot, `building.2`, `cube`, `flag`, `cylinder.split.1x2`, `bell`, `point.3.connected.trianglepath.dotted`, `wifi`, `antenna.radiowaves.left.and.right`, `camera`, `arrow.down.circle`, `app`).

## Files
- `WendyOS iPhone v6 Companion.dc.html` — the interactive prototype (all screens). Mock data: `EPS` episodes, `ALERTS` alerts, `EV2` events, `P` places, `A` assets, `APPS`, `CAMS`, `M` missions, `S` sims, `D` data, `N` notifications. Chat intent matching: `reply()`, suggestions: `suggest()`.
- `WendyOS Entity Diagram.dc.html` — entity model and chat reach.
- `WendyOS Data Views.dc.html` — Data tab explorations (turn 3 = chosen design: device list + episode picker; turns 1–2 = alternatives for reference).
- `Clip Thumb.dc.html` — thumbnail component used by the Data/Activity screens (props: `clip`, `showTrig`, `compact`).
- `support.js`, `ios-frame.jsx` — only needed to open the prototype.
