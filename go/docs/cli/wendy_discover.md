## wendy discover

Discover WendyOS devices on the network

### Synopsis

Continuously scan for WendyOS devices until Ctrl+C. Use --timeout to scan once for a fixed duration.

```
wendy discover [flags]
```

### Options

```
  -h, --help               help for discover
      --timeout duration   Scan once for this duration then exit (default 5s)
      --type string        Discovery type: usb, lan, bluetooth, external, all (default "all")
```

### Options inherited from parent commands

```
      --device string   Target device hostname
      --json            Output in JSON format
```

### SEE ALSO

* [wendy](wendy.md)	 - Wendy CLI - Edge Computing Development Tool

