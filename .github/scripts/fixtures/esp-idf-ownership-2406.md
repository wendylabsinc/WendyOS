# Rootful ESP-IDF output ownership

The #2406 ownership fix is `05d68a344c7a5791d3cf7f9380862ddb447b4690`.
The Linux fixture calls the actual project loader, Docker detection, IDF runner
and ownership restoration. It uses the digest-pinned image shipped for Mosaico,
but compiles a minimal ESP32-C6 app: it does not flash hardware or claim to build
the Mosaico application.

Run in an existing native Linux job as its ordinary non-root user, with Go,
Python 3 and access to the existing rootful Docker daemon:

```sh
python3 .github/scripts/test-esp-idf-ownership.py --output "$RUNNER_TEMP/esp-idf-ownership"
```

The CMake configure phase records UID 0. Success must produce a RISC-V ELF with
an entry point and an ESP app image. All generated build/configuration paths
checked by the fixture must belong to the invoking user after return. A root-owned
unrelated source, an external symlink target and a file owned by another UID must
keep their owners. Separate fresh projects fail configuration and cancel a live
configuration command; both must restore ownership too.

A second executable uses a Go source overlay to omit only the call to ownership
restoration. The same successful firmware build must then fail its ownership
assertions. Build errors, skips, timeouts and cleanup errors cannot satisfy that
negative control. The overlay does not modify the checkout.

The existing containerd job is a suitable host if the Linux install job has no
remaining time/disk budget. No new runner is needed. The amd64 image has 2.23 GiB
of compressed layers; allow 8–12 GiB including extraction and build outputs.
The image pull is bounded at ten minutes, each configure/build at three minutes,
each test compilation at three minutes, and command supervision adds bounded
cleanup time. Allow 35 minutes for the entire gate; ordinary runs should be much
shorter. `evidence.json` records actual command durations so the budget can be
tightened after hosted execution. Keep its logs in the existing PR evidence
artifact; there are no release artifacts from this fixture.

The driver cleans only uniquely named containers whose bind mount is inside its
private workspace, then restores that workspace for removal. It neither removes
the shared image nor changes Docker contexts, builders or caches. Unknown
ownership or cleanup failures retain the workspace and fail the gate.

Portable cleanup checks are in `esp_idf_ownership_test.py`. Native macOS can
cross-compile the Linux test and validate the IDF project in an ordinary
container, but it cannot establish rootful Linux host ownership. Keep #2406
pending until the normal and negative-control hosted journeys execute.
