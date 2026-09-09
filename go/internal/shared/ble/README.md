# `internal/shared/ble` — generic BLE central (client)

A **cross-platform BLE central**: connect to a peripheral, talk GATT, and open an L2CAP channel that
can carry TLS. It runs on the machine acting as the BLE *central* and talks to whatever is acting as
the *peripheral*.

Four subpackages, each free of any Wendy identifier:

1. [`central`](central/) — the connection API (`Connection`), covering GATT and L2CAP, plus the
   L2CAP-to-`net.Conn` adapter TLS runs over.
2. [`scan`](scan/) — a continuously-streaming scanner that finds peripherals and yields the
   addresses `central.Connect` takes.
3. [`bluez`](bluez/) — the BlueZ D-Bus plumbing both use on Linux (object enumeration, adapter and
   device resolution, property readers, D-Bus error mapping).
4. [`permission`](permission/) — checks OS Bluetooth permission before a scan or connection touches
   the platform BLE stack for the first time.

Everything Wendy-specific lives in [`internal/cli/ble`](../../cli/ble/) — the Wendy Lite and WendyOS
agent protocol clients, their UUIDs, PSMs and framing. **Adding a Wendy identifier to `central`,
`scan` or `bluez` is the change to push back on**; a `shared/models` or `shared/certs` import into
one of them would be the first sign of it. (`WENDY_BT_ADAPTER`, the controller override `bluez`
honors, is the single deliberate exception — it has to match the name the agent already reads.)

There is one Wendy file at the root of this directory: `lite_info.go`, the Wendy Lite GATT info
service. It lives here rather than with its siblings in `cli/ble` because `shared/discovery` reads a
board's L2CAP PSM from it, and a `shared/` package importing a `cli/` one is an upward edge. It is
outside `central/` and `scan/` for the reason above, and nothing else should join it.

**This package must never import `shared/discovery`**, which imports it.

## Files

**`central/`** — the connection:

| File | Role |
| --- | --- |
| `ble_darwin.go` / `.h` / `.m` | macOS backend. cgo bridge to a CoreBluetooth implementation in Objective-C. |
| `ble_linux.go` | Linux backend. Raw `AF_BLUETOOTH` / `BTPROTO_L2CAP` socket via `golang.org/x/sys/unix`. |
| `bluez_linux.go` / `gatt_linux.go` | Linux GATT client over BlueZ D-Bus — session, device resolution, characteristic index, notification router. |
| `ble_windows.go` | Windows stub. Every method returns "not implemented". |
| `conn.go` | Adapts an L2CAP channel to `net.Conn` so Go's `crypto/tls` can run over it (`NewL2CAPStream`), plus `ErrRecvTimeout`, `ErrGATTNotFound`, `ErrGATTDisconnected` and `TimeoutSeconds`. |
| `uuid.go`, `gatt_errors.go` | Untagged, so their tests run on every platform. |

**`scan/`** — finding peripherals. Its own cgo bridge on macOS, BlueZ D-Bus on Linux, a WinRT
advertisement watcher on Windows. See [its section](#scanning-the-scan-subpackage).

**`bluez/`** — Linux-only helpers plus an untagged `errors.go`, so the D-Bus error table is testable
without a bus. `central` and `scan` are its only callers; `internal/agent/bluetooth` still carries
its own older copies of some of these.

`scan/` and `bluez/` have tests that run without a radio; `central`'s pure pieces (UUID
canonicalization, the characteristic index build, the notification router, address parsing, error
mapping) do too. The parts that need hardware are gated behind `WENDY_BLE_LIVE_*` (see below).

## The `Connection` API ([`central`](central/))

Each platform file in `central/` defines the same `Connection` type and method set, selected by build
tag — there is no Go interface, so the build tags are the contract. The API is blocking, with
integer-second timeouts.

