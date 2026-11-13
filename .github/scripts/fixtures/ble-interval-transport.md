# BLE interval refusal and authenticated traffic (audit 2527)

Run as the ordinary Linux user in the existing race-test job:

```sh
python3 .github/scripts/test-ble-interval-transport.py "$RUNNER_TEMP/ble-interval-transport"
```

The output directory must not exist. Preserve it as evidence. The gate builds
normal and historical no-rejection-fallback variants with the race detector,
runs eight controller outcomes, and requires the control to fail at the missing
authenticated attachment assertion. Each runtime is bounded by 30 seconds;
builds are bounded by 180 seconds. No elevated capabilities or radio are used.

An overlay replaces only ACL-handle lookup and HCI syscall preparation and
isolates the radio-policy filename. Production HCI command generation/event
parsing, initial/steady tuning decisions, provider dial ownership, PKI, TLS and
stream hello remain active. Allowed outcomes must exchange two exact payloads.
Unknown results, accepted-command timeout, disconnection and cancellation must
send no TLS bytes, release controller IO and free the provider setup slot. The
accepted timeout uses the unchanged four-second production budget.

This covers the software continuation after controller refusal. It does not
prove a physical Realtek controller keeps its ACL/CoC alive, controller firmware
behaviour, RF performance or mesh routing. Those require the reserved-radio
rehearsal. A cross-compile or `--prepare-only` output is not runtime acceptance.
