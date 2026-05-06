# Refresh Select Target Drive

## Problem

During `wendy os install`, the interactive "Select target drive" screen is populated from a single call to `listExternalDrives()` before the picker opens. If a user inserts an SSD, SD card, or USB drive after the picker is already displayed, the view does not refresh and the newly inserted drive never appears.

This is especially noticeable on Windows, but the current normal `os install` flow appears static on all platforms:

- `installLinuxImage` calls `listExternalDrives()` once.
- It converts those results into `tui.PickerItem`s.
- It calls `pickFromItems("Select target drive", driveItems)`.
- `pickFromItems` sends `PickerAddMsg` once, then immediately sends `PickerDoneMsg`.

There is drive-refresh behavior elsewhere in the `tour` wizard: the drive selection phase rescans every 2 seconds and updates the view. The regular `wendy os install` flow should likely get similar behavior, or another explicit refresh mechanism.

## Investigation Notes

There are two existing refresh patterns worth comparing:

### `tour` drive selection

The `wendy tour` wizard has a custom drive-selection phase (`phaseDriveWait`) rather than using `tui.PickerModel`.

Current behavior:

- entering the phase calls `listExternalDrives()` once
- the model schedules `rescanDrivesAfter(2 * time.Second)`
- that timer emits `tourDriveRescanMsg`
- `Update()` handles `tourDriveRescanMsg` by calling `listExternalDrives()` again
- `m.drives` is replaced with the new list
- another rescan is scheduled while the phase remains `phaseDriveWait`

This gives the desired UX, including removed drives disappearing from the list, but the scan itself happens directly in `Update()`. That is less consistent with the rest of the Bubble Tea code because blocking work is normally done in `tea.Cmd`s.

### `wendy discover`

`wendy discover` uses a custom Bubble Tea table model with command-driven polling:

- `Init()` starts scan commands via `tea.Batch(...)`
- each scan command performs discovery asynchronously and returns a typed result message
- `Update()` handles the result message by replacing the authoritative collection slice and calling `refreshTable()`
- `Update()` returns the next scan command, optionally wrapped in `delayThen(...)`

Examples:

- USB, Ethernet, and External discovery use `delayThen(interval, scanCmd)`
- LAN immediately starts another scan; the scan itself has a timeout
- BLE has source-specific retention logic because BLE scans are lossy

This pattern is preferable for time-based refresh: scan work lives in `tea.Cmd`s, result messages update model state, and the model schedules the next scan.

### Generic picker limitation

The normal `os install` path currently uses `pickFromItems(...)`, which creates `tui.PickerModel`, sends one `PickerAddMsg`, then sends `PickerDoneMsg`.

`PickerModel` currently supports append/merge semantics:

- `PickerAddMsg` adds unseen items and optionally merges duplicates
- `PickerDoneMsg` stops the "scanning" indicator

It does not currently support authoritative replacement of the whole item list. That matters for drives because inserted drives should appear and removed drives should disappear.

## Recommendation

Unify on the `wendy discover` **command-driven polling pattern** for time-based refreshes, but do not try to reuse the full `discoverModel` for drive selection.

`discoverModel` is a dashboard with multiple scan sources, multiple intervals, source-specific reconciliation, copy/update/default actions, and a custom table. A target-drive selector is a simple picker with one scan source, one interval, and one terminal action: select a drive. The shared abstraction should be the polling mechanics, not the entire UI model.

Recommended rules:

- Static choices continue using `pickFromItems(...)`.
- Time-refreshing picker choices use a new refreshing picker helper/model.
- Custom dashboards like `wendy discover` keep custom models, but use the same command-driven polling shape.
- Physical-resource lists, including drives, should use authoritative replacement rather than append-only merge.

## Proposed Implementation Plan

### 1. Add authoritative replacement support to `tui.PickerModel`

Add a new message, for example:

```go
type PickerSetMsg struct {
    Items []PickerItem
}
```

`PickerSetMsg` should replace the picker contents wholesale:

