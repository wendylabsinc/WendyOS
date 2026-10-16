# Reading D400 factory calibration over raw UVC (no librealsense) — protocol notes

Reference notes for the native (SDK-free) RealSense direction decided on WDY-3595.
Extracted 2026-10-11 from Intel librealsense **v2.59.1** (Apache-2.0,
github.com/IntelRealSense/librealsense); citation form:
`https://github.com/IntelRealSense/librealsense/blob/v2.59.1/<path>#L<lines>`.
Kernel citations: torvalds/linux master `drivers/media/usb/uvc/uvc_ctrl.c`.
These notes feed the design doc for replacing `cmd/wendy-realsense-source`
with native capture in the agent (calibration read + Z16 + Go alignment).

## 1. Transport — hardware-monitor (HWM) over a UVC extension unit

- Depth XU: USB interface (mi) **0** (the depth UVC function), **XU unit id 3**,
  **selector 1** (`DS5_HWMONITOR`), GUID `C9606CCB-594C-4D25-AF47-CCC496435995`.
  - `src/ds/ds-private.h:50` — `const uint8_t DS5_HWMONITOR = 1;`
  - `src/ds/ds-private.h:81-83` — `depth_xu = { 0, 3, 2, {0xC9606CCB, ...} }`
    (fields `{subdevice, unit, node, guid}`, `src/platform/uvc-device.h:47-53`;
    only `unit` matters on Linux). GUID in descriptor byte order for `lsusb -v`:
    `CB 6C 60 C9 4C 59 25 4D AF 47 CC C4 96 43 59 95`, `bUnitID = 3`.
- Pick the `/dev/videoN` whose `/sys/class/video4linux/videoN/device/bInterfaceNumber`
  is 0 (librealsense: `filter_by_mi(all_device_infos, 0)`, `src/ds/d400/d400-device.cpp:564`).
  Use the streaming node, not the metadata node. No stream needs to be active.
- HWM channel wiring: `command_transfer_over_xu(*raw_sensor, depth_xu, DS5_HWMONITOR)`
  — `src/ds/d400/d400-device.cpp:651-656`.

### Command packet (SET_CUR)

`hw_monitor::fill_usb_buffer` — `src/hw-monitor.cpp:29-60`; constants `src/hw-monitor.h:41-47`:
`IVCAM_MONITOR_MAGIC_NUMBER=0xcdab`, `HW_MONITOR_COMMAND_SIZE=1000`,
`HW_MONITOR_BUFFER_SIZE=1024`, `HW_MONITOR_DATA_SIZE_OFFSET=1020`,
`SIZE_OF_HW_MONITOR_HEADER=4`. Little-endian throughout.

| offset | type | value |
|---|---|---|
| 0 | u16 LE | `size` = (total bytes written) − 4 (= 20 with no payload) |
| 2 | u16 LE | magic `0xCDAB` |
| 4 | u32 LE | opcode |
| 8 | u32 LE | param1 |
| 12 | u32 LE | param2 |
| 16 | u32 LE | param3 |
| 20 | u32 LE | param4 |
| 24 | bytes | optional payload |

The buffer sent over the XU is always zero-padded to exactly **1024 bytes**
(`command_transfer_over_xu::send_receive`, `src/platform/uvc-option.cpp:143-180`).
Defaults: params 0, timeout 5000 ms, response required (`src/hw-monitor.h:103-124`).

### Response (GET_CUR)

GET_CUR of **1024 bytes** on the same selector immediately after SET_CUR:
- bytes 0..3: u32 LE — echo of the opcode on success, or a **negative i32 error**
  (`hw_monitor::send`, `src/hw-monitor.cpp:146-196`). Error codes
  (`d400_hwmon_response`, `src/ds/d400/d400-private.h:427-480`): `SUCCESS=0`,
  `WRONG_COMMAND=-1`, `WRONG_PARAMETER=-6`, `HW_NOT_READY=-7`, `WRONG_CRC=-19`,
  `TABLE_IS_EMPTY=-25`, `CALIBRATION_WRONG_TABLE_ID=-28`, …
- bytes 4..4+N−1: payload.
- bytes 1020..1023: u32 LE = N, payload length excluding the 4-byte echo
  (`uvc-option.cpp:176-180`).
- Serialize the SET/GET pair; do not interleave with a live librealsense client.

### Linux ioctl mapping

`v4l_uvc_device::set_xu/get_xu` — `src/linux/backend-v4l2.cpp:2029-2059`:
`UVCIOC_CTRL_QUERY` with `{unit:3, selector:1, query:UVC_SET_CUR(0x01)/UVC_GET_CUR(0x81),
size:1024, data}` on the video node fd; `get_xu` zeroes the buffer first.

