## wendy init

Initialize a new Wendy project

### Synopsis

Interactively create a new Wendy project with scaffolding, entitlements, and optional AI assistant setup.

```
wendy init [app-id] [flags]
```

### Examples

```
  # Interactive wizard
  wendy init

  # Fully non-interactive WendyOS Python app with persist storage
  wendy init \
    --app-id demo-app \
    --target wendyos \
    --language python \
    --entitlement gpu,usb,persist \
    --persist-name demo-data \
    --persist-path /data \
    --assistant skip

  # Fully non-interactive WendyOS app with GPIO and I2C entitlements
  wendy init \
    --app-id edge-sensors \
    --target wendyos \
    --language swift \
    --entitlement gpio,i2c \
    --gpio-pins 17,27,22 \
    --i2c-device /dev/i2c-1 \
    --assistant skip

  # Wendy Lite defaults to Swift; use this to avoid entitlement prompts
  wendy init \
    --app-id lite-app \
    --target wendy-lite \
    --no-extra-entitlements \
    --assistant skip

  # Start Claude after init and install Wendy skills automatically
  wendy init \
    --app-id ai-app \
    --target wendyos \
    --language python \
    --entitlement gpu,audio \
    --assistant claude \
    --install-claude-skills
```

### Options

```
      --app-id string           Application ID to write into wendy.json
      --assistant string        AI assistant to launch after init: claude, codex, or skip
      --entitlement strings     App entitlement to enable (repeatable or comma-separated)
      --gpio-pins string        GPIO pins for the gpio entitlement (comma-separated, e.g. 17,27,22)
  -h, --help                    help for init
      --i2c-device string       I2C device path for the i2c entitlement (e.g. /dev/i2c-1)
      --install-claude-skills   Install Wendy Claude skills before launching Claude
      --language string         Project language: swift or python
      --no-extra-entitlements   Skip entitlement prompts and use only the default network entitlement
      --persist-name string     Container ID for the persist entitlement
      --persist-path string     Mount path for the persist entitlement (e.g. /data)
      --target string           Target platform: wendyos or wendy-lite
```

### Options inherited from parent commands

```
      --device string   Target device hostname
      --json            Output in JSON format
```

### SEE ALSO

* [wendy](wendy.md)	 - Wendy CLI - Edge Computing Development Tool