- rebuild `items`
- rebuild `seenIdx`
- preserve cursor position where possible
- clamp cursor if the new list is shorter
- call `refreshTable()`

This differs from `PickerAddMsg`, which should remain append/merge-oriented for streaming discovery.

### 2. Add a command-driven refreshing picker helper

Add a helper for dynamic picker flows, for example:

```go
func pickRefreshingFromItems(
    ctx context.Context,
    title string,
    interval time.Duration,
    load func(context.Context) ([]tui.PickerItem, error),
) (string, error)
```

or return the selected `tui.PickerItem` if callers need richer values.

Internally, it should follow the `discover` pattern:

```text
Init / start
  -> run load command

load result message
  -> send/handle PickerSetMsg
  -> schedule delayThen(interval, load command)

selection/cancel
  -> quit and cancel polling context
```

Avoid a simple background goroutine that repeatedly calls `p.Send(...)` unless the existing picker architecture makes that much simpler. For time-based polling, prefer Bubble Tea commands and result messages.

### 3. Use the refreshing picker for `wendy os install` drive selection

Replace the current interactive target-drive flow:

```text
listExternalDrives once
convert to PickerItem
pickFromItems("Select target drive", items)
```

with a refreshing drive picker that:

- rescans every ~2 seconds, matching the tour UX
- shows an empty-state message while no external drives are present
- lets newly inserted drives appear without restarting the command
- removes drives that disappear before selection
- returns the selected `drive`

Because picker values currently return strings, maintain a `DevicePath -> drive` mapping from the latest scan. Prefer making the helper return the full selected item, or set `Value` to a `drive` if safe for this package-local usage.

### 4. Share or centralize the delay helper

`discover.go` already has:

```go
func delayThen(d time.Duration, cmd tea.Cmd) tea.Cmd
```

The tour has a separate `rescanDrivesAfter(...)`. Standardize on one helper for command-delay polling. It can remain in the `commands` package if only command models use it, or move to `tui` if it becomes broadly useful.

### 5. Optional follow-up: clean up the tour drive refresh

The tour already has the right UX but a less ideal implementation. After the regular `os install` picker is fixed, consider refactoring the tour drive phase to use the same command-driven shape:

```text
drive scan cmd -> drive scan result msg -> replace m.drives -> delayThen(2s, drive scan cmd)
```

This is optional for the initial bug fix, but it would align all time-based refresh code with the `discover` approach.

## Expected Behavior

- Starting `wendy os install` with no external drive should keep the picker open instead of failing immediately.
- Inserting an SSD, SD card, or USB drive while the picker is open should make it appear automatically within the refresh interval.
- Removing a drive while the picker is open should remove it from the list or prevent selecting a stale entry.
- The UI should communicate that it is scanning/refreshing, ideally with copy similar to the tour: `Refreshes every 2 s` / `auto-refreshing`.
- Cancel behavior should remain unchanged: `q` or `ctrl+c` returns `ErrUserCancelled`.
- Non-interactive `--drive` behavior should remain unchanged.

## Tests

Add or update tests around:

- `PickerSetMsg` replaces items instead of appending.
- `PickerSetMsg` removes stale items.
- Cursor is clamped/preserved sensibly after replacement.
- Refreshing picker schedules another scan after a successful scan result.
- Refreshing picker handles an initial empty result without quitting.
- `os install` drive selection maps the selected picker item back to the correct `drive` from the latest scan.

Use short test intervals or injectable delay functions to avoid slow tests, similar to the existing `discover` tests that override discovery intervals via environment variables.

## Cross-Platform Considerations

- Windows drive insertion/removal is the main reported pain point, so verify behavior there if possible.
- `listExternalDrives()` differs by platform; the picker layer should not add platform-specific assumptions.
- Refresh interval should be long enough to avoid hammering platform drive-listing commands. The tour currently uses 2 seconds, which is a reasonable starting point.
- Treat scan errors carefully: transient listing failures should probably display an error state or warning without immediately exiting, but fatal errors can still abort. Match existing command conventions where practical.
