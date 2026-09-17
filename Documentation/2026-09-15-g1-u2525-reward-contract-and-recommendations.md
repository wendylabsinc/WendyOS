# G1 u2525 `+100` reward contract and recommendations

Date: 2026-09-15
Scope: simulation-only analysis of the stage-aware `+100` continuation from update 2,525. This document does not qualify a checkpoint for deployment or physical execution.

## Bottom line

The current reward is not simply “success = 100 points.” A clean 60-second expert/reference replay scores **344.57 undiscounted points**:

- `+100.00` terminal success
- `+151.06` opposed plus three-finger grasp reward
- `+48.66` lift-height reward
- `+22.00` over-zone reward
- `+22.85` net from tracking, hold, landing, progress, and the stationary penalty

The two grasp-rate terms alone are **43.8% of the expert score**; terminal success is **29.0%**. This helps explain why increasing the terminal bonus cannot fix training by itself: the learner can collect a large amount of dense grasp/lift reward without ever completing the placement, and failed carry/contact events usually receive no immediate scalar punishment.

The leading training direction remains **learning rate `3e-6`**, but it should be combined with reward/termination corrections and a policy-drift guard. It is not yet a qualified winner: the short `3e-6` arm had no full successes.

## 1. Episode phases, step by step

The controller runs at 40 Hz (`dt = 0.025 s`) with 25 MuJoCo physics substeps per control step.

| Phase | Entry condition | What the policy is being encouraged to do | Exit/next-stage condition | 60 s deadline |
|---|---|---|---|---:|
| 0. Approach | Episode reset | Follow the joint reference and keep the can near its reference position | First hand-can contact moves stage to 1 | 21.6 s |
| 1. Grasp formation | Any hand-can contact | Form opposed thumb-versus-fingers contact, include all three digits, and optionally seat the can inside the palm | Can lift above 1 cm moves stage to 2 | 22.8 s grasp deadline |
| 2. Lift and carry | Can above 1 cm; `lifted` becomes true above 3 cm | Maintain grasp, increase lift height, and transport toward the destination | Once previously lifted and horizontally over the 4.5 cm landing zone, stage becomes 3 | 27.6 s lift; 43.2 s transport |
| 3. Place and release | Lifted can is over the destination zone | Descend upright, land on the intended table surface, open the hand, and remain supported/still | `task_success()` becomes true after all terminal conditions hold | 54.0 s place |
| Terminal evaluation | Current code waits for horizon/deadline/fatal/numerical termination | Deliver the `+100` only if success is still true when `done` occurs | Episode ends | 50–80 s horizon |

The phase deadlines scale with the randomized 50–80 second episode duration. For example, the approach deadline ranges from 18.0 to 28.8 seconds and the placement deadline from 45.0 to 72.0 seconds.

### Full-success requirements

The terminal bonus requires all of the following at episode termination:

| Requirement | Threshold |
|---|---|
| Maximum lift | Greater than 8 cm |
| Horizontal destination distance | Less than 4.5 cm |
| Upright orientation | Can vertical cosine greater than 0.95 |
| Can speed | Less than 0.03 m/s |
| Release | Recorded grasp followed by at least 2 cm opening in the placement corridor |
| Landing | Valid front-table landing in the corridor |
| Carry safety | Zero carry violations |
| Contact safety | Zero disqualifying contact violations |
| Stable support | At least 1,000 physics substeps, equivalent to approximately 1 second |
| Numerical state | No numerical fault |

## 2. Positive rewards

“Per step max” below is at 40 Hz. Rate rewards accumulate for as long as their gate remains active.

