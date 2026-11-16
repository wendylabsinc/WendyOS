# Audit2022 scene and interruption boundaries

Original `441ab9ce1bbc4cd9fcae4812c8a40fee08368082` changed base-class reset dispatch, the initial CokeScene epoch, default reference stepping, the `joint_effort43` observation field, and propagation of KeyboardInterrupt/SystemExit after motor-owner cleanup. These are distinct from frozen policy accuracy or a physical robot trial.

## Recorded scene

`test-coke-scene-runtime.py` requires all seven original `expert/` entries in `Examples/G1CokeDemo/assets.lock.json`, including the 126,262,669-byte model and 58,099,962-byte recorded rollout. It checks original sizes and SHA-256 before and after each run. Private temporary symlinks expose those readonly inputs to byte-identical copied production Python sources; the original input directory is never modified. Only disposable copies receive historical mutations.

The test executes the real MuJoCo3.12.0 model, renders RGB/depth/segmentation observations and compares all 2,400 default CokeScene steps against independent production Simulation replay, including qpos/qvel at every step. It requires the first epoch, terminal no-op, changing real pixels, exact 43-joint effort field, and reset into a fresh camera epoch. Three separate historical controls must fail their corresponding JUnit assertion. An import error, skip, timeout, incorrect asset or unrelated renderer failure cannot satisfy a control.

Local actual execution with Python3.12, NumPy2.2.6 and the original supplied bundle passed in approximately ten seconds. The simulated can reached 0.336259 m lift and 0.009945 m final placement distance; these are simulation results, not physical or learned-policy accuracy. All three controls failed at their intended assertions. MuJoCo's macOS CGL backend reports reduced depth precision without ARB_clip_control; the test asserts finite depth and schema, not sensor calibration accuracy.

Run with:

```sh
uv run --project Examples/G1CokeDemo --frozen --group dev --python 3.12 \
  python .github/scripts/test-coke-scene-runtime.py \
  --expert /absolute/path/to/checksummed/expert \
  --output /absolute/path/to/new/evidence
```

The existing Linux G1 row already provides the exact Coke runtime. A hosted renderer additionally needs `libosmesa6`, and the script selects OSMesa before import. The seven files must be supplied through an approved existing artifact mechanism outside Git, then passed with `--expert`. The repository intentionally has no download URL: `prepare-assets.py` consumes the documented local handoff. Missing assets fail admission, and no substitute model or fabricated rollout is accepted. Hosted registration remains pending this artifact prerequisite; the ordinary project test suite may skip the dedicated scene case, but the required wrapper never accepts a skip. Do not count an absent artifact or an unregistered hosted step as coverage.

## Interruption cleanup

`test-coke-physical-cleanup.py` runs actual production ExactPolicyUnitreeIO against an explicitly supplied private socket backend. It does not instantiate the Unitree SDK, open a DDS participant or send motor commands. The test acquires the real process/file ownership lock, proves contention from an independent process, starts the real watchdog thread and produces a socket release receipt. Actual SIGINT and `sys.exit(19)` interrupt the backend release path. Production must propagate the exact exception after releasing ownership and joining the watchdog. Another process must acquire the same lock, and a fresh in-process lifecycle must arm and stop.

Restoring the historical broad BaseException handler must fail both exact exception-preservation assertions. Normal and control run under the shared process guardian. Their Python children, socket descriptors, watchdog and file lock are owned by the fixture. Both normal cases and the control passed locally; the existing G1 row requires this gate and includes its two normal cases in the locked review suite. This proves the original software cleanup contract. Actual SDK publisher teardown and physical stop behavior remain separate acceptance work.

All outputs are logs, XML and JSON outside Git. No model, image, frame or video is committed or included in PR review source.
