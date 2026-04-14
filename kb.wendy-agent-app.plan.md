# WendyAgentApp AppKit Menu Migration Plan

## Goal

Replace the current SwiftUI `MenuBarExtra`-based menu with a native AppKit status bar item and `NSMenu` so the app uses standard macOS menu rendering and supports reliable menu item icons/state presentation.

## Why switch

The current implementation in:

- `swift/WendyAgentApp/WendyAgentApp.swift`
- `swift/WendyAgentApp/WendyAgentMenu.swift`

uses `MenuBarExtra` for both the menu bar item and the menu contents. In practice, the menu content is constrained by native menu rendering, and our attempts to render custom SwiftUI elements such as:

- overlays
- custom badges
- colored status dots
- more complex stacked layouts

have been unreliable or ignored.

If the requirement is "native Mac menus", AppKit is the right foundation:

- `NSStatusItem` for the menu bar item
- `NSMenu` for the popup menu
- `NSMenuItem` for rows
- optional template/status images for icons

## Target architecture

### 1. Replace `MenuBarExtra` with an AppKit coordinator

Introduce an AppKit-backed controller object responsible for:

- creating the `NSStatusItem`
- configuring its button image/title
- creating and rebuilding the `NSMenu`
- responding to menu actions such as Quit
- updating the menu whenever `WendyAgentStatus` changes

Suggested new file:

- `swift/WendyAgentApp/AppKitStatusController.swift`

Suggested type:

- `final class AppKitStatusController: NSObject`

Core responsibilities:

- hold `NSStatusItem`
- hold current `WendyAgentStatus`
- expose `func update(status: WendyAgentStatus)`
- expose `func setQuitHandler(_:)`
- rebuild menu contents when status changes

### 2. Keep the SwiftUI `App` entry point, but only as app lifecycle glue

Retain `@main struct WendyAgentApp: App`, but remove `MenuBarExtra` from `body`.

Instead:

- use a minimal `Settings { EmptyView() }` or similar scene placeholder
- create/manage `AppKitStatusController` from the app lifecycle
- continue owning:
  - `WendyAgent`
  - `WendyAgentStatus`
  - status observation
  - quit flow

`WendyAgentApp.swift` should become responsible for:

- bootstrapping agent state observation
- passing status updates to the AppKit controller
- terminating the app cleanly

### 3. Stop using SwiftUI for the popup menu contents

`WendyAgentMenu.swift` should either be:

- removed entirely, or
- reduced to status label helpers if any string mapping is worth reusing

The actual menu contents should be built with AppKit:

- `NSMenu`
- `NSMenuItem`
- separators

Example target menu structure:

1. status item row
   - title: `Idle`, `Starting`, `Running`, `Stopping`, `Stopped`, or `Failed`
   - icon/image indicating status category
2. if failed:
   - disabled detail row(s) showing the error message
3. separator
4. `Quit WendyAgent`

## Native menu representation strategy

Because `NSMenu` is row-based and not arbitrary-layout based, do not try to reproduce the current custom SwiftUI layout exactly.

Instead, use standard native menu idioms.

### Status row

Represent status as a normal disabled menu item.

Suggested mappings:

- `idle` → `Idle`
- `starting` → `Starting`
- `running` → `Running`
- `stopping` → `Stopping`
- `stopped` → `Stopped`
- `failed` → `Failed`

Mark the item disabled so it behaves like informational text, not an action.

### Status color / icon

There are two AppKit-friendly options.

#### Option A: status template images in assets

Create small status icons as assets, for example:

- `StatusMenuGray`
- `StatusMenuYellow`
- `StatusMenuGreen`
- `StatusMenuRed`

Use them as `NSMenuItem.image`.

Pros:

- most predictable in native menus
- easy to visually match System Settings
- no reliance on attributed title rendering

Cons:

- requires adding assets

#### Option B: SF Symbols where appropriate

Use symbols such as:

- `circle.fill`
- `exclamationmark.circle.fill`

with configured symbol images if rendering is acceptable.

Pros:

- quick to prototype

Cons:

- color/tint behavior in `NSMenuItem.image` can be inconsistent depending on rendering mode
- less predictable than dedicated assets

### Recommendation

Use **dedicated menu status dot assets** for reliability.

## Error message handling

For `.failed(let message)`, do not attempt a complex wrapped custom row first.

Use one of these native patterns:

### Preferred

Add one or more disabled menu items below `Failed`.

For long messages:

- either truncate to a concise summary in the menu
- or split into a couple of disabled lines

Example:

