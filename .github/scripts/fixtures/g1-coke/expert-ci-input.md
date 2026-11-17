# Required expert replay input

`expert-ci-input.json` lists the exact seven files already required by `Examples/G1CokeDemo/assets.lock.json`. Their total is 184,393,202 bytes. Provision them, with no extra directory level, at:

```text
${RUNNER_TEMP}/wendy-coke-expert/g1-expert-sim-replay-20260915/
```

The missing input is an owner-approved read-only remote artifact location and its existing CI access mechanism for these exact bytes. No such location is configured in the repository. This document does not authorize uploading the local Downloads bundle or creating a public release.

On 2026-11-13, organization code search for the bundle name and exact rollout hash found WendyOS's local handoff instructions and lock only. Searches for the model hash also found `wendy-g1-can-demo` and `g1-coke-demo` references, but their selected-reference rollout is a different 788,540-byte file with SHA-256 `8dddda350bfafd6dbe81696a4cca13d8b0c101cf095aa4e353e6520d011ae28e`; it cannot replace the locked 58,099,962-byte rollout. No exact-named Actions artifact or matching release asset was found in WendyOS, wendy-g1-can-demo, g1-coke-demo or g1-can-handoff-sim2real. These scoped searches do not prove that no differently named private archive exists.

Once an approved mechanism supplies those seven files, the existing Linux G1 row needs `libosmesa6` and this command, after its locked Python environment setup:

```sh
uv run --project Examples/G1CokeDemo --frozen --group dev --managed-python --python 3.12 \
  python .github/scripts/test-coke-scene-runtime.py \
  --expert "$RUNNER_TEMP/wendy-coke-expert/g1-expert-sim-replay-20260915" \
  --output "$RUNNER_TEMP/coke-scene-runtime"
```

The wrapper checks every size and SHA-256 before and after every normal/control execution. Preserve only its log, XML and JSON evidence, never the model or rollout. Missing or different files must fail admission. Keep this hosted boundary pending until the approved artifact mechanism and actual normal/control execution exist. The supplied local bundle already passed the actual MuJoCo replay and all three controls; that local proof does not establish hosted artifact availability.
