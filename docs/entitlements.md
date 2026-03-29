# App Configuration

The app configuration is a JSON object that contains the app's configuration.
A minimal version looks like this:

```json
{
    "appId": "com.example.app",
    "version": "1.0.0",
}
```

The app configuration is stored in the `wendy.json` file in the root of the app's directory.

### Entitlements

Entitlements are a way to grant containers access to resources on the host. The format of the entitlements is a JSON object with the following fields:

```json
{
    "appId": "com.example.app",
    "version": "1.0.0",
    ...
    "entitlements": [
        {
            "type": "network",
            "network": "host"
        },
        ...
    ]
}
```

## Network

The network entitlement allows the container to access the device's network. If the device is connected to WiFi, Ethernet or otherwise, the container will have access to make TCP and UDP connections to the internet.

A "network" type entitlement can have the following values:

- **network**: A string representing the network type. Can be `host` (default) or `none`.

> Note: NetworkMode `none` does not support remote debugging.

```json
{
    "type": "network",
    "network": "host"
}
```

## Device

The device entitlement allows the container to access the device's hardware.

## Mounts

The mounts entitlement allows the container to access the device's filesystem.

## Input

The input entitlement allows the container to access HID input devices such as barcode scanners, keyboards, and other devices that appear under `/dev/input/`. This is separate from the USB entitlement — USB covers `/dev/bus/usb` (raw USB access), while input covers the higher-level Linux input subsystem.

```json
{
    "type": "input"
}
```

The container receives:
- A bind mount of `/dev/input/` (including `by-id/` symlinks for stable device identification)
- Membership in the `input` group (GID 105) for device permissions
- A cgroup device rule allowing access to input devices (major 13)

### Device discovery

Event device numbers (`/dev/input/event0`, `event1`, etc.) are assigned dynamically and can change across reboots. Use the stable symlinks under `/dev/input/by-id/` to identify devices reliably:

```
/dev/input/by-id/usb-USBKey_Chip_USBKey_Module_202730041341-event-kbd
```

### When to use input vs USB

| Entitlement | Access | Use case |
|-------------|--------|----------|
| `input` | `/dev/input/` (Linux input subsystem) | Reading HID events — barcode scanners, keyboards, game controllers |
| `usb` | `/dev/bus/usb` (raw USB) | Low-level USB communication — custom protocols, firmware updates, libusb |

Most USB HID devices (scanners, keyboards) should use `input`. You only need `usb` if your app talks raw USB protocols.

## USB

The USB entitlement allows the container to access USB devices.

## Persist

The persist entitlement allows the container to persist data across restarts. Data is stored on the host filesystem and mounted into the container at the specified path.

```json
{
    "type": "persist",
    "name": "my-volume",
    "path": "/mnt/data"
}
```

- **name**: A unique name for the volume. Volumes with the same name are shared across apps.
- **path**: The path inside the container where the volume is mounted.

### Shared Volumes

Volumes are identified by name only (not by app ID), so multiple apps can share data by using the same volume name. This is useful for sharing caches or data between apps.

### Recommended Shared Volume Names

| Name | Path | Description |
|------|------|-------------|
| `huggingface-cache` | `/app/.cache/huggingface` | Shared cache for Hugging Face models (transformers, datasets, etc.). Avoids re-downloading large ML models for each app. |

Example for a Python ML app:

```json
{
    "type": "persist",
    "name": "huggingface-cache",
    "path": "/app/.cache/huggingface"
}
```