```go
conn, err := central.Connect(address, 10)   // address semantics differ per platform, see below
defer conn.Close()

// GATT
err  = conn.DiscoverServices(10)
ok  := conn.HasService(serviceUUID)
s   := conn.ListServices()                                  // comma-separated discovered UUIDs
data, err := conn.ReadCharacteristic(svcUUID, chrUUID)
err  = conn.WriteCharacteristic(svcUUID, chrUUID, data)             // write with response
err  = conn.WriteCharacteristicNoResponse(svcUUID, chrUUID, data)   // write without response
err  = conn.Subscribe(svcUUID, chrUUID)                             // enable notifications
data, err = conn.WaitNotification(svcUUID, chrUUID, 5)              // pull one queued notification

// L2CAP (connection-oriented channel)
err  = conn.OpenL2CAP(psm, 10)
err  = conn.L2CAPSend(payload)
data, err = conn.L2CAPRecv(30)
```

### Capability matrix

| Capability | macOS | Linux | Windows |
| --- | :---: | :---: | :---: |
| Connect to peripheral | ✅ CoreBluetooth | ✅ (socket created; the link comes up in `OpenL2CAP` or `DiscoverServices`) | ❌ |
| Service / characteristic discovery | ✅ | ✅ BlueZ D-Bus | ❌ |
| Read / write characteristic | ✅ | ✅ | ❌ |
| Write without response | ✅ | ✅ | ❌ |
| Notifications (subscribe + wait) | ✅ | ✅ (per-characteristic queues) | ❌ |
| L2CAP channel (open / send / recv) | ✅ | ✅ | ❌ |
| `net.Conn` + TLS over L2CAP | ✅ | ✅ | ❌ |

On Windows the GATT methods exist only to satisfy the shared method set; they return
`"not implemented"`.

**The Linux L2CAP path was broken in four ways at once**, and all four are fixed. They are recorded
because each masked the others: every one presented as the same uniform connect timeout or
mid-handshake disconnect, and none pointed at its own cause. Two of them also invalidated the
experiments that would have found the others, so the order they were fixed in mattered.

1. `parseBTAddr` guarded its separator check on the loop index rather than the byte offset, so it
   indexed `s[-1]` and **panicked on every valid address**. `central.Connect` could never return.
2. `parseBTAddr` then returned the address least-significant-byte first, but `x/sys/unix` reverses
   `SockaddrL2.Addr` while marshalling — it wants the human order and does the conversion itself.
   Pre-reversing cancelled that out and aimed every connect at a byte-reversed peer, so nothing
   answered. Every PSM timed out identically, including ones nothing listens on, which a reachable
   peer would have *refused*.
3. **The socket was never bound**, so the channel ran in basic L2CAP mode rather than LE
   credit-based flow control. `l2cap_sock_connect` only switches mode when the channel's source
   address type is LE, and only `bind` sets that. In basic mode the kernel writes each payload as a
   bare PDU **with no 2-byte SDU-length header**, so the peer reads the first two bytes of the data
   as the SDU length. A TLS ClientHello begins `0x16 0x03`, which reads as an SDU of 790 bytes —
   past the peer's MTU, so it dropped the channel immediately. This is why the failure looked
   content-dependent: a payload of `00 00` reads as a legal empty SDU and is accepted, while three
   zero bytes read as an empty SDU with a stray byte after it and are not.
4. `L2CAPSend` looped over the unwritten remainder, which cannot chunk anything: the socket is
   `SOCK_SEQPACKET`, so one write is one SDU and the kernel fails with `EMSGSIZE` rather than
   writing part of it. The first TLS record was larger than the negotiated MTU and simply failed.
   It now splits at the MTU read back from `BT_SNDMTU` once the channel is up.

A fifth, separate bug: `poll(2)` returning `EINTR` was reported as a failure. The Go runtime
preempts goroutines with `SIGURG`, so a long idle wait is interrupted routinely — an idle channel
would surface "interrupted system call" instead of the timeout sentinel `l2capNetConn.Read` knows to
retry, which breaks any connection that goes quiet. Poll, read and write now retry on `EINTR`.

Verified against a Wendy Lite board end to end: channel open, TLS handshake, WendyCom handshake and
round-tripping pings.

Note for anyone debugging this layer again: the socket API gives almost no visibility here — a
malformed K-frame is indistinguishable from an unreachable peer, because both simply fail to get a
reply. `btmon` is the tool that settles it, since the missing SDU-length header and the peer's
`Disconnection Request` are both plainly visible on the wire and neither is inferable from `errno`.

### Addressing is platform-dependent

