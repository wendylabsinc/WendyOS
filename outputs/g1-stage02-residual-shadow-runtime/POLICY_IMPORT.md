# Load a simulator policy onto G1

The Stage 2 adapter is now pinned to a **runtime ABI**, not a particular
checkpoint/update. Compatible policy weights can be staged and deployed with
one command; checkpoint SHA, update number, candidate ID, and reference SHA are
read from the sealed policy bundle.

## One-command path

Local simulator export:

```sh
sh tools/deploy_sim_policy /path/to/runtime-handoff --deploy
```

Downloaded simulator export:

```sh
sh tools/deploy_sim_policy https://artifact-host/policy.tar.gz --deploy
```

The default target is `unitree-g1-nx-2.local`. Use `--device` to select a
different saved Wendy device. Omit `--deploy` to create and validate the minimal
deployment context without changing the G1.

The command:

1. accepts a directory or safe tar archive;
2. verifies the checkpoint hash declared by the simulator export;
3. verifies the installed `316 -> 15` GRU ABI, 40 Hz control, 20 Hz camera,
   owned-joint mapping, reference arrays, and frozen policy source hashes;
4. strips the simulator export down to the files needed for inference;
5. emits a sealed `POLICY.json`;
6. deploys only the motion-zero inference app through `wendy run`;
7. relies on on-device tensor-shape/model construction before readiness can
   pass.

## Play policy

The physical UI asks the inference service for its current policy identity and
reference contract. Pressing **Play policy** first performs a motion-zero
preflight of:

- active policy ABI and reference;
- inference availability;
- camera/segmentation availability;
- current Unitree feedback, mode, timing, and remote neutrality;
- publisher ownership and fault-hold state.

Only after that succeeds does the UI ask for the existing harness/workspace
operator confirmation and submit the bounded run. The request pins the exact
checkpoint SHA seen during preflight; a concurrent policy swap is rejected
before publisher ownership. The physical wrapper remains the sole DDS command
owner.

## What still requires an adapter update

These are ABI changes, not ordinary policy swaps:

- observation or action dimensions;
- joint order or owned-joint mapping;
- architecture/recurrent-state layout;
- controller or camera rate;
- policy source implementation;
- reference file format.

A simulator score or successful import does not physically qualify a policy.
It only proves that the artifact is compatible with the installed runtime.
