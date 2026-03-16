## wendy run

Build and run application on a WendyOS device

### Synopsis

Reads wendy.json from the current directory or --prefix directory, builds a container image, and deploys it to the target device.

```
wendy run [flags]
```

### Options

```
      --debug                    Enable debug logging
      --deploy                   Create container but do not start it
      --detach                   Start container but do not stream logs
  -h, --help                     help for run
      --no-restart               Do not restart on exit
      --prefix string            Project directory to run from instead of the current working directory
      --restart-on-failure       Restart on failure
      --restart-unless-stopped   Restart unless manually stopped
      --user-args strings        Extra arguments to pass to the container
  -y, --yes                      Automatically accept all interactive prompts
```

### Options inherited from parent commands

```
      --device string   Target device hostname
      --json            Output in JSON format
```

### SEE ALSO

* [wendy](wendy.md)	 - Wendy CLI - Edge Computing Development Tool