`Connect` takes a *string*, but its meaning is not the same everywhere:

* **macOS** — a CoreBluetooth peripheral UUID (`"XXXXXXXX-XXXX-…"`). CoreBluetooth never exposes
  the hardware MAC address.
* **Linux** — a Bluetooth MAC address, `"AA:BB:CC:DD:EE:FF"` (parsed to a `[6]byte` in written
  order, which is what `unix.SockaddrL2` takes — see the note below). The LE
  address type is read from BlueZ's `Device1.AddressType` where the device is known, so
  random-static and resolvable-private addresses work; when BlueZ has never seen the device,
  `OpenL2CAP` tries public then random.

Either source of addresses hands you the right kind of value per platform: the [`scan`](scan/)
subpackage in `BLEDeviceInfo.Address`, or [`shared/discovery`](../discovery/) in
`models.BluetoothDevice.Address`. Windows is the gap — `scan` can find peripherals there, but
`central.Connect` cannot reach them.

### L2CAP-over-TLS bridge (`central/conn.go`)

`NewL2CAPStream(conn)` wraps a `*Connection` as a `net.Conn`. Call it after `OpenL2CAP`; closing the
returned conn closes the underlying BLE connection.

* `Read` polls `L2CAPRecv` in `recvChunkSeconds` (2 s) slices and retries `ErrRecvTimeout`, so one
  `Read` may span many `L2CAPRecv` calls. Leftovers from an oversized read are buffered for later
  reads — needed because macOS coalesces incoming bytes into a stream buffer while Linux
  `SOCK_SEQPACKET` preserves SDU boundaries.
* **With no read deadline set, `Read` blocks indefinitely**, as `net.Conn` requires. The 2 s slice is
  polling granularity, not a timeout callers see. With a deadline it returns a `net.Error` whose
  `Timeout()` is true, which is what stops `crypto/tls` treating a deadline as a broken connection.
  Write deadlines are ignored.
* `Close` takes the same lock `Read` holds across `L2CAPRecv`, so it waits out an in-flight read (at
  most 2 s). That matters on macOS, where `wendy_ble_disconnect` hands the CoreBluetooth wrapper to
  ARC while a blocked reader still holds a bare pointer to it. Its idempotency flag is now belt and
  braces — `Connection.Close` is idempotent on both backends.
* `LocalAddr`/`RemoteAddr` are placeholders (`"ble-l2cap"` / `"ble"`).

The TLS config that runs over this is the caller's business. Wendy's is
`ble.NewClientTLSConfig` in [`internal/cli/ble`](../../cli/ble/): it sets `InsecureSkipVerify: true`
— there is no hostname to verify over L2CAP, and ML-DSA chain certs don't parse in Go's built-in
verifier — and performs real validation in a `VerifyConnection` callback from `shared/certs`. That
function depends on the Wendy PKI, which is exactly why it is not here.

## Scanning (the [`scan`](scan/) subpackage)

`central.Connect` needs an address, and this is where addresses come from. It is as protocol-free as
`Connection` is — the services to look for are an argument, and a result carries only what a BLE
advertisement can actually supply.

```go
devices, err := scan.DiscoverBluetoothContinuous(ctx, scan.Options{
    Services: []string{"7565E9EB-4C20-4B67-9272-D708B397B631"}, // empty = every device in range
})
if err != nil {
    return err // the scan could not start at all; a mid-stream failure closes the channel instead
}
for set := range devices {
    // set is the COMPLETE list, sorted by RSSI descending — replace, don't merge.
    // set[i].Address feeds straight into central.Connect.
}
```

A stream re-emits the whole array whenever it changes and closes when `ctx` is cancelled. Devices
accumulate and are never dropped: BLE has no "device went away" signal, and a disappearance timeout
would make a peripheral blink out of a picker between advertisements. Emits are coalesced on
`Options.Interval` (1 s by default), which is what stops RSSI churn — it changes on nearly every
advertising packet — from spinning the channel.

Two things it deliberately does *not* report:

* **No L2CAP PSM.** A PSM is not in an advertisement. Read it from the peripheral over GATT after
  connecting, or fall back to a per-protocol default the caller owns.
