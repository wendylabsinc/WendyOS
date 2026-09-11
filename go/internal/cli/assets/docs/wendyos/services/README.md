# WendyOS systemd Services

WendyOS is a Yocto-based Linux distribution managed entirely by systemd. This document covers what runs on a WendyOS device, the startup order, and how services depend on one another.

## Service inventory

| Service | Unit file | Purpose |
|---------|-----------|---------|
| `wendyos-etc-binds.service` | `recipes-core/wendyos-etc-binds` | Bind-mounts `/data/etc/wendyos` and `/data/etc/NetworkManager/system-connections` over their `/etc` counterparts so identity and WiFi credentials survive OTA updates |
| `wendyos-uuid-generate.service` | `recipes-core/wendyos-identity` | Generates `/etc/wendyos/device-uuid` on first boot using `uuidgen` |
| `wendyos-device-name-generate.service` | `recipes-core/wendyos-identity` | Generates `/etc/wendyos/device-name` (e.g. `brave-dolphin`) from word lists |
| `wendyos-identity.service` | `recipes-core/wendyos-identity` | Updates the mDNS Avahi service record with the device UUID and name |
| `wendyos-hostname.service` | `recipes-connectivity/avahi` | Derives `hostname` from the persistent device name and sets it before Avahi starts |
| `gadget-setup.service` | `recipes-core/gadget-setup` | Configures the USB gadget composite device (NCM or ECM network + ACM serial console) using Linux configfs |
| `wendyos-usbgadget-unbind.service` | `recipes-core/gadget-setup` | Tears down the USB gadget on shutdown |
| `wendyos-agent.service` | `recipes-core/wendyos-agent` | Runs the `wendy-agent` binary; provides device management, gRPC API, WiFi control, and container orchestration. Ordered after the boot-time identity publisher and Avahi so an explicit rename can be re-asserted without a race. |
| `wendyos-agent-updater.service` | `recipes-core/wendyos-agent` | One-shot agent self-update (downloads from GitHub releases). Also stopped during `UpdateOS` to prevent cgroup SIGTERM propagation to the in-flight OTA (wendyos-update) process |
| `wendyos-agent-updater.timer` | `recipes-core/wendyos-agent` | Triggers the updater 5 min after boot, then daily at 03:00 with a random 30-min jitter. Stopped for the duration of any `UpdateOS` call so the updater cannot interrupt an in-flight OTA (wendyos-update) operation; re-started when the call returns |
| `containerd.service` | upstream (meta-virtualization) | Container runtime; `wendy-agent` requires it before starting |
| `var-lib-containerd.mount` | `recipes-core/systemd-mount-containerd` | Bind-mounts `/data/containerd` to `/var/lib/containerd` so container images persist on the data partition |
| `home.mount` | `recipes-core/systemd-mount-home` | Bind-mounts `/data/home` to `/home` |
| `var-log.mount` | `recipes-core/systemd-conf` | Bind-mounts `/data/log` to `/var/log` (only when `WENDYOS_PERSIST_JOURNAL_LOGS=1`) |
| `swapfile-setup.service` | `recipes-core/swapfile-setup` | Creates and activates a swap file on `/data` before `swap.target` |
| `rpi5-eeprom-config.service` | `meta-rpi-extensions/recipes-bsp/rpi-eeprom` | One-shot: sets `PSU_MAX_CURRENT=3000` in the RPi 5 EEPROM for reliable USB gadget operation; reboots to apply, then never runs again |
| `rpi5-eeprom-nvme-config.service` | `meta-rpi-extensions/recipes-bsp/rpi-eeprom` | One-shot: sets `PCIE_PROBE=1` and `BOOT_ORDER=0xf461` in the RPi 5 EEPROM for NVMe boot |
| `wendyos-mdns.service` | `recipes-connectivity/avahi` | Writes the Avahi service definition for `_wendyos._udp` discovery on port 50051 |
| `systemd-resolved.service` | upstream (systemd) | Optional stub DNS resolver; enabled by adding `resolved` to `DISTRO_FEATURES` at build time. When not enabled, NetworkManager manages `/etc/resolv.conf` directly. |
| systemd-networkd | upstream (meta-networking) | Manages all network interfaces (WiFi, USB gadget `usb0`) |
| avahi-daemon | upstream (meta-connectivity) | Publishes and resolves mDNS/DNS-SD records |
| serial-getty@ttyAMA0.service / serial-getty@ttyS0.service | `recipes-core/systemd` | Provides a login shell on the debug UART |

