# HelloMLX

This demo is intended to be deployed from one Mac to another Mac running `wendy-agent`.

The development Mac holds the VLM model locally and `wendy run` deploys that model to the target Mac along with the app. Because the model is large, the target Mac should be connected to the development Mac with a Thunderbolt cable for a practical transfer speed.

## 1. Install the `wendy` CLI

On the Mac you are using to deploy the demo:

```sh
brew install wendylabsinc/tap/wendy
```

## 2. Install `wendy-agent` on the target Mac

### Option A: Download a GitHub release

1. Open the releases page: <https://github.com/wendylabsinc/wendy-agent/releases>
2. Download the latest macOS agent archive, for example:
   `wendy-agent-macos-arm64-<version>.zip`
3. Unzip it.
4. Move `WendyAgentMac.app` to `/Applications`.
5. Launch `WendyAgentMac.app` on the target Mac and allow any requested permissions.
6. Make sure camera access is allowed for `WendyAgentMac.app` in macOS privacy settings. This demo depends on webcam access and will not work if camera permission is denied.

### Option B: Install via Homebrew cask

```sh
brew install --cask wendylabsinc/tap/wendy-agent-nightly
open /Applications/WendyAgentMac.app
```

After launching the app, make sure camera access is allowed for `WendyAgentMac.app` in macOS privacy settings. This demo will not work without camera permission.

## 3. Download the VLM model on the development Mac

This example expects the model directory referenced by `wendy.json` to exist locally before you run the demo.

Download it with:

```sh
./Scripts/DownloadVLM.sh
```

The model is about 16 GB, so make sure you have enough disk space on the development Mac before continuing.

## 4. Install Metal Toolchain on the development Mac

The Metal toolchain is required to build this demo. Install it with:

```sh
xcodebuild -downloadComponent MetalToolchain
```

## 5. Connect the two Macs

Connect the development Mac and target Mac with a Thunderbolt 4 or Thunderbolt 5 cable.

This is strongly recommended because the VLM model is about 16 GB and will take a long time to transfer over slower links.

## 6. Run the demo

From the `Examples/HelloMLX` directory on the development Mac:

```sh
wendy run
```

The `wendy` UX will let you pick the target Mac.

`wendy run` builds the app, deploys the files declared in `wendy.json`, including the local VLM model, starts the app on the target Mac, and streams logs.

## 7. How the demo works

This demo runs a vision-language model on the target Mac and evaluates a prompt against frames captured from the target Mac's webcam.

By default, the app configuration includes:

- a local model path: `gemma-3-27b-it-qat-4bit`
- a square frame resolution of `256x256` (override with `--resolution Y`)
- a web server on port `8080`
- a prompt that asks the model to comment on the captured frames

At startup the app prints its final runtime configuration to stdout, for example:

```text
Container sh.wendy.examples.HelloMLX created.
Application sh.wendy.examples.HelloMLX started.
Final app config:
{
  "fps" : 2,
  "interval" : 5,
  "modelPath" : "gemma-3-27b-it-qat-4bit",
  "port" : 8080,
  "prompt" : "These frames were captured at a fixed interval from a webcam. You are a comedian using dark humor below the waist line. Ridicule what is shown by the frames.",
  "resolution" : 256
}
HELLO_MLX_URL=http://konstantins-macbook-pro-m5.local:8080/
HELLO_MLX_DATA_DIR=/Users/konstantinbe/Library/Application Support/sh.wendy.WendyAgentMac/apps/sh.wendy.examples.HelloMLX/Runs
Available cameras:
  - Studio Display Camera [0x2014000015bc0000]
```

The key line is `HELLO_MLX_URL=...`. Open that URL in a browser to access the demo UI on the target Mac.

## 8. Stop the demo

To stop the deployed app:

```sh
wendy device apps stop sh.wendy.examples.HelloMLX
```

## Useful note

If you press `Ctrl-C` while `wendy run` is streaming logs, that stops the local CLI session. To actually stop the app on the target Mac, use `wendy device apps stop ...`.