| Reward | Active when | Formula / rule | Maximum contribution | Intended behavior | Important consequence |
|---|---|---|---:|---|---|
| Joint tracking | Stage 0 | `0.025 × 0.25 × exp(-30 × mean_joint_error²)` | `+0.00625/step`, `+0.25/s` | Stay near the demonstrated approach | A long, accurate approach remains positively rewarded |
| Can tracking | Stage 0 | `0.025 × 0.50 × exp(-80 × can_position_error²)` | `+0.0125/step`, `+0.50/s` | Preserve the demonstrated can trajectory | Together with joint tracking, perfect approach can earn `+0.75/s` |
| First visual detection | First valid detection only | One-shot | `+0.10/episode` | Acquire the can visually | Tiny relative to the grasp rates |
| Opposed grasp | Stages 1–2, before reference release | `4/s × min(thumb force, index+middle force)/2 N`, clipped to `[0,1]` and averaged across substeps | `+0.10/step`, `+4/s` | Secure thumb-opposed contact | Can accumulate for much of grasp, lift, and carry |
| All-three-finger grasp | Stages 1–2, before release | `4/s × min(thumb,index,middle force)/2 N`, clipped to `[0,1]` and averaged across substeps | `+0.10/step`, `+4/s` | Use thumb, index, and middle together | Adds another `+4/s`; the two grasp terms can dominate the episode return |
| Three-finger hold | Stages 1–2 when all-three score is positive | Fixed rate | `+0.00625/step`, `+0.25/s` | Keep all three fingers engaged | Small compared with the two `+4/s` grasp terms |
| Inside-palm pre-lift | At least 0.1 N inside-palm contact during at least 80% of substeps, continuously for 1 s, before a 3 cm lift | One-shot, non-farmable | `+1.00/episode` | Seat the can in the palm before lifting | Rare in the observed run and absent in the nominal expert score |
| Lift height | Stage 2 or later | `0.025 × 0.50 × clip(lift/0.02 m, 0, 4)` | `+0.05/step`, `+2/s` at 8 cm or higher | Lift progressively rather than use a hard 8 cm threshold | Continues paying at its maximum above 8 cm |
| Over landing zone | Stage 3 | Fixed rate | `+0.025/step`, `+1/s` | Carry the can to the destination | Can be accumulated while hovering over the zone |
| Bottom/lower-rim landing | First qualifying front-table contact in stage 3 | One-shot | `+3.00` | Prefer a proper bottom landing | Small compared with accumulated grasp reward |
| Sidewall landing | First qualifying sidewall contact in stage 3 | One-shot | `+1.00` | Gives partial credit to a weaker landing | Lower than bottom landing, as intended |
| Stage advance | On a new best stage | `0.25 × stage_delta` | Up to `+0.75` cumulatively | Reward forward phase transitions | Monotonic, so it cannot be farmed by moving backward |
| Object progress | On a new best composite progress | Best-so-far delta of grasp, lift, destination progress, and placement | Up to `+1.00` cumulatively | Bridge between milestones | Useful but much smaller than the grasp-rate terms |
| Full task | Only when `task_success AND done` | One-shot | `+100.00` | Complete the whole task cleanly | Current implementation delays delivery until episode termination |

There is **no high-force ceiling** in the grasp reward. Force is used to determine whether the minimum 2 N secure-contact score has saturated, but excess force is not negatively rewarded by these terms.

## 3. Explicit punishments

| Punishment | Trigger | Value | Ends episode? | Interpretation |
|---|---|---:|---:|---|
| Residual magnitude | Any nonzero residual action | `-0.025 × 0.10 × mean((offset/cap)²)` | No | Keeps PPO corrections small relative to the reference |
| Excess target velocity/acceleration | Requested joint motion beyond 0.25 rad/s or 0.5 rad/s² | `-0.025 × 0.05 × excess` | No | Discourages abrupt residual motion |
| Stationary before placement | After 5% of the episode, not placed, with both joint and can speed below thresholds | `-0.0025/step`, `-0.10/s` | No | Intended to prevent doing nothing; smaller than the maximum `+0.75/s` approach shaping |
| Missed phase deadline | First missed approach, grasp, lift, transport, or place deadline | `-2.00` once | Yes | Stops trajectories that fall behind the stage schedule |
| Severe early robot-table strike | Severe robot-table contact before first hand-can contact | `-5.00` | Yes | Fatal approach collision |
| Numerical fault | Any non-finite simulator position or velocity | Current step reward is replaced by `-100` | Yes | Simulator integrity failure, not a behavior-shaping term |

“Severe” robot-table contact means more than 1 mm penetration or more than 2 N force. A narrow middle-finger/table brush is permitted after hand-can contact begins and before the can has ever lifted 3 cm.

## 4. Disqualifiers that are not immediate punishments

This is the largest reward-alignment gap.

