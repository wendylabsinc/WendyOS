# HelloMLX

This demo is intended to be deployed from one Mac to another Mac running `wendy-agent`.

The development Mac holds the selected VLM model locally and `wendy run` deploys that model to the target Mac along with the app. Larger model tiers should be transferred over Thunderbolt for practical transfer speed.

## 1. Install the `wendy` CLI

On the Mac you are using to deploy the demo:

```sh
curl -fsSL https://install.wendy.dev/cli.sh | bash
```

Or via Homebrew (see INSTALL.md for trust requirements):

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

This example requires you to choose a model tier before running the demo. `wendy.json` deploys `Models/Current`, and the download script updates that symlink to point at the selected model.

Choose one tier explicitly. Each tier uses a model supported by the current `MLXVLM` dependency in this example:

```sh
./Scripts/DownloadVLM.sh small    # 16 GB Macs, recommended beta tier (~1.2 GiB)
./Scripts/DownloadVLM.sh medium   # 32 GB Macs, better quality/larger transfer (~2.9 GiB)
./Scripts/DownloadVLM.sh large    # 64 GB Macs, Gemma 27B (~15.7 GiB)
```

| Tier | Recommended Mac | Model | MLXVLM type | Download size | Expectation |
| ---- | --------------- | ----- | ----------- | ------------- | ----------- |
| `small` | 16 GB unified memory | `mlx-community/Qwen2-VL-2B-Instruct-4bit` | `qwen2_vl` | ~1.2 GiB | Recommended beta demo tier for constrained Macs. |
| `medium` | 32 GB unified memory | `mlx-community/Qwen2.5-VL-3B-Instruct-4bit` | `qwen2_5_vl` | ~2.9 GiB | Higher quality, larger transfer, more memory headroom. |
| `large` | 64 GB unified memory or more | `mlx-community/gemma-3-27b-it-qat-4bit` | `gemma3` | ~15.7 GiB | Best large-model option; not practical on 16 GB Macs. |

Quality note: `small` and `medium` are low-quality validation tiers for constrained Macs. Use a Mac with lots of RAM and the `large` model when you need the demo to actually do the job well.

The current selected model is available at:

```text
Models/Current -> <selected-model-directory>
```

The script also writes `Models/Current/info.json` with the selected tier, Hugging Face model ID, and local model size. The web UI displays this metadata in the Model status field.

Make sure the development Mac has enough disk space for the selected tier before continuing.

## 4. Install Metal Toolchain on the development Mac

The Metal toolchain is required to build this demo. Install it with:

```sh
xcodebuild -downloadComponent MetalToolchain
```

## 5. Connect the two Macs

Connect the development Mac and target Mac with a Thunderbolt 4 or Thunderbolt 5 cable.

This is strongly recommended for the `medium` and `large` tiers, especially because `large` transfers roughly 16 GB of model files.

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

- a local model path: `Current`, which is the deployed copy of `Models/Current`
- a square frame resolution of `256x256` (override with `--resolution Y`)
- a web server on port `8080`
- a prompt that asks the model to comment on the captured frames in English

At startup the app prints its final runtime configuration to stdout, for example:

```text
Container sh.wendy.examples.hellomlx created.
Application sh.wendy.examples.hellomlx started.
Final app config:
{
  "fps" : 2,
  "interval" : 5,
  "modelPath" : "Current",
  "port" : 8080,
  "prompt" : "These frames were captured at a fixed interval from a webcam. Reply in English. You are a comedian using dark humor below the waist line. Ridicule what is shown by the frames.",
  "resolution" : 256
}
HELLO_MLX_URL=http://konstantins-macbook-pro-m5.local:8080/
HELLO_MLX_DATA_DIR=/Users/konstantinbe/Library/Application Support/sh.wendy.WendyAgentMac/apps/sh.wendy.examples.hellomlx/Runs
Available cameras:
  - Studio Display Camera [0x2014000015bc0000]
```

The key line is `HELLO_MLX_URL=...`. Open that URL in a browser to access the demo UI on the target Mac.

## 8. Stop the demo

To stop the deployed app:

```sh
wendy device apps stop sh.wendy.examples.hellomlx
```

## Useful note

If you press `Ctrl-C` while `wendy run` is streaming logs, that stops the local CLI session. To actually stop the app on the target Mac, use `wendy device apps stop ...`.
