# WendyOS Hardware Requirements

## Minimum requirements

| Component | Minimum | Notes |
|-----------|---------|-------|
| CPU architecture | arm64 or x86_64 | |
| RAM | 1 GB | 4 GB recommended for ML workloads |
| Storage | 64 GB | NVME or SD card. See [Storage](#storage) below. |
| Network | Any | Required for cloud connectivity and OCI image pulls |

## Storage

WendyOS requires at least **64 GB** of storage. This covers the OS partitions (root A/B for Mender updates), the config partition, and enough headroom for container images and persistent data.

A larger drive is not required by WendyOS itself, but running large container images (for example, ML inference containers with bundled model weights) can consume significant space. If your workload pulls images above a few gigabytes, a 128 GB or larger drive is recommended.

The 64 GB minimum applies to removable NVME and SD card installations. On Jetson Orin Nano and Jetson AGX Orin, the `mender_data` partition auto-expands on first boot to fill any remaining space, so a larger drive is automatically used. Jetson AGX Thor is flashed over USB recovery to its internal storage instead of to a removable target drive.

### Partition sizes at a glance

| Device | Root FS (A+B) | Config | Other |
|--------|--------------|--------|-------|
| Raspberry Pi (SD / NVME) | 8 GB | 64 MB | — |
| Jetson Orin Nano / AGX Orin (SD / NVME) | 8 GB + 8 GB | 64 MB | 512 MB mender_data (auto-expands) |
| Jetson AGX Thor | Flashpack-managed internal partitions | N/A | Flashed over USB recovery; no external SD/NVME target |

See the device-specific pages for full partition layouts:
- [WendyOS on Raspberry Pi](pi/README.md)
- [WendyOS on Jetson](jetson/README.md)

## Supported devices

### Raspberry Pi

| Device | Boot media |
|--------|-----------|
| Raspberry Pi 5 | SD card, NVME |
| Raspberry Pi 4 | SD card |
| Raspberry Pi 3 | SD card |

### NVIDIA Jetson

| Device | Boot media |
|--------|-----------|
| Jetson Orin Nano | SD card, NVME |
| Jetson AGX Orin | eMMC, NVME |
| Jetson AGX Thor | Internal flash + NVME via USB recovery |
| DGX Spark | Install `wendy-agent` on the existing Linux system (no image flash needed) |

### x86-64 machines (Intel & AMD)

Any 64-bit Intel or AMD PC, workstation, or server runs Wendy by installing `wendy-agent` on your existing Linux distribution (Ubuntu, Fedora, Arch, etc.) — no WendyOS image flash required. This is the same install path used for DGX Spark.

| Machine | Setup |
|---------|-------|
| Intel x86-64 (Core, Xeon) | Install `wendy-agent` on existing Linux |
| AMD x86-64 (Ryzen, EPYC) | Install `wendy-agent` on existing Linux |

See [Install wendy-agent on Linux](/docs/installation/linux).

## GPU acceleration

| GPU | Status |
|-----|--------|
| NVIDIA (CUDA) | Supported — Jetson and x86 systems with NVIDIA GPUs, via the `gpu` entitlement |
| AMD (ROCm) | Supported on compatible x86 Linux hosts with `amdgpu`, via the `gpu` entitlement |

See [GPU Access](/docs/hardware/gpu).
