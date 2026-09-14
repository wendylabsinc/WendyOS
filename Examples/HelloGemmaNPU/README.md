# HelloGemmaNPU — Text Generation on the Hexagon NPU

Runs a small Gemma model on the Dragonwing NPU from inside a container deployed with
`wendy run`, and serves a web page for interactive prompts. No SSH to the device.

On this hardware the NPU is the only accelerator that produces correct LLM output at a
usable rate: the Adreno GPU returns garbage for LLM inference (upstream Turnip MUL_MAT
defect) and the CPU is several times slower.

## Usage

    MODEL_DIR=/path/to/genie-export ./setup.sh <device-ip>
    wendy run --device <device-ip>

Then open `http://<device-ip>:8080/` and type a prompt.

`setup.sh` copies the aarch64 Qualcomm runtime out of the **locally built rootfs** — it
never touches the board — stages the model, and writes `genie_config.json` from the SDK's
Genie config for the chosen model. With no `MODEL_DIR` the app still builds and runs, and
the page reports exactly which files are missing.

Override `CONFIG=` to pick a different model, e.g. `CONFIG=phi3-mini/phi3-mini-genaitransformer-htp-kv-share.json`
or one of the `qwen3/` configs. `ROOTFS=` and `SDK=` override the source paths.

## The model is not vendored

Genie needs a context binary compiled for this SoC, which comes from Qualcomm AI Hub, plus
the matching `tokenizer.json`. Gemma additionally requires accepting Google's licence.
Point `MODEL_DIR` at the export directory; the app is model-agnostic, so a
permissively-licensed model (Phi-3-mini, Qwen3) drops in the same way.

## What the entitlement provides

`{ "type": "npu" }` delivers four things, visible in the table at the top of the page:

| | |
|---|---|
| `/dev/fastrpc-*` | the FastRPC transport to the on-SoC DSPs (non-secure nodes only) |
| `/dev/dma_heap/system` | the dma-buf heap the HTP backend allocates from |
| `MACHINE_NAME` | the device-tree model, which selects the DSP skel directory |
| `FASTRPC_PROCESS_ATTRS` | selects the unsigned DSP process domain |

That last one matters more than it looks. The `-secure` FastRPC nodes are the signed
process-domain path and are never granted to a container; the kernel confines the granted
nodes to the *unsigned* domain and refuses anything else with `Untrusted application
trying to offload to signed PD` in `dmesg`. Without the attribute the DSP is reachable but
cannot be offloaded to, which surfaces as an opaque `Failed to create device: 14001`. The
app sets a fallback for older agents, so it also works before that entitlement fix lands.

## Image requirements

The Qualcomm userspace needs **glibc >= 2.38** (`debian:trixie-slim` clears it) plus
`libatomic1`, `libc++1`, `libc++abi1`, `libstdc++6` and `libyaml-0-2`. Two backend naming
schemes ship side by side, so both `libQnnHtp*` and `libQairtHtp*` are packaged.

## Things worth knowing

- **`tar -h` is load-bearing.** The DSP skel directory is a symlink into another board's
  tree; a copy that does not dereference it lands dangling and the skels never resolve.
- **`/proc/device-tree/model` is unreadable in a container** — it sits behind the
  `/sys/firmware` mask, which also hides the DMI and ACPI tables. `MACHINE_NAME` exists
  precisely so the mask can stay in place.
- **A failed FastRPC run can wedge the whole cDSP**, after which even host-side tools fail.
  Recover with `echo stop > /sys/class/remoteproc/remoteproc2/state` then `start`
  (`remoteproc2` is the cDSP); restarting `cdsprpcd` does not help and no reboot is needed.
- **`qnn-platform-validator` is not an access check.** It reports `Prerequisites: Present`
  with no entitlement at all, because it only checks that the backend libraries dlopen.

## Related

`Examples/HelloLLM` is the NVIDIA Jetson equivalent, using CUDA and HuggingFace
Transformers instead of the NPU and Genie.