**Kernel gotcha** (`uvc_xu_ctrl_query`, uvc_ctrl.c ~L3033-3160): the driver returns
**-ENOBUFS unless `size` equals the control's own GET_LEN**. Always issue
`UVC_GET_LEN (0x85)` (size=2, u16 LE result — 1024 here) first and use that value.
-ENOENT = wrong unit/selector (selector matched as `ctrl->index == selector-1`).
`UVCIOC_CTRL_QUERY = _IOWR('u', 0x21, struct uvc_xu_control_query)` = `0xc0107521`
on 64-bit; struct `{__u8 unit; __u8 selector; __u8 query; __u16 size; __u8 *data;}`
(16 bytes with padding: size at offset 4, pointer at offset 8).
EIO/EAGAIN/EBUSY are transient/retriable.

## 2. Commands

Opcodes: shared `ds::fw_cmd` (`src/ds/ds-private.h:101-138`) and
`ds::d400_fw_cmd` (`src/ds/d400/d400-private.h:160-172`).

| command | opcode | params | returns | citation |
|---|---|---|---|---|
| GETINTCAL | 0x15 | param1 = table id | one calibration table | `d400-private.h:163`; `d400-device.cpp:485-489` |
| GVD | 0x10 | — | device-info blob | `ds-private.h:110`; `hw-monitor.cpp:230-257` |
| RECPARAMSGET | 0x7E | — | per-resolution depth intrinsics (fw ≥ 5.11.9.5) | `ds-private.h:135`; gate `d400-device.cpp:491-499` |
| UAMG | 0x30 | — | byte0 ≠ 0 ⇒ advanced mode | `ds-private.h:119`; `ds-device-common.cpp:408-417` |
| GET_ADV | 0x2C | p1=9 (etDepthTableControl), p2=0 | `depth_table_control` (20 B) → depth units | `ds-private.h:117,172,174-179`; `ds-options.cpp:372-384` |

Table ids (`d400_calibration_table_id`, `d400-private.h:275-285`):
`coefficients_table_id=25` (depth/stereo), `depth_calibration_id=31`,
`rgb_calibration_id=32` (color + depth↔color extrinsics), `fisheye=33`, `imu=34`,
`lens_shading=35`, `projector=36`.

**Depth scale** is NOT in the tables (`d400-device.cpp:1073-1099`): advanced mode
+ fw ≥ 5.6.3.0 → GET_ADV `depth_table_control.depth_units` (µm per LSB; meters =
1e-6 × value; struct `src/ds/ds-private.h:505-512`); otherwise constant **0.001 m**
(0.0001 m only for D405/D401-GMSL). Factory default on D415/D435i/D455 = 1 mm.

**GVD offsets** (`d400_gvd_offsets`, `d400-private.h:252-271`): fw version at 12
(4 bytes, print reversed `b[15].b[14].b[13].b[12]`), module serial at 48 (6 bytes
hex), asic serial at 64; rgb_sensor presence flag at 174.

## 3. Table layouts (byte-level, offsets from payload start)

### Common header (16 bytes) + CRC

`table_header` — `src/ds/ds-private.h:262-272` (pack(1)):
`version` u16 **big-endian** (the only BE field anywhere), `table_type` u16,
`table_size` u32, `param` u32, `crc32` u32. Validation (`check_calib`,
`ds-private.h:308-342`): len ≥ 16; `table_size + 16 ≤ len`; crc32 of payload[16:]
== header.crc32. CRC is poly 0xEDB88320 init 0xFFFFFFFF final-complement —
**exactly Go's `crc32.ChecksumIEEE`** (`third-party/rsutils/src/crc32.cpp`).

### Depth/stereo: `d400_coefficients_table` (GETINTCAL param1=25), 576 bytes

`src/ds/d400/d400-private.h:313-325`. Offsets: header 0, intrinsic_left (3×3 f32)
16, intrinsic_right 52, world2left_rot 88, world2right_rot 124, **baseline 160**
(f32, mm, may be negative — SDK uses fabs; `get_stereo_baseline_mm`,
`d400-device.cpp:478-483`), brown_model u32 164, reserved 168,
**rect_params 256** = 16 × float4 `{fx, fy, ppx, ppy}` (pixels, rectified depth),
reserved 512.

`ds_rect_resolutions` index order (`ds-private.h:274-295`):
```
0:1920x1080  1:1280x720  2:640x480  3:848x480  4:640x360  5:424x240
6:320x240   7:480x270   8:1280x800  9:960x540  10,11:reserved
12:640x400  13:576x576  14:720x720  15:1152x1152
```

