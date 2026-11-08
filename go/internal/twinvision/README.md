# Shared camera alignment

Room-based alignment for a fixed external camera runs in the Linux digital twin.
Apple and web clients capture full-range grayscale frames, display the response,
and confirm that their selected camera and room are still current before commit.
They do not choose lens candidates, validate poses, or write solved metadata.

`POST /v1/camera-alignment` has four operations:

1. `begin`: send `camera`, `roomFrame`, and `frame` with width, height, codec,
   sequence, ageMS and base64 gray bytes. The service reads the exact room map
   and current camera metadata. It returns a token and first-frame result.
2. `verify`: send the token and a fresh frame captured at least 800 ms after
   the first solve returned. Format must match and sequence must advance.
   The service fixes the selected lens and validates pose agreement. It returns
   the proposed metadata but does not save it.
3. `commit`: send the token within ten seconds of verification. The service
   checks the room while holding its store lock and writes camera metadata with
   the revision read at `begin`. A successful token cannot be replayed.
4. `cancel`: discard the token. Pending jobs also expire after two minutes.

Successful responses contain `policyVersion: 1`. A failed or ambiguous solve is an
error, never a saveable display candidate. Matching measured lenses remain
measured. A measured lens for another capture mode requires correcting the
mode or measuring that lens. Without a measurement, the worker tries a joint
fit, a matching stored estimate, and a focal-length search, in that order.
Estimated results remain assumed through verification and persistence.

`solver.go` owns validation and native transport. `native/SiteLocalization.cpp`
is the only solver source. Apple source files forward to it for scan-map capture,
onboard-camera matching, and screen-based lens calibration. The worker and Apple
builds consume `native/opencv.lock`. The shared native tests run during Linux
worker builds and through `macos/Scripts/test-localization.sh` on Apple. The
`Digital twin parity` workflow checks both Linux architectures, Apple, and the
Go service. The worker resets OpenCV's RNG and uses one thread per request.

Run `go test -race ./internal/twin ./internal/twinvision` from the Go directory.
For worker replay, set `WENDY_TWIN_REPLAY` to a JSON file containing `landmarks`
as base64, `frame`, a nine-element `lens`, and numeric `provenance`. Set
`WENDY_TWIN_VISION_WORKER` to the packaged worker, then run `TestNativeReplay`.
The replay does not connect to a camera or robot or modify metadata.

This extraction covers fixed-camera alignment. USD geometry interpretation and
Go2 onboard-camera visual hints still originate in clients. LiDAR registration,
reference persistence, bounded turning and motion guards already run in the
Linux service. Those remaining client inputs need a separate migration before
all robot-localization behavior can be called identical.

Robot diagnostics should call `POST /v1/robot` with
`{"Operation":"observe","Serial":"..."}`. This reads the existing observation
stream without opening a control session or renewing its movement lease.
`open` returns HTTP 409 while another controller has requested data or control
within the last 15 seconds. Closing a controller releases ownership immediately;
a disconnected client becomes replaceable after that interval.
