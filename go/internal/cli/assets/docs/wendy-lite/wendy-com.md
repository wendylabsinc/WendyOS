# WendyCom Protocol

## Basics

WendyCom is a lightweight request-response protocol used to control Wendy Lite devices. It runs over two transports: USB (via the ESP32 USB Serial JTAG peripheral) and mTLS (over the network).

The host sends a `WendyComCommand` and the device replies with a `WendyComResponse`. Each command carries a `request_id` that the response echoes back, allowing the host to match replies to outstanding requests. The command type is encoded as a `oneof` field in the protobuf message.

In addition to the request-response exchange, either side can send unsolicited `WendyComEvent` messages at any time. Events are not tied to a request: they carry an `event_id` instead of a `request_id` and receive no reply. The device uses them, for example, to stream console output after a `ConsoleAttach` command (the `event_id` given in the attach params tags the resulting events).

Each message consists of an 8-byte header followed by a protobuf-encoded body.

### Message Header

|Offset|Field|Type|Description|
|------|-----|----|-----------|
|0|`magic`|`uint8_t`|Fixed sync byte `0xA5`, used to detect framing errors|
|1|`version`|`uint8_t`|Protocol version, currently always `2`|
|2|`category`|`uint8_t`|Message category; only `0` is accepted (non-zero triggers a receive reset)|
|3|`channel`|`uint8_t`|Sub-channel within the link — see [WendyCom via Cloud](#wendycom-via-cloud)|
|4–5|`reserved`|`uint16_t`|Unused, set to `0` on send|
|6–7|`body_size`|`uint16_t`|Length of the body that follows, in network byte order|

## WendyCom over TLS

The device listens on TCP port 5054 and advertises itself via mDNS as `_wendy-lite._tcp`. Every connection is TLS-encrypted. The device operates in one of two modes depending on whether it has been enrolled in Wendy Cloud:

- **Unauthenticated TLS** — When not enrolled, the device uses a built-in default certificate; client identity is not verified. This is the factory default. The mDNS TXT record advertises `mtls=false`.
- **mTLS** — When enrolled, the device uses a provisioned certificate and requires the client to present a certificate signed by the configured chain of trust. The mDNS TXT record advertises `mtls=true`.

## WendyCom over USB Serial JTAG

WendyCom runs over the ESP32 USB Serial JTAG peripheral. This link supports several modes, WendyCom being one of them; the host switches between them by sending escape sequences.

### Modes

**`USJ_MODE_OFF` (0)** — USB input is silently discarded.  No data is written to the channel.

**`USJ_MODE_CONSOLE` (1)** — ESP log output is mirrored to the USB channel (the `vprintf`-hook forwards every log line).  `wendy_usj_write()` is active.  This is the startup default.

**`USJ_MODE_ECHO` (2)** — Every received byte is immediately echoed back.  Useful for connectivity checks.

**`USJ_MODE_COM` (3)** — The channel is handed to the `wendy_com` stack, enabling WendyCom message exchange over USB as we do over mTLS.

### Escape sequences

`DLE` (0x10) characters in the data stream are interpreted as commands. Each `DLE` is followed by a command byte:

```text
DLE c  →  switch to console mode
DLE e  →  switch to echo mode
DLE m  →  switch to com mode
DLE o  →  switch to off mode
```

In `USJ_MODE_COM`, the `wendy_com_uart` layer intercepts escape sequences before data reaches the `wendy_com` stack:

```text
DLE _    →  pass a literal DLE byte through to the `wendy_com` stack
DLE k    →  keep-alive (reserved, not yet implemented)
DLE <x>  →  disconnect the `wendy_com` link, then switch to mode <x>
```

Two consecutive `DLE` characters are considered like one. Therefore, you can put as many `DLE` as you want in front of a command byte.

### Establishing a WendyCom connection

Before entering `USJ_MODE_COM`, the host must flush any stale data buffered in the
USB layer on both sides.  The handshake uses echo mode for this:

1. Open the USB channel.
2. Send `DLE e` to switch to echo mode.
3. Verify the link: send a few bytes and confirm that data flows back.
4. Drain the channel: read until no data arrives within a timeout.
5. Send a unique sentinel byte sequence and wait until it is echoed back —
   this confirms the channel is fully flushed and both sides are in sync.
6. Send `DLE m` to switch to `USJ_MODE_COM`.

The channel is now in `USJ_MODE_COM` with no stale data in either buffer.

#### Implementation notes

A board that has just rebooted is a hostile environment for this sequence: boot
logs keep the stream continuously readable so it never goes quiet, bytes can be
dropped, and the device is not ready to serve WendyCom for some time after USB
comes up. The CLI's implementation (`serialHandshake`, in
`go/internal/cli/liteclient/link_direct.go`) is therefore more aggressive than
the steps above, collapsing 2-5 into a single retry loop:

- **Steps 2 and 5 go out as one write.** Each attempt sends
  `DLE DLE DLE DLE e` immediately followed by 16 spaces of padding and a fresh
  32-character random sentinel, as a single payload.
- **The echo mode command is repeated with every attempt.** The device refuses
  `DLE e` until its WendyCom agent is running, and the window between USB
  initialization and that point is wide enough to swallow the first attempt.
  Applying the command is a plain mode assignment, so re-issuing it is
  idempotent. Escape sequences are consumed by the mode parser and never echoed,
  so the prefix cannot show up in the echoed stream.
- **Every attempt carries a fresh sentinel.** The sentinel is a stream position
  marker, not a liveness probe: because the echo is FIFO, seeing the *latest*
  one come back proves nothing the host wrote earlier is still in flight, which
  is what step 4 is really after. Matching an older sentinel would leave the
  later echoes queued, and the WendyCom framer reads their leading padding as a
  frame header.
- **The padding is 16 spaces.** A `DLE` whose command byte is lost stays latched
  and consumes the byte after it, so a run of expendable bytes ahead of the
  sentinel keeps a dropped byte from corrupting it.
- **Matching uses a rolling window.** The host reads one byte at a time, keeping
  the last 32, and compares that window to the sentinel just sent. Draining the
  channel (step 4) falls out of this for free, without the stream ever having to
  go quiet.
- **Retries widen.** The first attempt goes out immediately, then the host waits
  100 ms before retrying, then 200, 300, 400 and 500 ms, holding at 500 ms,
  within a total budget of 3 seconds — about eight attempts. Widening keeps
  probing a device that is still booting without pushing a kilobyte of sentinels
  at it.

### Switching to program mode

In addition to the modes described above, the host can reset the device into program mode via the DTR and RTS signals exposed by the USB Serial JTAG peripheral. The `wendy os install` command uses this to flash the firmware.
See [wendy os install](../clients/wendy-cli/commands/os/install.md)

### Caveat

USB Serial JTAG currently has no flow control. If the host sends data faster than the device can consume it, bytes may be silently dropped. To avoid this, messages must be kept below 1024 bytes, and the host must wait for a response before sending the next message.

## WendyCom via Cloud

Devices enrolled in Wendy Cloud can be reached through a cloud broker instead of a direct USB or TLS connection. The device dials **out** to the broker over mTLS and keeps that single connection alive, reconnecting automatically after a delay if it drops. Once established, the socket is handed to the `wendy_com` stack as a regular link — from the device's perspective a cloud connection behaves exactly like a local TLS client. There is at most one cloud link at a time.

Clients (the CLI) do not connect to the device; they connect to the broker over **gRPC** and the broker relays WendyCom messages between the client and the device's socket. The WendyCom protocol itself — handshake, commands, responses, events — runs end-to-end between client and device; the broker is transparent to it.

### CLI ↔ cloud: gRPC tunnel

The broker exposes a bidirectional streaming RPC:

```proto
service WendyComTunnelBrokerService {
    rpc WendyComTunnel(stream WendyComTunnelMessage) returns (stream WendyComTunnelMessage);
}
```

The client's first stream message must be `WendyComTunnelOpen`, which carries the `asset_id` of the target device. Every subsequent message, in both directions, is a `WendyComTunnelPayload` whose `bytes` field contains one protobuf-encoded `WendyComMessage` body — **without** the 8-byte header. Framing and channel numbering are owned entirely by the broker: it prepends the header (with the channel byte) on the way to the device and strips it on the way back.

The lifetime of the gRPC stream is the lifetime of the tunnel: closing the stream closes the channel on the device, and vice versa.

### Channel multiplexing

The `channel` byte of the message header lets several clients share the device's single cloud socket. Each gRPC tunnel stream is assigned its own channel by the broker; frames from the device are routed back to the owning stream by their channel byte.

