#!/bin/bash
# Stages everything the container needs, without touching the device.
#
# The Qualcomm runtime is proprietary and ships inside the WendyOS image, so it is copied
# from the locally built rootfs rather than vendored here. The model is never vendored:
# point MODEL_DIR at a Genie bundle (Qualcomm AI Hub export).
set -euo pipefail

DEV="${1:?usage: ./setup.sh <device-ip>}"
SDK="${SDK:-/mnt/ssd/wendy/build/tmp/work/armv8-2a-poky-linux/qairt-sdk/2.47.0.260601/sources/qairt/2.47.0.260601}"
ROOTFS="${ROOTFS:-/mnt/ssd/wendy/build/tmp/work/iq_8275_evk_wendyos-poky-linux/wendyos-image/1.0/rootfs}"
MODEL="${MODEL_DIR:-}"
CONFIG="${CONFIG:-gemma3/gemma3-270M-htp.json}"

# The HTP backend under both naming schemes, the FastRPC transport, the board-to-skel
# mapping, the DSP-side skels, and Genie on top.
PATHS=(
  usr/bin/genie-t2t-run
  usr/lib/libGenie.so
  usr/lib/libQnnHtp.so usr/lib/libQnnHtpPrepare.so usr/lib/libQnnHtpV75Stub.so
  usr/lib/libQnnHtpV75CalculatorStub.so usr/lib/libQnnHtpNetRunExtensions.so
  usr/lib/libQnnSystem.so
  usr/lib/libQairtHtp.so usr/lib/libQairtHtpPrepare.so usr/lib/libQairtHtpV75Stub.so
  usr/lib/libQairtHtpBackendExtensions.so usr/lib/libQairtSystem.so
  usr/lib/libcdsprpc.so.1.0.0 usr/lib/libadsprpc.so.1.0.0
  usr/share/qcom/conf.d
  usr/share/qcom/qcs8300/Qualcomm/IQ8275-EVK/dsp
)

echo "==> 1/3 Qualcomm runtime from the built rootfs"
[ -d "$ROOTFS" ] || { echo "    no rootfs at $ROOTFS - build the image, or set ROOTFS="; exit 1; }
# -h dereferences: the skel directory is a symlink into another board's tree, and a
# non-dereferenced copy lands dangling inside the container.
tar czhf genie-htp.tgz -C "$ROOTFS" "${PATHS[@]}"
echo "    $(du -h genie-htp.tgz | cut -f1)"

echo "==> 2/3 model bundle"
rm -rf model && mkdir -p model
if [ -n "$MODEL" ] && [ -d "$MODEL" ]; then
  cp -L "$MODEL"/*.serialized.bin "$MODEL"/tokenizer.json model/ 2>/dev/null || true
  echo "    from $MODEL: $(ls model | tr '\n' ' ')"
else
  echo "    no MODEL_DIR set - the app will start and report what is missing."
  echo "    Re-run as: MODEL_DIR=/path/to/export ./setup.sh $DEV"
fi

echo "==> 3/3 Genie config ($CONFIG)"
SRC="$SDK/examples/Genie/configs/$CONFIG"
[ -f "$SRC" ] || { echo "    no config at $SRC (set CONFIG=<dir>/<file>.json)"; exit 1; }
python3 - "$SRC" <<'PY'
import json, os, sys
cfg = json.load(open(sys.argv[1]))
ctx = sorted(f for f in os.listdir("model") if f.endswith(".serialized.bin"))
if ctx:
    cfg["dialog"]["engine"]["model"]["binary"]["ctx-bins"] = ctx
cfg["dialog"]["engine"]["backend"]["extensions"] = "htp_backend_ext_config.json"
json.dump(cfg, open("genie_config.json", "w"), indent=2)
print("    ctx-bins:", ctx or "<none staged; stock config kept>")
PY
echo "==> ready.  Now run:  wendy run --device $DEV"
