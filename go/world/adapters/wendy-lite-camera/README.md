# Full-resolution stationary camera firmware

Private device-specific overlay for Wendy Lite v0.15.0, commit
`6b8bc11b0ee5c48c7777136de2ccc684dbec4325`. The user's Seeed XIAO ESP32-S3 Sense
was updated through Wendy native OTA over USB on November 22, 2026.

`full-resolution.patch` requests the camera driver's largest supported OV
resolution. The driver clamps this request to the detected sensor's maximum
before allocating buffers. The SensorLink manifest reads the initialized size
instead of advertising VGA. JPEG quality is 12, compared with the release's 24.
The physical camera selected 2048×1536; this is not a claim that every XIAO sensor
supports that size. Other board implementations retain their existing dimensions.

Build in an isolated Wendy Lite checkout using ESP-IDF 6.0.3 and the
`boards/seeed_xiao_esp32s3_native.defaults` overlay. Apply the patch with
`git apply`, retain the release's 8 MB OTA partition layout, and set a distinct
application version such as `0.15.0-world-fullres`. The verified build and original
release rollback image are retained outside Git in
`~/Library/Caches/wendy-world/camera-hires-20261122/`.

For this CLI version, temporarily hide root `Package.swift` in the isolated
checkout so ESP-IDF project detection wins. Add a local `wendy.json`:

```json
{"appId":"world-camera-hires","version":"0.15.0-world-fullres","platform":"wendy-lite"}
```

After confirming the USB selector belongs to the intended device, deploy with
`wendy --json run --prefix <checkout> --device <selector> --detach --yes --skip-cloud-registration`.
This writes the inactive app slot and reboots; a full-device install is not
required. Check the reported camera dimensions and decode actual received JPEGs
after deployment. The original Wendy Lite working tree remains unchanged; the overlay stays private.

The [physical verification](../../docs/verification/camera-twin-2026-11-22/full-resolution.json)
records 16 decoded 2048×1536 samples from one source session. The subsequent
alignment retry still rejected the current view. Larger images can reduce frame
rate; complex images can exceed the driver's bounded JPEG buffer. Neither
long-duration reliability nor physical localization accuracy is established.
