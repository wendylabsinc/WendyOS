# Go2 H264 runtime fixture, audit 1831

Run `bash .github/scripts/test-go2-h264-loopback.sh` in the existing Linux media
job, before its final IP-camera fixture/module cleanup. It uses the job-owned
v4l2loopback 0.15.4 module and creates only video128 with a fresh `ci-go2-*` label.
It refuses an existing video128. No new runner is needed.

The same existing job needs containerd, ctr, runc and busybox-static. Use the
existing containerd version setup if testing both versions; this gate starts
only a private daemon under a private mount/PID namespace with private /run.
Installing a package is not permission to replace/start the host daemon. The
existing media GStreamer dependencies include x264enc, h264parse, avdec_h264,
videotestsrc, fdsrc, fdsink and videoconvert. No remote image pull is used.

The normal path is:

- Generated red/green Annex-B access units at 640x360, wrapped in the timestamp,
  resolution and sequence CDR format documented by the physical Go2 observation.
- Production CDR parser and frame writer, actual H264 V4L2 loopback negotiation.
- Production native VideoService capture with mmap/UAPI handling, followed by
  independent GStreamer decoding of every captured frame's pixel samples.
- Actual camera-entitled OCI creation, persisted nonempty entitlement label,
  running-consumer reconciliation, unchanged production camera mounts/device
  rules, and the same native capture executable running inside the container.
  Both red and green phases must independently decode correctly.

Only the static test capture executable is added to the container spec. No
camera mount or cgroup rule is supplied by the test. The provider records the
real reconciliation result; it does not replace containerd metadata or tasks.
The synthetic publisher bypasses DDS and Manager discovery/scheduling, so those
are not claims of this gate. The service's native capture method is exercised;
this fixture does not add a second gRPC stream assertion.

The old CDR decoder must fail before kernel access, forcing MJPEG in the writer
must fail the kernel format observation, and removing the nonempty entitlement
sentinel must lose the actual running container consumer. An unrelated failure,
skip or compilation error cannot satisfy those control gates.

Each kernel invocation is limited to 90 seconds by the existing owned-process
guardian. Normal plus two kernel controls take at most 270 seconds, plus targeted
builds. Normal should complete in seconds. On uncertain signal/timeout teardown,
the owned node is retained for disposable-runner teardown; it is not removed
while the namespace may still own an open descriptor. Normal exits remove only
video128 after exact label, device identity and module-version checks. Logs go
under RUNNER_TEMP/wendy-go2-h264.*. The script deletes only its compiled binaries.

Local evidence is limited to real generated-video decoding and its old-CDR
negative control, portable procedure checks, shell checking and Linux cross
compilation. Kernel and OCI normal/control execution remains pending hosted CI.

No full Go2 firmware recording exists in the repository; the historical document
contains only a short payload prefix. The exact-target physical companion is
`.github/regression-coverage/manual/go2-camera-rehearsal.py`. Its raw subscriber
records bounded original CDR bytes without publishing any robot command. Every
physical case starts `not_run`. Archived-corpus replay through the kernel service
is explicitly still pending; synthetic pixels cannot close that acceptance.
