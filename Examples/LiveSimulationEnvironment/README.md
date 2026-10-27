# Live simulation environment

A crate and a mesh ramp around the managed Go2. For G1 change `profile` to `g1`
in `scene.json`. Start your explicitly selected Go2/G1 VM first, then:

```sh
wendy vm robot project link ./Examples/LiveSimulationEnvironment --vm my-go2
wendy vm robot project watch ./Examples/LiveSimulationEnvironment
```

Edit `scene.json` or `assets/ramp.obj` in your editor or with a coding agent.
Successful changes reset and pause the same sandbox. Resume and acquire control
again in the browser. Stop watch with Ctrl+C; the VM and last scene keep running.
Keep `.wendy-simulation.json` local (add it to your Git ignore file).
See the [full workflow](../../go/internal/cli/assets/docs/installation/live-simulation-projects.mdx)
for limits, lifecycle behavior, and recovery.
