# G1 Object-Retarget Motion-Zero Handoff

This bundle is frozen for deployment integration and read-only validation. It does not authorize robot motion.

- Checkpoint: `/workspace/g1-object-retarget-sweep-20260917/stage2-winner.pt`
- Checkpoint SHA-256: `3aaf0c8279e9190a6d47740f6dfdd427ff4fb495ab7e1ae11334e1e1d3573738`
- Reference: `/workspace/g1-object-retarget-sweep-20260917/runtime/selected-reference`
- Reference SHA-256: `8dddda350bfafd6dbe81696a4cca13d8b0c101cf095aa4e353e6520d011ae28e`
- Selection manifest: `/workspace/g1-object-retarget-sweep-20260917/physical-handoff-motion-zero/selected-policy.json`
- Handoff manifest: `/workspace/g1-object-retarget-sweep-20260917/physical-handoff-motion-zero/handoff-manifest.json`
- Handoff manifest SHA-256: `efc3962d27886377a195ae2fc34f18f5b02922e5842701b347b2c5322508f881`
- Retarget implementation: `/workspace/g1-object-retarget-sweep-20260917/source/object_retarget.py`
- Precomputed gains: `/workspace/g1-object-retarget-sweep-20260917/physical-handoff-motion-zero/retarget-gains-f32.npy` (`[1200, 43, 2]`, `float32`)
- Shift weights: `/workspace/g1-object-retarget-sweep-20260917/physical-handoff-motion-zero/retarget-shift-weights-f32.npy`
- Nominal can world trajectory: `/workspace/g1-object-retarget-sweep-20260917/physical-handoff-motion-zero/nominal-can-world-f64.npy`
- Nominal camera/can reference: `/workspace/g1-object-retarget-sweep-20260917/physical-handoff-motion-zero/nominal-can-camera-reference.npz`

## Required live computation

1. Detect the can center from the live RGB-D observation in the physical camera frame.
2. Reject missing, stale, low-confidence, or temporally inconsistent detections.
3. Transform the detected point using the calibrated and timestamp-aligned physical camera-to-policy/world transform.
4. Compute `detected_can_world_xy - nominal_can_initial_world_xy`.
5. Clip the offset to the simulator-qualified envelope before multiplying by the per-frame `[43,2]` gain and selected gain `1.0`.
6. In the motion-zero gate, log nominal targets, retargeted targets, offsets, clipping, and timestamps without sending joint commands.

## Limitations

- Simulation-selected only; not physical-robot qualified.
- The simulator used ground-truth randomized can displacement. The robot must use perception-derived can position; simulator truth is forbidden.
- The stored camera trajectory is nominal MuJoCo kinematics, not a calibration of the physical RGB-D camera.
- A live detected camera-frame point must be transformed through a calibrated, timestamped camera-to-policy/world transform before applying the gains.
- Perception confidence, latency, stale-frame rejection, offset clipping, and zero-motion dry-run comparison remain unqualified.
- The retargeter corrects world XY translation only; it does not retarget can yaw, vertical displacement, or target placement geometry.
- No artifact in this handoff grants physical motion authority.
