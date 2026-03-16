## wendy device update

Update the agent binary on the target device

### Synopsis

Downloads the latest agent binary from GitHub and uploads it to the device. Use --binary to provide a local binary instead.

```
wendy device update [flags]
```

### Options

```
      --binary string   Path to a local agent binary to upload (skips download)
  -h, --help            help for update
      --nightly         Use the latest nightly (prerelease) build
```

### Options inherited from parent commands

```
      --device string   Target device hostname
      --json            Output in JSON format
```

### SEE ALSO

* [wendy device](wendy_device.md)	 - Manage WendyOS devices

