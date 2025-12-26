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
            "mode": "host"
        },
        ...
    ]
}
```

## Network

The network entitlement allows the container to access the device's network. If the device is connected to WiFi, Ethernet or otherwise, the container will have access to make TCP and UDP connections to the internet.

A "network" type entitlement can have the following values:

- **mode**: A string representing the network type. Can be `host` (default) or `none`.

> Note: NetworkMode `none` does not support remote debugging.

```json
{
    "type": "network",
    "mode": "host"
}
```

## Video

The video entitlement allows the container to access V4L2 devices (e.g., webcams).

A "video" type entitlement can have the following fields:

- **mode**: `all` or `whitelist` (default `whitelist`)
- **devices**: List of device paths used when `mode` is `whitelist`

```json
{
    "type": "video",
    "mode": "all"
}
```

```json
{
    "type": "video",
    "mode": "whitelist",
    "devices": ["/dev/video0", "/dev/media0"]
}
```

## Audio

The audio entitlement allows the container to access host audio.

- Always mounts `/dev/snd` for ALSA access.
- If a PipeWire or PulseAudio runtime socket is present, it is bind-mounted and
  environment variables are set for client discovery.

```json
{
    "type": "audio"
}
```

## Device

The device entitlement allows the container to access the device's hardware.

## Mounts

The mounts entitlement allows the container to access the device's filesystem.

## USB

The USB entitlement allows the container to access USB devices.
