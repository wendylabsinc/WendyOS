> **Note:** `wendy device setup` is an advanced command and is not listed in
> `wendy device --help`. It remains fully functional.

Interactive wizard that provisions the device, configures WiFi, and optionally updates the agent. Connects the device to [Wendy Cloud](../../../../cloud/) using the CLI's stored mTLS certificates.

To enroll against a self-hosted [pki-core](../../../../pki/) instead, use [`wendy device enroll`](./enroll.md) with `--cloud-grpc` pointing at it.