| Event | Current effect | Immediate scalar penalty? | Why it matters |
|---|---|---:|---|
| Disallowed severe contact after grasp starts | Increments `contact_violations`; makes terminal success impossible | **No** | The policy can continue collecting grasp/lift/zone reward after the episode is already invalid |
| Wrong-table contact during carry | Increments `carry_bad`; makes terminal success impossible | **No** | Failure is only expressed through the missing distant terminal bonus |
| Lost opposed grip during active lift/carry | Increments `carry_bad` unless in controlled descent/release allowance | **No** | There is no immediate negative learning signal at the exact failure frame |
| Invalid front-table landing | Increments `carry_bad`; makes terminal success impossible | **No** | Placement failure can still retain all previously accumulated positive reward |
| Excess grasp force | No force ceiling | **No** | Contact scores saturate at 2 N but do not discourage unnecessary force |
| Failure to finish | No `+100` | Indirect only | If the policy never reaches success, changing `+100` to `+300` delivers no new samples or gradient signal |

## 5. What the expert/reference earns

Exact nominal replay: one unrandomized, zero-residual, 60-second reference trajectory; full success; 33.6 cm maximum lift; zero contact or carry violations.

| Component | Points | Share of total |
|---|---:|---:|
| Terminal success | 100.000 | 29.0% |
| Opposed grasp | 76.044 | 22.1% |
| Three-finger grasp | 75.012 | 21.8% |
| Lift height | 48.656 | 14.1% |
| Over landing zone | 22.000 | 6.4% |
| Approach tracking and motion terms | 14.088 | 4.1% |
| Three-finger hold | 4.694 | 1.4% |
| Bottom landing | 3.000 | 0.9% |
| Monotonic progress | 1.648 | 0.5% |
| Stationary penalty | -0.568 | -0.2% |
| **Undiscounted total** | **344.573** | **100%** |

At `gamma = 0.9995`, the discounted return from the start of that replay is **164.675**. The final `+100` contributes only **30.125** to the start-state return because it arrives at control frame 2,400.

## 6. Gamma, GAE, GRU, and the “20% retention” question

| Mechanism | Current value | Half-life at 40 Hz | What it actually does |
|---|---:|---:|---|
| Reward discount | `gamma = 0.9995` | 1,386 steps, **34.65 s** | Determines how much a future reward contributes to an earlier return |
| GAE trace | `gamma × lambda = 0.9995 × 0.95 = 0.949525` | 13.38 steps, **0.335 s** | Determines how rapidly advantage evidence is propagated backward through sampled steps |
| Proposed high-lambda arm | `gamma × lambda = 0.9995 × 0.995` | 125.74 steps, **3.14 s** | Carries advantage evidence farther, but increases estimator variance |
| GRU | Recurrent policy memory | Not a reward half-life | Retains observation/history information; it does **not** make terminal reward credit automatically propagate through a long episode |

The terminal reward does not literally “retain only 20%.” Its retained fraction depends on how many steps away it is: `gamma^k` for discounted return and approximately `(gamma × lambda)^k` for the GAE trace. In the 60-second expert replay, the start-state discounted terminal contribution is about 30%, while the direct GAE trace is far shorter.

The current 128-step rollout is only 3.2 seconds. Therefore, a terminal reward delayed tens of seconds cannot directly teach the earlier grasp and carry decisions through one GAE trace, even though the critic can eventually bootstrap some information backward across updates.

## 7. Evidence from the observed training and matched arms

The update-866 run showed that lifting was common at the batch level but full completion remained rare at the episode level:

| Observation | Result |
|---|---:|
| Completed episodes above 8 cm | 128 / 3,714 = 3.4% |
| Full success | 62 / 3,714 = 1.7% |
| `P(full success | >8 cm)` | 62 / 128 = 48.4% |
| `P(contact failure | >8 cm)` | 19 / 128 = 14.8% |
| `P(carry failure | >8 cm)` | 50 / 128 = 39.1% |

The matched short arms pointed to policy drift at the original learning rate:

