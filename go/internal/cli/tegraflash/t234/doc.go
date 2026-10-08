// Package t234 flashes a supported Jetson Orin (T234) over USB recovery from
// the builder's signed schema-v2 recovery flashpack on macOS, Linux, and Windows. It does
// not run NVIDIA host binaries, Docker, or a bundle-side preparation script.
//
// Stage 1 RCM-boots the signed chain declared by the flashpack. The booted
// initrd then exposes "flashpkg", the selected rootfs device, and a final
// "flashpkg" status LUN. The host verifies device.json before handoff, writes
// the partition layout with a native Go GPT/filesystem writer, and accepts the
// operation only after the device reports SUCCESS. Raw block access is isolated
// in the hidden privileged `wendy __t234-write` helper.
//
// Single enumeration is negotiated through an optional device.json capability.
// Unmarked packages remain in legacy mode with old initrds. With a capable
// initrd the host uses go-diskfs to opt a private package copy into single mode;
// already-selected packages bypass that editor. Mode stays fixed after handoff:
// only legacy mode may use a USB-disconnect fallback.
//
// The device side of this protocol is meta-tegra's tegra-initrd-flash
// initramfs (recipes-core/initrdscripts/tegra-flash-init/init-flash.sh);
// the LUN exchange details in this package track that script.
package t234
