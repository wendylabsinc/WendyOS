# Logging and Debugging

WendyOS uses systemd-journald for log management. All services write to the journal via stdout/stderr or `logger`. This document covers how journald is configured, how to enable persistent storage, and how to access logs.

## Default behaviour

Whether journal logs survive a reboot depends on `WENDYOS_PERSIST_JOURNAL_LOGS`, whose default tracks `WENDYOS_OTA`. Boards built with the `wendyos-update` OTA stack (`WENDYOS_OTA = "wendy"`) have a `/data` partition and keep persistent logs there. QEMU builds (`WENDYOS_OTA = "none"`) and Raspberry Pi, which sets the option to `0` for now, store logs in volatile memory (`/run/log/journal`), where they are lost on reboot.

## Persistent journal option

To persist logs across reboots, set `WENDYOS_PERSIST_JOURNAL_LOGS=1` in your build configuration (`local.conf` or the distro conf). This causes two extra files to be installed:

**`/usr/lib/systemd/journald.conf.d/10-wendyos-persistent.conf`**

```ini
[Journal]
Storage=persistent
SystemMaxUse=512M
SystemKeepFree=1G
RuntimeMaxUse=64M
MaxRetentionSec=1month
Compress=yes
ForwardToSyslog=yes
```

**`/lib/systemd/system/var-log.mount`**

Bind-mounts `/data/log` onto `/var/log`:

```ini
[Mount]
What=/data/log
Where=/var/log
Type=none
Options=bind,x-systemd.mkdir
```

The `x-systemd.mkdir` option automatically creates `/data/log` if it does not exist. The unit requires `/data` and runs after `data.mount`; `wendyos-data.service` prepares the data partition (first-boot format and grow) before `data.mount`. journald starts early with the volatile runtime journal, and `systemd-journal-flush.service` moves it to `/var/log/journal` once this mount is in place.

When `Storage=persistent` is set, journald creates `/var/log/journal/<machine-id>/` and writes binary journal files there. These survive reboots and OTA updates (the `/data` partition is outside the update boundary on Tegra machines).

### Enabling persistent logging at runtime (without a rebuild)

On a running device you can force persistence without a full image rebuild:

```sh
mkdir -p /var/log/journal
systemd-tmpfiles --create --prefix /var/log/journal
systemctl restart systemd-journald
```

This only persists until the next OTA update that does not include the persistent-logging configuration.

## Accessing logs

### Follow a service in real time

```sh
journalctl -fu wendyos-agent.service
journalctl -fu gadget-setup.service
journalctl -fu wendyos-etc-binds.service
```

### View logs since last boot

```sh
journalctl -b
```

### View logs from a previous boot (requires persistent storage)

```sh
# List available boots
journalctl --list-boots

# Show logs from boot index -1 (previous boot)
journalctl -b -1
```

### Filter by systemd identifier (syslog tag)

```sh
journalctl -t wendy-agent-updater
journalctl -t rpi5-eeprom-update
journalctl -t gadget-setup.sh
```

### View all WendyOS service logs together

```sh
journalctl -u "wendyos-*" -u gadget-setup.service
```

### Check journal disk usage

```sh
journalctl --disk-usage
```

### Vacuum old logs

```sh
# Keep only the last 7 days
journalctl --vacuum-time=7d

# Keep only 200 MB
journalctl --vacuum-size=200M
```

## Log storage locations

| Configuration | Location | Survives reboot |
|---------------|----------|-----------------|
| `WENDYOS_PERSIST_JOURNAL_LOGS=0` | `/run/log/journal/` | No |
| `WENDYOS_PERSIST_JOURNAL_LOGS=1` | `/var/log/journal/` (→ `/data/log/journal/`) | Yes |

## Individual service log identifiers

| Service | syslog identifier / unit |
|---------|--------------------------|
| wendy-agent | `wendyos-agent.service` |
| agent updater | `wendy-agent-updater` |
| agent downloader | `wendy-agent-download` |
| USB gadget setup | `gadget-setup.sh` |
| UUID generation | `wendyos-uuid-generate.service` |
| Device name | `wendyos-device-name` |
| etc bind mounts | `wendyos-etc-binds` |
| EEPROM update | `rpi5-eeprom-update` |
| NVMe EEPROM | `rpi5-eeprom-nvme-update` |
| NetworkManager | `NetworkManager` |

## Kernel command line and console

By default, WendyOS configures two kernel consoles:

- `console=serial0,115200` — hardware UART (see [uart.md](uart.md))
- `console=tty1` — HDMI framebuffer

Kernel messages and early boot output appear on both. Systemd redirects unit output to the journal after early boot.

To see kernel messages in the journal:

```sh
journalctl -k          # current boot
journalctl -k -b -1    # previous boot
```
