# `wendy auth local-key show`

Prints this computer's **local** public key and its `sha256:` fingerprint. The key proves this computer to a Closed device (see [`wendy device access`](../device/access.md)).

```sh
wendy auth local-key show [--json]
```

Give the output to an owner of a device, who trusts it with [`wendy device keys add`](../device/keys.md). The private key stays in `~/.wendy/local-operator-key.pem` (mode `0600`; `WENDY_CONFIG_DIR` moves it). It is created on first use and is independent of `wendy auth login`: logging in, logging out and certificate renewal never change it.