- **Channel 0 is open by default.** It is created implicitly when a link connects and can never be opened or closed explicitly. Direct connections (USB, local TLS) always use channel 0.
- The broker assigns channels `1`–`255` to tunnel streams, one channel per stream. Released channel numbers are quarantined for a short time before reuse, so a late frame from the device cannot reach a channel's new owner.
- The device currently supports up to 4 channels per link, including channel 0.

Data received on a channel that has not been opened is reported with a `channel_state` error (see below).

### Channel open/close service messages

The broker manages channels on the device using `WendyComService` messages:

```proto
message WendyComOpenChannel {}
message WendyComCloseChannel {}

message WendyComChannelOpen  {}
message WendyComChannelClose {}

enum WendyComChannelErrorReason {
    WENDY_COM_CHANNEL_ERROR_REASON_NOT_OPEN = 0;
    WENDY_COM_CHANNEL_ERROR_REASON_REJECTED = 1;
}

message WendyComChannelError {
    WendyComChannelErrorReason reason = 1;
}

message WendyComChannelState {
    oneof state {
        WendyComChannelOpen  open  = 1;
        WendyComChannelClose close = 2;
        WendyComChannelError error = 3;
    }
}

message WendyComService {
    oneof cmd {
        WendyComOpenChannel  open_channel  = 1;
        WendyComCloseChannel close_channel = 2;
        WendyComChannelState channel_state = 3;
    }
}
```

When a new tunnel stream is opened, the broker sends `open_channel` on the allocated channel and waits for the device's confirmation before relaying any payload; when the stream ends, it sends `close_channel` and releases the channel.

The device answers channel management with a `WendyComChannelState` message, sent device → host on the channel concerned:

- `open_channel` → the device replies with `channel_state` `open` on success, or `error` with reason `REJECTED` if the channel cannot be opened (all channel slots in use, or the channel is already open — including channel 0).
- `close_channel` → the device replies with `channel_state` `close`, or `error` with reason `REJECTED` if the channel cannot be closed (channel 0) or is not open. The device may also send `channel_state` `close` unsolicited when it closes a channel on its own.
- Any other (disallowed) service message → the device replies with `channel_state` `error` with reason `REJECTED`.
- A command received on a channel that is not open → the device reports `channel_state` `error` with reason `NOT_OPEN`.

`channel_state` reports are consumed by the broker — they never reach the gRPC client.

## WendyCom Handshake

A handshake message is sent automatically when a connection is established. The host sends the protocol version it supports (e.g. `major=1, minor=0`) along with a fresh `handshake_id`; the device replies with the same kind of message, echoing the `handshake_id` and carrying its own protocol version. The major version must match on both sides — the device closes the connection if the host's major version is unsupported. Commands sent before the handshake completes are rejected.

## WendyCom Commands

### Ping

Checks that the device is reachable and responsive. The device replies with `OK` and no payload.

### Reboot

Asks the device to reboot. The device replies with `OK` before resetting.

### App Download

Transfers a WASM application binary to the device in three steps:

1. **AppPushBegin** — opens a transfer session and tells the device the total file size in bytes.
2. **AppPushData** — sends the binary in chunks, each carrying its byte offset within the file. The host sends one command per chunk and waits for `OK` before sending the next.
3. **AppPushEnd** — closes the transfer session, signalling that all chunks have been delivered.

### AppStart

Starts the previously downloaded WASM application on the device.

### AppStop

Stops the currently running application.

### GetDeviceIdentity

Queries the device for its identity. The device responds with a `WendyComDeviceIdentity` message containing three fields:

- `id` — a unique identifier for the device (e.g. a serial number or hardware ID).
- `name` — a short machine-readable name.
- `display_name` — a human-readable display name.

If the device cannot provide an identity, it returns an error result and the host discards the device from its list of reachable devices.

### GetDeviceInfo

Queries the device for information about its hardware and software. The device responds with a `WendyComDeviceInfo` message containing:

- `os` — the operating system name, typically `wendy-lite`.
- `os_version` — the operating system version.
- `cpu_architecture` — the CPU architecture.
- `target` — the SoC name (e.g. `esp32c6` or `esp32s3`).
- `wasm_app_support` — whether the device can run WASM applications.
- `native_app_support` — whether the device can run native applications.
- `board` — the board identifier (e.g. `esp32c6_generic` or `esp32s3_seeed_xiao_native`). Empty when the board is not known.
