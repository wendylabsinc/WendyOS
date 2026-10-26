// Package realsense reads Intel RealSense D400-series factory calibration
// natively — the HWM (hardware monitor) vendor protocol over a UVC extension
// unit — with no vendor SDK, no cgo and no new dependencies.
//
// This is the vendor-specific drawer: everything that knows Intel's byte
// layouts lives here, behind plain Go types, so the rest of the agent stays
// vendor-blind. The protocol was ported from Intel librealsense v2.59.1
// (Apache-2.0) and validated byte-for-byte against a D435i (fw 5.15.1.55) on
// 2026-10-12; specs/2026-10-11-realsense-calibration-protocol-notes.md is the
// normative byte-level reference, and testdata/ holds raw reply frames
// captured from that camera.
//
// Two hard-won rules govern this package:
//
//   - Gate on content, never on sizeof. Table sizes vary by firmware (the
//     D435i's fw 5.15.1.55 returns table 25 as 512 bytes where the SDK struct
//     says 576); parsers trust the header's version, table_size and CRC plus
//     the offsets they actually read.
//   - Control requests are budgeted, never retried in a loop. A storm of
//     failed UVC control retries once wedged a Jetson's whole USB controller
//     (tegra-xusb, taking the robot's WiFi with it). Each exchange makes one
//     bounded attempt sequence and then fails; callers refuse, they do not
//     hammer.
//
// Portions derived from librealsense src/ds/ds-private.h and
// src/ds/d400/d400-private.h, Copyright(c) 2015 Intel Corporation,
// licensed under the Apache License, Version 2.0.
package realsense