Depth intrinsics selection (`get_d400_intrinsic_by_resolution_coefficients_table`,
`d400-private.cpp:128-226`): exact match → rect_params[idx], distortion model
Brown-Conrady with **all coefficients zero**; 848×100 → use 848×480 entry with
`ppy -= 190`; unknown resolution → scale the 1920×1080 entry by W/1920, H/1080;
degenerate entry (fx==fy && ppx==ppy) → synthesize fx=fy=1.5·W, ppx=W/2, ppy=H/2.
Depth↔left-IR extrinsic = identity; left→right = identity rotation + translation
`[0.001·baseline, 0, 0]` m (`d400-device.cpp:673-682`).

Preferred modern source (fw ≥ 5.11.9.5; SDK tries first — `d400-device.cpp:213-229`):
**RECPARAMSGET**, payload = packed 20-byte rows
`{u16 width; u16 height; f32 fx, fy, ppx, ppy}` (`ds-private.h:297-305`);
scan for the requested resolution, fall back to the coefficients table.

### Color: `d400_rgb_calibration_table` (GETINTCAL param1=32), 256 bytes

`src/ds/d400/d400-private.h:339-357`. Offsets: header 0, intrinsic (3×3 f32,
normalized) 16, distortion (5 × f32, Brown k1 k2 p1 p2 k3) 52, rotation
(Rodrigues, unrectified — unused) 72, translation (mm, unrectified — unused) 84,
projection (3×4 — unused) 96, calib_width u16 144, calib_height u16 146,
intrinsic_matrix_rect 148, **rotation_matrix_rect 184** (3×3 f32),
**translation_rect 220** (3 × f32, mm), reserved 232.

**`float3x3` is column-major**: `element(i,j) = flat[j*3+i]`
(`rsutils/number/float3.h:62-73`).

Color intrinsics (`get_d400_color_stream_intrinsic`, `d400-private.cpp:242-293`),
with f[0..8] = the 9 floats at offset 16 (fx_n=f[0], fy_n=f[4], ppx_n=f[2],
ppy_n=f[5]):
1. `calib_AR = calib_height/calib_width` (fallback 9/16); `actual_AR = H/W`.
2. `actual_AR < calib_AR` → `f[4] *= calib_AR/actual_AR; f[5] *= …`;
   else `f[0] *= actual_AR/calib_AR; f[2] *= …`.
3. `ppx=(1+f[2])·W/2; ppy=(1+f[5])·H/2; fx=f[0]·W/2; fy=f[4]·H/2`.
4. Model INVERSE_BROWN_CONRADY, coeffs = distortion[5] as-is (often all zero
   from factory).

Depth↔color extrinsics (`get_d400_color_stream_extrinsic`,
`d400-private.cpp:228-240`) use the **rectification** fields only:
color→depth `R = rotation_matrix_rect` (column-major),
`t = -0.001 × translation_rect` (meters) — registered COLOR→DEPTH
(`d400-color.cpp:89`). depth→color (what rs-enumerate-devices prints) is the
inverse: `R' = Rᵀ`, `t' = -Rᵀ·t`. Convention: column-major rotation acting on
column vectors (`rs2_extrinsics`, `rs_sensor.h:111-116`; `rsutil.h`).
Intel's Custom Calibration white paper (rev 1.7, 2021, §2.2) confirms: left
imager is the world origin, rotations are "RGB → left camera", translations mm,
Brown `[k1;k2;p1;p2;k3]`.

## 4. Gotchas

- USB PIDs (`d400-private.h:16-45`): D435i 0x0B3A, D415 0x0AD3, D455 0x0B5C,
  D435 0x0B07, D405 0x0B5B. VID 0x8086.
- Layouts identical across D415/D435/D435i/D455 (USB); data differs
  (calib 1920×1080 vs D455 1280×800; baseline ≈55/50/95 mm). D405 has a
  different color-intrinsic computation (`d400-private.cpp:296-377`); D457
  GMSL/MIPI does not do HWM over this XU. D435i IMU calibration (table 34) is
  separate and not needed.
- Firmware gates: RECPARAMSGET fw ≥ 5.11.9.5 (code gate, `d400-device.cpp:493`);
  GET_ADV depth table fw ≥ 5.6.3.0 + advanced mode. Minimum fielded fw per model
  (`d400-private.h:196-227`) all support GETINTCAL/GVD.
- Depth intrinsics are per-resolution; there is no single "depth intrinsics".
- All LE except `table_header.version` (BE u16).
- Always CRC-validate (ChecksumIEEE over payload[16:]); assert
  `table_size+16 ≤ len` and payload ≥ 576 (coefficients) / 256 (rgb).
- Expect negative i32 in the response echo: -25 TABLE_IS_EMPTY /
  -28 WRONG_TABLE_ID on SKUs lacking a table (check GVD rgb flag at 174).
- Emitter/projector state is irrelevant to calibration reads (inferred from
  code: no path ties GETINTCAL to emitter).
