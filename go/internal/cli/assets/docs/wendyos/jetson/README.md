# Jetson

WendyOS supports the following NVIDIA Jetson targets:

| Board | Machine config | SoC | JetPack / L4T | OTA | Yocto series |
|---|---|---|---|---|---|
| Jetson AGX Orin DevKit (NVMe) | `jetson-agx-orin-devkit-nvme-wendyos` | tegra234 | JetPack 7.2 / L4T 39.2.0 | wendyos-update | blacksail |
| Jetson AGX Orin DevKit (eMMC) | `jetson-agx-orin-devkit-emmc-wendyos` | tegra234 | JetPack 7.2 / L4T 39.2.0 | wendyos-update | blacksail |
| Jetson Orin Nano DevKit (NVMe) | `jetson-orin-nano-devkit-nvme-wendyos` | tegra234 | JetPack 7.2 / L4T 39.2.0 | wendyos-update | blacksail |
| Jetson Orin Nano DevKit (SD) | `jetson-orin-nano-devkit-wendyos` | tegra234 | JetPack 7.2 / L4T 39.2.0 | wendyos-update | blacksail |
| Jetson AGX Thor DevKit (NVMe) | `jetson-agx-thor-devkit-nvme-wendyos` | tegra264 | JetPack 7.2 / L4T 39.2.0 | wendyos-update | blacksail |

---

## AGX Thor (tegra264)

All Jetson boards — Orin (tegra234) and Thor (tegra264) — now build from the **blacksail** Yocto series in a split-poky composition, unified on JetPack 7.2 / L4T R39.2.0. Mender has been removed; all boards use the WendyOS `wendyos-update` OTA stack (`WENDYOS_OTA = "wendy"`). The notes below cover the tegra264-specific (Thor) differences that remain:

- **Flash format** — produces `tegraflash-tar` (a compressed tar of the tegraflash package) rather than a separate `tegraflash` artefact.
- **Bootloader** — uses NVIDIA prebuilt UEFI firmware (`tegra-uefi-prebuilt`).
- **Image name suffix** — oe-core master defaults `IMAGE_NAME_SUFFIX` to `.rootfs`, which would produce deployed symlinks such as `wendyos-image-${MACHINE}.rootfs.tegraflash-tar`. The Thor machine config explicitly sets `IMAGE_NAME_SUFFIX = ""` so the deployed symlink matches the expected name `wendyos-image-${MACHINE}.tegraflash-tar`, consistent with all other boards.

To bootstrap a Thor build:

```bash
./bootstrap.sh --board jetson-agx-thor
```

See `conf/template/boards/jetson-agx-thor/` in the `meta-wendyos` repo for the board template files.

---

## Secure boot policy

> The notes below describe the **current internal policy**, not a shipped, user-facing secure-boot flow. End-user secure-boot enablement is in progress; e-fuse burning remains planned; see [WDY-1237](https://linear.app/wendylabsinc/issue/WDY-1237) and [WDY-1238](https://linear.app/wendylabsinc/issue/WDY-1238) for the surrounding security tickets.

Burning device fuses to enforce secure boot is an irreversible hardware change. WendyOS will **not** burn fuses on user devices as part of normal flashing. Fuse burning is reserved for sensitive, real-world production deployments where the customer explicitly opts in.

Until a full secure-boot flow is shipped, the hardening priorities are:

- **Kernel hardening** — _planned, not yet shipped_.
- **Removal of SUID binaries from production images** — _planned, not yet shipped_ ([WDY-1238](https://linear.app/wendylabsinc/issue/WDY-1238)). Recent CVEs frequently leverage SUID binaries, so trimming them from production images is a near-term focus.
- **Image / binary signing** — _planned, not yet shipped_. Approach differs between Jetson (tegra fuse / NVIDIA tooling) and Raspberry Pi; coordination across boards is open.

SSH is already removed from production images. Whether a general-purpose shell can also be removed is being investigated; see [WDY-1238](https://linear.app/wendylabsinc/issue/WDY-1238).
