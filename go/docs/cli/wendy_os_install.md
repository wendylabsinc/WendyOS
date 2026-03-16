## wendy os install

Install WendyOS or Wendy Lite firmware on a device

### Synopsis

Interactively select a supported device, download the latest OS image or firmware, and write it to the target.

When called with positional arguments, skips interactive prompts:
  wendy os install <image-path> <drive-id> --force

```
wendy os install [image] [drive] [flags]
```

### Options

```
      --force     Skip confirmation prompt
  -h, --help      help for install
      --nightly   Use nightly/prerelease builds
```

### Options inherited from parent commands

```
      --device string   Target device hostname
      --json            Output in JSON format
```

### SEE ALSO

* [wendy os](wendy_os.md)	 - Manage the WendyOS operating system