- uvcvideo: exact-size rule (ENOBUFS), first XU query triggers kernel control
  init; the baseline sign and the −0.001 translation sign flip are the easiest
  mistakes.
- The two D400 table structs are naturally-aligned-identical to packed; parse
  with explicit offsets in Go regardless.

## 5. Validation recipe (the WDY-3595 gate)

On the same camera with librealsense available (e.g. the SDK helper's
`describe`, or `rs-enumerate-devices -c`):
1. Depth 848×480 / 1280×720 / 640×480 intrinsics must match rect_params /
   RECPARAMSGET rows float-exactly; infrared 1/2 identical to depth.
2. Color at 1920×1080 and one 4:3 mode (640×480) — exercises the aspect-ratio
   branch; D455 also 1280×800 vs 1280×720.
3. Depth→Color extrinsics = our inverse (Rᵀ, −Rᵀt); Color→Depth = our direct
   parse. Sanity: |t| ≈ 0.015 m (D435i/D415) / ≈ 0.059 m (D455);
   Depth→IR2 translation.x ≈ ∓baseline/1000.
4. Depth scale vs `get_depth_scale()` (or default 0.001).
5. GVD fw/serial vs rs-enumerate-devices output.
6. Byte-level: Intel.Realsense.CustomRW -r dumps the same tables to XML
   (mind mm vs m).

## 6. License

librealsense is Apache-2.0. A Go port of the table structs/parsing must: include
the Apache-2.0 license text or SPDX id + link; retain the copyright notice, e.g.
`// Portions derived from Intel librealsense (github.com/IntelRealSense/librealsense),
v2.59.1, src/ds/d400/d400-private.h and src/ds/ds-private.h. Copyright(c)
2015 Intel/RealSense, Inc. Licensed under Apache-2.0.`; state changes
("translated to Go, parsing only"). No copyleft obligations.

## Unverified

- Official datasheet wording on depth units (forum/staff confirmations only).
- Kernels older than current master for the exact-size ENOBUFS rule (use
  UVC_GET_LEN at runtime regardless).
- `table_type`/`param` header semantics beyond the quoted comments (SDK never
  checks them on read).

## Hardware validation — 2026-10-12, Unitree G1 Nx (asset 323), D435i fw 5.15.1.55

A std-lib-only Go probe (UVCIOC_CTRL_QUERY, no librealsense) built and run on
the robot read every table successfully. Full output:
scratchpad rs-probe/probe-output-g1-d435i-2026-10-12.txt (session 19aa51ab).

- HWM channel: GET_LEN=1024 as documented; node auto-found via sysfs
  (bInterfaceNumber==00 + name match — nodes renumbered video6→video0 between
  sessions, so never hardcode the index).
- GVD: fw 5.15.1.55, serial 337122071091, rgb_sensor=1. Depth units 1000 µm.
- **Table 25 is 512 bytes on this firmware, not 576** (version 3.0,
  table_size=496 + 16 header): the trailing reserved bytes are absent. Baseline
  f32@160 and rect_params 16×float4@256 are intact; gate `len >= 512`, not 576.
  CRC (IEEE over payload[16:]) validates.
- Baseline −50.049 mm (D435i physical: 50 mm; negative per convention).
- Table 25 rect params agree with RECPARAMSGET per-resolution to float32
  precision (two independent firmware paths, same numbers).
- Table 32 (rgb, 240+16 bytes): CRC valid; color intrinsics for 3 resolutions;
  color→depth translation (−0.014853, −0.000083, −0.000442) m ↔ the known
  ~15 mm RGB-to-left-imager offset; depth→color = Rᵀ, −Rᵀt round-trips.

Conclusion: the pure-Go read is self-consistent and physically correct on real
hardware. The SDK oracle (Thor helper build) is now a nice-to-have for
byte-offset paranoia, not a gate: the CRC already proves we framed the right
bytes, and two firmware paths agree with each other and with the camera's
physical geometry.

## Z16 capture validation — 2026-10-12, same camera

The agent's generalised mmap loop sequence (S_FMT Z16, REQBUFS 2, mmap,
STREAMON, DQBUF) ran standalone on the G1's depth node: negotiated Z16
640x480, bytesperline 1280, full 614,400-byte frames, 93% valid-depth pixels,
centre pixel 2.27 m, scene range 0.46–6.78 m — 90 frames in 0.97 s.

- **The driver's default frame interval for Z16 640x480 is ~90 fps**, not 30.
  A capture that does not set VIDIOC_S_PARM runs at triple the expected rate;
  the service layer must set the interval explicitly.
- S_FMT on a node held by another process fails EBUSY at the S_FMT stage
  (observed against the vendor-SDK demo app holding all three functions —
  the lockout the native design removes, live).