| Arm | Early-to-late mean reward change | Lift above 8 cm | Full success | Interpretation |
|---|---:|---:|---:|---|
| LR `3e-5` | -33.6% | 17 / 130 = 13.1% | 2 / 130 = 1.5% | Clear deterioration; grasp, three-finger, and lift rewards all declined |
| LR `3e-6` | +158.7% | 6 / 48 = 12.5% | 0 / 48 | Best stabilization direction, but too few episodes and no success to qualify |
| LR `1e-5` | +17.4% | 5 / 54 = 9.3% | 0 / 54 | Better than baseline drift, weaker than `3e-6` |
| Lambda `0.995` | Approximately flat | 3 / 36 = 8.3% | 0 / 36 | Partial; cannot rank reliably |
| Terminal `+300` | Improving but incomplete | 1 / 27 = 3.7% | 0 / 27 | The larger bonus was never delivered, so this did not test its intended effect |

## 8. Recommended changes, in order

### Priority 1: stabilize optimization before changing many rewards

1. Use actor learning rate **`3e-6`** as the next matched baseline.
2. Protect the pretrained recurrent representation:
   - initially freeze the GRU, or use a separate GRU learning rate around `3e-7`;
   - allow the critic a higher learning rate than the actor because the value function must relearn the changed return scale;
   - retain the actor/GRU when changing reward contracts, but reset the critic and optimizer as the existing migration contract requires.
3. Add a KL drift guard against the update-2,525 parent:
   - measure policy KL on a fixed reference/held-out observation set after every update;
   - stop PPO epochs early when KL exceeds a small target band;
   - optionally add a KL penalty and anneal its coefficient only after held-out grasp/lift/carry metrics remain stable.

The current PPO objective has ratio clipping (`0.8–1.2`), entropy regularization, and gradient clipping, but no explicit KL anchor. PPO clipping alone does not guarantee that repeated updates stay close to the parent policy.

### Priority 2: deliver terminal credit when success actually happens

Change `done` so the episode terminates immediately after the complete success criteria, including the one-second stable-support requirement, become true. Deliver `+100` at that moment.

Benefits:

- removes an unnecessary wait until the 50–80 second horizon;
- shortens the temporal distance between placement/release decisions and terminal credit;
- prevents a completed episode from being damaged after it has already satisfied the task;
- makes `P(full | place)` and return comparisons easier to interpret.

### Priority 3: make disqualifying failures immediately visible to the learner

Add a one-shot penalty at the first disqualifying contact or carry violation and terminate the invalid episode. Keep the permitted middle-finger grasp brush exempt.

A reasonable first experiment is:

| Event | Suggested first test |
|---|---:|
| Fatal approach collision | `-20` to `-25`, then terminate |
| First post-grasp disqualifying contact | `-5` to `-10`, then terminate |
| First carry violation or dropped opposed grasp | `-5` to `-10`, then terminate |
| Invalid landing | `-5` to `-10`, then terminate |

The goal is not that every late failure must make the entire episode return negative. A late failure may have earned legitimate approach and grasp credit. The requirement is that the action causing the failure has a clearly worse local advantage than safe continuation. An early fatal collision should preferably produce a negative total episode return and must be worse than a safe no-progress timeout.

### Priority 4: rebalance grasp reward and bridge carry/placement

The combined opposed/all-three grasp rates contributed `151.06` points to the expert—more than the terminal bonus. Cap them so maintaining an already-secure grasp cannot dominate finishing the task.

Suggested first contract:

| Component | Suggested structure |
|---|---|
| Opposed grasp | Keep current score, but cap cumulative episode credit at `+10` |
| All-three grasp | Keep current score, but cap cumulative episode credit at `+10` |
| Three-finger hold | Keep `+0.25/s`, cap at `+5` |
| Safe carry progress | Best-so-far destination progress while lifted, upright, opposed, and violation-free; cap at `+10` |
| Controlled descent | One-shot `+5` on first safe descent inside the landing corridor |
| Bottom landing | Raise one-shot credit from `+3` to approximately `+10` |
| Correct release | One-shot `+10` after valid opening in the corridor |
| One-second stable placement | One-shot `+15`, followed immediately by terminal success `+100` |

Use best-so-far or one-shot terms so the policy cannot farm reward by oscillating, hovering, repeatedly touching the table, or reopening/regrasping.

### Priority 5: only then test longer credit assignment