## Startup order

The diagram below shows the major ordering relationships. Solid arrows indicate `After=`/`Requires=`; dashed arrows indicate `Before=`/`Wants=`.

```
local-fs.target
   |
   +-- data.mount
   |      |
   |      +-- mender-systemd-growfs-data.service (Tegra only)
   |             |
   +-- wendyos-etc-binds.service ────────────────────────────────────+
          |                                                           |
          +-- wendyos-uuid-generate.service                          |
          |        |                                                  |
          +-- wendyos-device-name-generate.service                   |
                   |                                                  |
                   +-- (sysinit.target) ──────────────────────────────+
                                                                      |
                          var-lib-containerd.mount                    |
                          home.mount                    gadget-setup.service
                          [var-log.mount]                      |
                                |                              |
                          containerd.service            network.target
                                |                              |
                                +------------------------------+
                                             |
                                     wendyos-agent.service
                                      (multi-user.target)

wendyos-uuid-generate.service
wendyos-device-name-generate.service
      |
      +-- wendyos-hostname.service --> avahi-daemon.service --+
      +-- wendyos-identity.service ---------------------------+
                                                               |
                                                     wendyos-agent.service
```

## systemd target dependencies

| Target | WendyOS additions |
|--------|-------------------|
| `sysinit.target` | `wendyos-etc-binds.service`, `wendyos-uuid-generate.service`, `wendyos-device-name-generate.service` |
| `local-fs.target` | `var-lib-containerd.mount`, `home.mount`, `[var-log.mount]` |
| `swap.target` | `swapfile-setup.service` |
| `multi-user.target` | `wendyos-agent.service`, `wendyos-agent-updater.service`, `gadget-setup.service`, `wendyos-hostname.service`, `wendyos-identity.service`, `rpi5-eeprom-config.service`, `rpi5-eeprom-nvme-config.service` |
| `timers.target` | `wendyos-agent-updater.timer` |

## Key ordering rules

1. `/data` must be mounted before any bind mount or identity service runs.
2. `wendyos-etc-binds.service` must complete before UUID and device-name generation so the generated files land on the persistent `/data` volume.
3. `gadget-setup.service` runs before `systemd-networkd` so the `usb0` interface exists when networkd starts.
4. `containerd.service` must be running and `/var/lib/containerd` must be bind-mounted before `wendyos-agent.service` starts.
5. EEPROM services (RPi 5 only) run after `multi-user.target` and use a flag file (`/var/lib/wendyos/eeprom-updated`, `/var/lib/wendyos/eeprom-nvme-updated`) to guarantee they execute at most once.
6. `wendyos-agent.service` runs after `wendyos-identity.service` and `avahi-daemon.service`. This ensures `ReassertHostnameAdvertisement`, which restores an explicit `wendy device rename` hostname over boot-time TXT records, runs after the publisher instead of racing it. These are ordering-only dependencies; packaged Linux installs that do not provide the WendyOS units continue to start normally.

## Checking service status

```sh
# Overview of all WendyOS-specific services
systemctl status wendyos-*.service gadget-setup.service rpi5-eeprom-config.service

# Live journal for the agent
journalctl -fu wendyos-agent.service

# Check mount targets
systemctl status var-lib-containerd.mount home.mount
```

## Wendy Data storage

`WENDY_DATA_INGEST_URL` is the cloud DataIngestService endpoint sealed episodes are
uploaded to (`ingest.data.wendy.sh` for the dev data platform). It is required for
uploads: the enrolled cloud host does not serve this service, so there is no
fallback. Without it the agent logs once at startup that uploads are disabled and
sealed episodes stay queued in the local store, bounded by its quota. The device
authenticates with its enrolled asset certificate in the TLS handshake; no request
header carries identity.

`WENDY_DATA_TELEMETRY_URL` optionally redirects OpenTelemetry exports to the same
platform so device telemetry lands in its ClickHouse store. Unset, telemetry goes
to the enrolled cloud host as before.

`WENDY_DATA_DIR` sets the root directory for the device-local Episode store.
The production default is `/var/lib/wendy-agent/data/episodes`. Set it in the
agent service environment when Episodes should live on a larger data volume:

```sh
WENDY_DATA_DIR=/data/wendy-agent/data/episodes wendy-agent
```

The agent creates the directory on startup. Unprivileged developer agents that
cannot create the production default fall back to their platform user
configuration directory. WendyOS services should set an explicit path on the
persistent `/data` partition when the root filesystem is small.