- `Failed`
- `Connection lost`
- separator
- `Quit WendyAgent`

### Alternative

Use an alert or separate window for detailed diagnostics later if needed.

For this migration, keep the menu simple.

## Status bar button strategy

The top menu bar item itself should also move to AppKit.

### Initial approach

Use the existing asset:

- `StatusIcon`

Set it on:

- `statusItem.button?.image`

Configure as template if needed so it behaves correctly in the menu bar.

### Error indication in the menu bar item

Do not attempt overlay composition in AppKit button title/image on day one.

Instead choose one of:

1. plain base icon only
2. swap between two full images:
   - `StatusIcon`
   - `StatusIconError`
3. append a text marker in the button title if acceptable

### Recommendation

Use **two complete assets** if a distinct error state is needed in the menu bar item itself.

## Concrete implementation steps

### Step 1: add an AppKit controller

Create:

- `swift/WendyAgentApp/AppKitStatusController.swift`

Implement:

- `final class AppKitStatusController: NSObject`
- `private let statusItem: NSStatusItem`
- `private var onQuit: (() -> Void)?`
- `private var currentStatus: WendyAgentStatus`

Methods:

- `init(status: WendyAgentStatus)`
- `func update(status: WendyAgentStatus)`
- `func setQuitHandler(_ handler: @escaping () -> Void)`
- `private func rebuildMenu()`
- `private func updateStatusButton()`
- `@objc private func quitSelected()`

### Step 2: build native menu items

Inside `rebuildMenu()`:

- create fresh `NSMenu`
- append a disabled status item with title from a shared mapping helper
- assign `image` from status category
- if failed, append disabled detail item(s)
- append separator
- append Quit item with action/target
- assign menu to `statusItem.menu`

### Step 3: move string/status mapping into a shared helper

To avoid duplication, define small internal helpers either in the controller or a separate file:

- `var menuTitle: String`
- `var menuImageName: String`
- `var isTransitional: Bool`

If convenient, add an internal extension on `WendyAgentStatus`.

Suggested file if extracted:

- `swift/WendyAgentApp/WendyAgentStatus+MenuPresentation.swift`

### Step 4: refactor `WendyAgentApp.swift`

Change `body` from `MenuBarExtra` to a placeholder scene, for example:

- `Settings { EmptyView() }`

Add stored state for the controller, e.g.:

- `private let statusController = AppKitStatusController(status: .idle)`

On app startup:

- set quit handler on the controller
- start bootstrap task
- update the controller whenever `status` changes

Possible hooks:

- initialize controller with the current state
- call `statusController.update(status:)` inside the status observation callback
- also call it when local `status` changes during startup/shutdown

### Step 5: remove SwiftUI menu views

After AppKit is working:

- delete or stop referencing `WendyAgentMenu`
- delete or stop referencing `WendyAgentStatusItem`

If no longer needed, remove:

- `swift/WendyAgentApp/WendyAgentMenu.swift`

### Step 6: add assets for menu status icons

Add new assets under:

- `swift/WendyAgentApp/Assets.xcassets`

Suggested image sets:

- `MenuStatusIdle`
- `MenuStatusTransition`
- `MenuStatusRunning`
- `MenuStatusFailed`

These should be tiny dot icons optimized for `NSMenuItem.image`.

## Proposed status mapping

| WendyAgentStatus | Menu title | Menu icon |
|---|---|---|
| `.idle` | `Idle` | gray dot |
| `.starting` | `Starting` | yellow dot |
| `.running` | `Running` | green dot |
| `.stopping` | `Stopping` | yellow dot |
| `.stopped` | `Stopped` | gray dot |
| `.failed(_)` | `Failed` | red dot |

## Suggested rollout order

1. create `AppKitStatusController`
2. wire `NSStatusItem` with a static menu and Quit action
3. connect live status updates
4. add native status row and failure detail row
5. remove `MenuBarExtra`
6. clean up unused SwiftUI menu code
7. optionally add alternate menu bar icon for error state

## Risks / tradeoffs

### Pros

- truly native macOS menu behavior
- predictable rendering
- easier to maintain for simple status menus
- no more fighting `MenuBarExtra` rendering limitations

### Cons

- less declarative than SwiftUI
- `NSMenu` is less flexible for custom layout
- richer visuals require assets instead of view composition

## Recommendation summary

Proceed with an AppKit migration based on:

- `NSStatusItem`
- `NSMenu`
- asset-backed status dot icons
- simple disabled status rows

This fits the product direction better than continuing to push custom SwiftUI layouts through `MenuBarExtra`.