After immediate success/failure termination and the post-lift bridge are in place, compare:

- `lambda = 0.95` baseline;
- `lambda = 0.98` intermediate trace, half-life about 0.84 seconds;
- `lambda = 0.995`, half-life about 3.14 seconds.

Do not combine a lambda change, terminal-bonus change, grasp cap, and learning-rate change in one arm. Higher lambda can help delayed credit, but it can also raise variance and make the critic less stable.

### Priority 6: keep `+100`; defer `+300`

Keep the terminal bonus at `+100` for the first corrected run. The expert already scores 344.57 under this contract, and a `+300` arm receives no additional signal until success occurs. Retest `+300` only after the corrected environment produces enough successful episodes to compare `P(full | >8 cm)` and placement stability with useful denominators.

## 9. Required reporting for the next run

Report completed-episode conditionals, not only “batches containing at least one event.” Use the same held-out references, randomization, duration mix, and seeds for every arm.

| Phase conditional | Diagnostic question |
|---|---|
| `P(hand contact | episode)` | Can the policy still acquire the can? |
| `P(opposed grasp | hand contact)` | Does contact become a mechanically useful grasp? |
| `P(all-three | opposed grasp)` | Is the intended three-digit topology formed? |
| `P(>3 cm lift | opposed grasp)` | Does the grasp bear load? |
| `P(>8 cm | >3 cm)` | Does initial lift become sustained lift? |
| `P(transport | >8 cm)` | Can the policy carry after lifting? |
| `P(carry-safe | transport)` | Does transport avoid drops and wrong contacts? |
| `P(place | carry-safe transport)` | Does the can enter a valid landing? |
| `P(release | place)` | Does the hand release correctly? |
| `P(stable 1 s | release)` | Does the placement settle? |
| `P(full success | >8 cm)` | End-to-end post-lift conversion |
| `P(contact violation | >8 cm)` | Post-lift collision failure rate |
| `P(carry violation | >8 cm)` | Post-lift grip/carry failure rate |

Also log per-episode reward-component sums. A single total reward curve cannot tell whether improvement came from grasp farming, real carry progress, placement, or terminal success.

## 10. Proposed experiment sequence

| Experiment | Change from parent | Purpose | Promotion criterion |
|---|---|---|---|
| A. Stabilization | LR `3e-6`; GRU frozen or lower LR; KL guard | Confirm destructive drift is controlled | No material held-out grasp/lift regression across three seeds |
| B. Termination correction | Immediate success and first-disqualifier termination; immediate penalties | Put credit/blame at the correct frames | Lower post-lift violation rates without reducing lift acquisition |
| C. Reward rebalance | Cap grasp totals; add bounded carry/descent/release/settle milestones | Convert lift into safe placement | Higher `P(place | carry-safe transport)` and `P(full | >8 cm)` |
| D. GAE sweep | Lambda `0.95`, `0.98`, `0.995` with all else fixed | Test longer credit assignment | Better pooled full-success Wilson interval without excess variance |
| E. Terminal sweep | `+100` versus `+300`, only after successes are frequent | Test terminal magnitude honestly | Better held-out full success, not merely higher training return |

No arm should be promoted from training reward alone. Require repeated held-out simulator grasp, lift, hold, transport, release, and stable placement, with denominators and failure causes, before considering physical transfer.

## Source snapshot

- [`grip_reward.py`](../outputs/g1-u2525-hparam-sweep-proper-20260915/source/grip_reward.py)
- [`palm_reward.py`](../outputs/g1-u2525-hparam-sweep-proper-20260915/source/palm_reward.py)
- [`stage_reward.py`](../outputs/g1-u2525-hparam-sweep-proper-20260915/source/stage_reward.py)
- [`contact_policy.py`](../outputs/g1-u2525-hparam-sweep-proper-20260915/source/contact_policy.py)
- [`cpu_environment.py`](../outputs/g1-u2525-hparam-sweep-proper-20260915/source/cpu_environment.py)
- [`policy.py`](../outputs/g1-u2525-hparam-sweep-proper-20260915/source/policy.py)
- [`cpu_run.py`](../outputs/g1-u2525-hparam-sweep-proper-20260915/source/cpu_run.py)
