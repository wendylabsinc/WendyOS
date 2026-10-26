# `wendy device setup`

Configure a device's Wi-Fi and enrollment interactively. This advanced command
remains available even though it is hidden from `wendy device --help`.

```sh
wendy cloud login
wendy device setup
```

Follow the prompts to select your device, configure Wi-Fi, and optionally update
the agent. For enrollment alone, use
[`wendy device enroll`](./enroll.md). For a self-hosted deployment, ask your
administrator for its Cloud endpoint and select the matching saved sign-in.