* **No stable cross-platform identity.** `Address` is a CoreBluetooth UUID on macOS and a MAC on
  Linux and Windows, exactly as `central.Connect` requires, so it cannot be compared across
  machines.

`Name` is the advertised local name, and it is a display label rather than an identity: macOS falls
back to CoreBluetooth's cached name and Linux to BlueZ's persisted one, so it can outlive the
advertisement it came from. Windows has no cache to fall back on and reports `""` instead.

Service UUIDs are normalized through `scan.CanonicalUUID`, which matters more than it looks:
CoreBluetooth renders a 16-bit UUID as four hex characters (`"180F"`) where BlueZ and WinRT give the
full 128-bit form, so comparing raw strings across platforms silently misses matches. `central` has
its own copy of that function — it must not import `scan` — and the two must stay identical.

| Capability | macOS | Linux | Windows |
| --- | :---: | :---: | :---: |
| Continuous scan | ✅ CoreBluetooth | ✅ BlueZ D-Bus | ✅ WinRT advertisement watcher |
| Filter by service UUID | ✅ (in Go) | ✅ (BlueZ + Go) | ✅ (WinRT + Go) |
| RSSI | ✅ | ✅ | ✅ |
| Advertised name | ✅ | ✅ | ✅ (no cache fallback) |
| `RunBLECheck` is meaningful | ✅ | ❌ returns 0 | ❌ returns 0 |

**The Windows column is unverified on hardware.** It compiles and its JSON parsing is covered by
tests, but no one has run it against a real radio, and the WinRT event binding is the part most
likely to need work — `Register-ObjectEvent` binds WinRT `TypedEventHandler` events unevenly, so the
script may need an `Add-Type` inline-C# shim instead. Treat it as a first cut. Windows also has no
`central` backend, so nothing it finds can be connected to yet.

The macOS backend keeps a long-lived `CBCentralManager` and lets Go poll a snapshot, so there are no
cgo→Go callbacks and nothing ever calls into Go from a CoreBluetooth thread. It scans with
`AllowDuplicates: YES` and **no** native service filter — CoreBluetooth's own filter drops
peripherals whose advertisement omits the UUID, which is exactly the case a name-based fallback
exists to catch — and filters in Go instead. The Windows backend runs one long-lived Windows
PowerShell 5.1 process (the WinRT-projecting host) driving a `BluetoothLEAdvertisementWatcher` and
streaming JSON lines back.

Because CI has no radio and no macOS job compiles this cgo at all, the only thing that exercises the
real bridges is the hardware-gated test:

```sh
WENDY_BLE_LIVE_SCAN=1 go test ./internal/shared/ble/scan -run TestLiveScan -v
# WENDY_BLE_LIVE_SERVICES=<uuid>[,<uuid>] to exercise filtering
```

## Permission preflight (the [`permission`](permission/) subpackage)

`permission.Preflight(ctx)` is meant to be wired into `scan.Options.Preflight` (or run before any
other first touch of the platform BLE stack) by a caller that wants a clean error instead of a
crash on macOS. It re-execs the running binary as the hidden `__ble-check` subcommand
(`permission.CheckArg`) — `cmd/wendy` registers that subcommand to run `scan.RunBLECheck()` and
exit with its result — so the CoreBluetooth touch that can `SIGABRT` a process with no Bluetooth
TCC permission kills a disposable child instead of the caller. The result is cached with
`sync.Once` for the life of the process, since terminal Bluetooth permission cannot change while a
`wendy` invocation is running.

It has no platform build tags and no dependency on `scan` or `central` — it only knows how to
re-exec itself and interpret an exit code, so it is safe for any caller in this module to import.

## Platform implementation notes

### macOS (`central/ble_darwin.m`)

The interesting part is that this runs inside a **CLI binary with no main run loop**, which
CoreBluetooth normally assumes exists.

* The `CBCentralManager` gets its own serial dispatch queue; every C entry point blocks on a
  `dispatch_semaphore_t` signalled from the delegate callbacks.
* `Connect` first tries `retrievePeripheralsWithIdentifiers:` (CoreBluetooth shares its peripheral
  cache process-wide, so a peripheral already seen during discovery needs no re-scan) and only
  falls back to scanning. This avoids a 10 s stall when the device stops advertising between
  discovery and connect.
* L2CAP streams get a **dedicated `NSThread` running a real `NSRunLoop`**, because
  `NSStreamDelegate` events are otherwise never delivered. Writes are marshalled onto that thread
  with `performSelector:onThread:waitUntilDone:YES` — a `CBL2CAPChannel` output stream returns `-1`
  when written from a foreign thread (e.g. Go's TLS goroutine), even when the stream reports open.
  Channel setup waits for `NSStreamEventHasSpaceAvailable` (5 s cap) before declaring the channel
  usable.
* Incoming L2CAP bytes accumulate in an `NSMutableData` behind a lock; `L2CAPRecv` drains the whole
  buffer at once. Reads use a 4096-byte chunk.
* Requires cgo, `-framework CoreBluetooth`, and the macOS Bluetooth TCC permission. Sandboxed
  terminals can `SIGABRT` on CoreBluetooth init rather than returning an error, which is why the
  probe runs in a throwaway subprocess: [`permission.Preflight`](permission/) re-execs the CLI as
  `__ble-check`, and `scan.RunBLECheck` is the same probe for a caller to wire into
  `scan.Options.Preflight`.
* This file and `../scan/scan_darwin.m` both link into one binary, so their C symbols and
  Objective-C class names must not collide — ObjC class names are process-global and a duplicate
  makes the runtime pick one arbitrarily. Hence `wendy_ble_*` / `WendyBLEConnection` here versus
  `wendy_blescan_*` / `WendyBLEScanSession` there.
* Error codes are a small C enum (`timeout`, `not found`, `connect failed`, `discover failed`,
  `write failed`, `read failed`, `L2CAP failed`, `disconnected`) mapped to Go `error` strings by
  `bleError`. The one exception is `L2CAPRecv`, which returns the `ErrRecvTimeout` sentinel instead
  of routing a timeout through `bleError`, so a caller can tell an idle channel from a dead one and
  retry. Everything else surfaces as an opaque string.

### Linux (`central/ble_linux.go`, `bluez_linux.go`, `gatt_linux.go`)

Two halves that share only the peer's address, and the split is deliberate.

**L2CAP** is a raw socket, no cgo and no D-Bus. `Connect` only creates the socket; `OpenL2CAP` does
a best-effort, non-fatal BlueZ lookup for the peer's LE address type, then the non-blocking
`connect(2)` + `Poll` (so the timeout is respected) + `SO_ERROR` check, then switches back to
blocking mode. Receive is `Poll` + one `read(2)` into a 64 KiB buffer, one SDU per call. When BlueZ
has no object for the device its address type is unknown, so the caller's timeout is split and
public is tried before random — a wrong type times out rather than failing fast, because the
controller ends up paging an address nobody answers.

Three things are easy to get wrong here and were. The socket must be **bound** before connect, with
an LE source address type — that, and nothing else, is what puts the channel into LE credit-based
flow control instead of basic mode; the bound address itself is irrelevant, `BDADDR_ANY` is fine.
`SockaddrL2.Addr` takes the address in the order it is written, most significant byte first:
`x/sys/unix` reverses it into the kernel's little-endian `bdaddr_t` itself, so handing it a
pre-reversed address silently targets the wrong peer. And sends must be split at the negotiated SDU
size — `SOCK_SEQPACKET` means one write is one SDU, so an
oversized buffer fails with `EMSGSIZE` and a write loop never gets the chance to chunk it. The size
comes from `BT_SNDMTU`, which is only meaningful once the channel is open.

`Connect` stays lazy on purpose: the kernel's L2CAP connect does its own scan-and-connect and reaches
a device BlueZ has no object for at all, which is the normal case some seconds after a scan stops.
Making `Connect` establish a link through BlueZ would break that, and the pure-L2CAP callers never
ask for GATT anyway.

**GATT** goes through BlueZ over D-Bus, because there is no reasonable alternative — an ATT client on
a raw socket would mean implementing the protocol by hand, and BlueZ owns the link anyway.
`DiscoverServices` resolves the device object by matching its `Address` property (never by
synthesizing `/org/bluez/hciX/dev_AA_BB_…`), calls `Device1.Connect()`, waits for `ServicesResolved`,
then walks `GattService1` / `GattCharacteristic1` objects under the device path into a
`(service UUID, characteristic UUID) → object path` index. BlueZ reports UUIDs lowercase and 128-bit
where callers pass uppercase, so both sides go through `canonicalUUID`.

BlueZ evicts unpaired devices from its object tree roughly 30 s after discovery stops, so a MAC that
a scan produced often has no D-Bus object left by the time GATT runs. On that miss `DiscoverServices`
runs a short `StartDiscovery`, polls until the device appears, and **stops discovery before
connecting** — an outgoing LE connect while the controller is scanning fails on older kernels.

Notifications arrive as `PropertiesChanged` signals carrying `Value`. One match rule on the device's
path namespace covers the device object and every characteristic under it; a single router goroutine
fans those into **one bounded queue per characteristic**, which is why `WaitNotification` is
per-characteristic here and not per-connection as it is on macOS. The router never blocks — godbus
spawns a goroutine per signal rather than dropping when a registered channel is full, so a stuck
router would leak unboundedly — and drops the oldest value when a queue fills.

Two BlueZ behaviors to know: `ReadValue` also updates `Value` and emits `PropertiesChanged`, so a
read on a subscribed characteristic enqueues a phantom notification; and gdbus batches property
changes per main-loop iteration, so two notifications in the same iteration can collapse into one.
Neither is worth working around for a rate-limited status characteristic, and the escape hatch if a
streaming one ever appears is `AcquireNotify`, which hands back a seqpacket fd and bypasses D-Bus.

`Close` tears the ACL link down only if this connection brought it up *and* no L2CAP channel is
riding on it — `Device1.Disconnect()` drops the HCI link, which would kill a channel belonging to us
or to another process.

Pairing/bonding is assumed to already exist; this package never initiates it and registers no
`org.bluez.Agent1`. A peripheral that demands encryption surfaces `NotPermitted` / `NotAuthorized`,
mapped to a message telling the user to pair with `bluetoothctl` first.

## Known limitations

These are all about `central`'s `Connection` API; `scan`'s own caveats are in its section above.

* **GATT is not goroutine-safe**, on either backend. At most one outstanding GATT operation per
  connection. The L2CAP data path is the exception: one concurrent sender plus one concurrent
  receiver is supported and relied on (`liteclient`'s framing writes from command goroutines while
  its read loop blocks in `Read`). On macOS that holds because sends are dispatched onto the I/O
  thread and never touch the recv buffer or its semaphore; on Linux because `write(2)` and `read(2)`
  on one socket are independent. Two concurrent *senders* are still unsafe — on macOS they race on
  the shared write result, and on Linux `L2CAPSend`'s write loop would interleave and corrupt the
  stream — so callers must serialize writes, as `liteclient` does.
* **macOS only: `WaitNotification` is per-connection, not per-characteristic.** All subscriptions
  share one semaphore; a notification on characteristic *A* wakes a waiter on *B*, which then finds
  its own queue empty and reports a timeout. Fine with one subscription, fragile with several. Linux
  gives each characteristic its own queue.
* **macOS only: a read on a subscribed characteristic lands in the notification queue.** The
  `didUpdateValueForCharacteristic:` handler checks the notify queues first, so `ReadCharacteristic`
  on a characteristic you've subscribed to never gets its response and times out after 10 s. On
  Linux a read is a D-Bus method call with its own reply and cannot be confused with a notification.
* **No `context.Context`**, no cancellation; timeouts are whole seconds only, and several
  (GATT read/write/subscribe: 10 s) are hard-coded.
* **No MTU or max-SDU negotiation** is exposed. Linux reads `GattCharacteristic1.MTU` where BlueZ
  publishes it, but only to decide whether a read may have been truncated.
* **No descriptor access, no indications** (only notifications), no characteristic-property
  inspection beyond the `Flags` Linux puts in error messages.
* **Central role only** — no advertising, no GATT server, no peripheral role.
* **No reconnect or retry logic**; a disconnect surfaces as an error on the next call.
* On macOS, a disconnect wakes blocked waiters but `L2CAPRecv` may report `disconnected` rather
  than a clean EOF.
