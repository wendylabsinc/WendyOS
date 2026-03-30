# wendy.conf Infrastructure — Pairing Plan

Linear: https://linear.app/wendylabsinc/issue/WDY-779/implement-wendyconf-infrastructure

**How we work:** Before touching anything in a phase, I explain what I'm
about to do and why. Then I do it and report what I found or built. If
anything comes back unexpected — build failure, wrong package name, surprising
service ordering, anything — I stop and we decide together before continuing.
You only need to be hands-on for the two hardware steps (flash + boot).

This is Part 2 of three. Part 1 (partition) is done. Part 3 (WiFi handler)
follows. This part builds the framework Part 3 plugs into.

> **Two repos.** Phases 1–3 are in this worktree (`WendyOS`, Yocto).
> Phase 4 is in `wendy-agent` (Go CLI).

---

## Commit strategy

Each incremental change gets its own commit immediately — regardless of
whether the tree is buildable at that point. The commit sequence is the
primary narrative: a reader should be able to follow the work file-by-file
without needing the surrounding context. Commit bodies capture *why*
decisions were made and surface any unexpected findings.

Commits marked **[if]** happen only when the relevant situation arises.
All commits are shown after confirmation — no commit lands without a
thumbs-up.

---

## Stop conditions

I stop and ask before proceeding if:
- A build fails or produces unexpected warnings
- The package name for `findfs` or `mount` in Yocto differs from what the
  recipe draft assumes
- The service unit's boot ordering produces a dependency cycle or unexpected
  boot delay
- Any file I'm about to create already exists with conflicting content
- Anything in the hardware verification output doesn't match expectations

---

## Phase 1 — Discover, then build the recipe

**What I'll explain before starting:**
The recipe is platform-agnostic so it goes in the top-level `recipes-core/`,
not under `meta-rpi-extensions` or `meta-tegra-extensions`. I'll read the
current WKS, `partuuid-rpi.bbclass`, and the base packagegroup to confirm
the baseline, then identify the exact Yocto package names that provide
`findfs` and `mount` by checking `oe-pkgdata-util` inside the build container.
I'll explain any finding that differs from the plan doc assumptions before
writing the recipe.

**What I'll do:**
1. Read the current WKS and confirm WENDYCONFIG is present as p2
2. Identify the correct package name(s) for `findfs` and `mount`
3. Write the four files:
   - `recipes-core/wendy-config/wendy-config_1.0.bb`
   - `recipes-core/wendy-config/files/wendy-config.service`
   - `recipes-core/wendy-config/files/wendy-config.sh`
   - `recipes-core/wendy-config/files/wendy-config-lib.sh`
4. Add `wendy-config` to `packagegroup-wendyos-base.bb`
5. Run `make build MACHINE=raspberrypi5-nvme-wendyos`
6. Verify the installed file list and `sysinit.target.wants` symlink

**What I'll report:**
- The confirmed `findfs` / `mount` package names and whether they match the
  draft RDEPENDS
- The full file list from `oe-pkgdata-util list-pkg-files wendy-config`
- Any build warnings
- Confirmation that the `sysinit.target.wants` symlink is present

**Commits in this phase:**

> **Commit A — lib**
> Staged: `recipes-core/wendy-config/files/wendy-config-lib.sh`
> ```
> Add wendy-config-lib.sh with wc_log, wc_stamp, wc_run_handlers
>
> Foundation helpers sourced by the main script. wc_run_handlers is a
> no-op stub in Part 2; Part 3 (WiFi) extends it here as a pure
> addition without touching wendy-config.sh.
> ```

> **Commit B — main script**
> Staged: `recipes-core/wendy-config/files/wendy-config.sh`
> ```
> Add wendy-config.sh provisioning script
>
> Implements the full first-boot lifecycle: locate WENDYCONFIG by FAT
> volume label, mount read-only, copy wendy.conf to tmpfs, remount
> read-write, zero-overwrite then delete the original, run handlers,
> write stamp. Label-based lookup avoids baking a build-time PARTUUID
> into the rootfs.
> ```

> **Commit C — service unit**
> Staged: `recipes-core/wendy-config/files/wendy-config.service`
> ```
> Add wendy-config.service systemd unit
>
> Runs as a oneshot at sysinit.target, after local-fs.target and
> systemd-udev-settle so block devices are stable, and before
> basic.target so provisioning is invisible to user-facing services.
> ConditionPathExists fast-path skips the service entirely on every
> boot after the first without spawning a process.
> ```

> **Commit D — recipe**
> Staged: `recipes-core/wendy-config/wendy-config_1.0.bb`
> ```
> Add wendy-config_1.0.bb Yocto recipe
>
> Packages the service unit, main script, and lib into the wendy-config
> package. Placed in recipes-core/ rather than a machine extension
> because the logic is platform-agnostic across RPi5 and Jetson.
> RDEPENDS on util-linux-findfs and util-linux-mount; flagged for
> correction if oe-pkgdata-util shows different package names.
> ```

> **Commit E — packagegroup**
> Staged: `packagegroup-wendyos-base.bb`
> ```
> Include wendy-config in packagegroup-wendyos-base
>
> Ensures the service and scripts are installed in all WendyOS images
> regardless of machine target.
> ```

> **Commit E′ [if RDEPENDS needed correction]**
> Only if `oe-pkgdata-util` shows the package names differ from the
> draft assumptions.
> ```
> Fix wendy-config RDEPENDS to match actual Yocto package names
>
> oe-pkgdata-util showed findfs ships in <actual-pkg> and mount in
> <actual-pkg>, differing from the plan's assumptions.
> ```

---

## Phase 2 — Hardware: "nothing to do" path  *(you flash and boot)*

**What I'll explain before handing off:**
I'll walk through what the service should do when it finds no `wendy.conf`
on the partition, what the expected journal output looks like, and what a
pass vs. fail looks like for each check. I'll write a single verification
script you can run over SSH so you don't have to type individual commands.

**What you do:**
1. `make flash-to-external MACHINE=raspberrypi5-nvme-wendyos`
2. Boot the RPi5, SSH in
3. Run the verification script I provide; paste its output here

**What I'll check in the output:**
- `systemctl status wendy-config.service` → `active (exited)`, exit code 0
- Journal shows the "no partition found" or "no wendy.conf" log line
- `/var/lib/wendy-config.done` exists
- `systemctl --failed` is empty
- Service does not re-run on a second boot (stamp file guard)

**Commits in this phase:**

No planned commits — this is a verification-only step.

> **Commit E″ [if] — service fix from hardware**
> Only if the service misbehaves and requires a code change.
> ```
> Fix wendy-config early-boot issue: <concise description>
>
> Hardware boot revealed <what went wrong>. <Why this fix addresses it>.
> ```

---

## Phase 3 — Hardware: full provisioning lifecycle  *(you write a file, flash, boot)*

**What I'll explain before handing off:**
I'll explain exactly what `wendy-config.sh` does step by step — mount
read-only, copy to temp, remount read-write, zero-overwrite then delete,
sync, unmount, run handlers (empty at this point), write stamp — so you
know what to look for in the journal. I'll write the verification script.

**What you do:**
1. Write a test `wendy.conf` to `/Volumes/WENDYCONFIG` from macOS:
   ```bash
   printf '[test]\nkey=hello\n' > /Volumes/WENDYCONFIG/wendy.conf
   diskutil unmount /Volumes/WENDYCONFIG
   ```
2. Boot the RPi5, SSH in
3. Run the verification script I provide; paste its output here

**What I'll check in the output:**
- Journal shows the full sequence: found → wiped → handlers (none) → stamp
- `wendy.conf` is absent from the partition after boot
- Stamp file present; service skips cleanly on second boot

**Commits in this phase:**

No planned commits — verification only.

> **Commit E‴ [if] — wipe or stamp fix from hardware**
> Only if the provisioning lifecycle reveals a defect.
> ```
> Fix wendy-config provisioning: <concise description>
>
> Full lifecycle run showed <what went wrong>. <Why this fix>.
> ```

---

## Phase 4 — Go CLI (`wendy-agent` repo)

**What I'll explain before starting:**
The one structural wrinkle: `diskutil eject` currently lives inside
`writeImageToDisk` in `disklister_darwin.go`. The WENDYCONFIG write needs
to happen between `dd` completing and the eject. I'll explain the exact
diff to `disklister_darwin.go` and `disklister_linux.go` before touching
them, so you can confirm the approach. Then I'll proceed with all five
changes: pull the eject out, write the three new files
(`wendy_config.go`, `wendy_config_darwin.go`, `wendy_config_linux.go`),
and add the post-dd hook and `collectWendyConf` stub to `os_install.go`.

**What I'll do:**
1. Explain the eject-decoupling diff; wait for a thumbs-up before touching
   `disklister_darwin.go` / `disklister_linux.go`
2. Write `go/internal/cli/commands/wendy_config.go` (shared `formatWendyConf`)
3. Write `go/internal/cli/commands/wendy_config_darwin.go`
4. Write `go/internal/cli/commands/wendy_config_linux.go`
5. Edit `os_install.go`: add post-dd hook and `collectWendyConf` stub
6. `go build ./...` and `go vet ./...`

**What I'll report:**
- The eject-decoupling diff for your review before it's applied
- `go build` / `go vet` output
- Confirmation that `collectWendyConf` returns nil (no-op) until Part 3,
  meaning `wendy os install` behaviour is unchanged for now

**Commits in this phase:**

Three commits after `go build ./...` and `go vet ./...` pass clean.

> **Commit F — decouple eject**
> Staged: `disklister_darwin.go`, `disklister_linux.go`
> ```
> Decouple eject from writeImageToDisk in disk lister
>
> The WENDYCONFIG write must happen between dd completing and the disk
> being ejected. Moving eject to the call site in os_install.go makes
> that sequencing explicit and keeps writeImageToDisk focused on the
> write operation alone.
> ```

> **Commit G — wendy_config Go package**
> Staged: `wendy_config.go`, `wendy_config_darwin.go`, `wendy_config_linux.go`
> ```
> Add wendy_config helpers for writing wendy.conf to WENDYCONFIG
>
> Shared formatter plus platform-specific implementations for macOS
> (diskutil mount/unmount) and Linux (standard mount). Isolated in
> their own files so Part 3 (WiFi) can extend formatWendyConf without
> touching os_install.go.
> ```

> **Commit H — os_install.go hook**
> Staged: `os_install.go`
> ```
> Wire wendy.conf post-dd hook into os_install, stub collectWendyConf
>
> Hook runs after dd and before eject. collectWendyConf returns nil
> until Part 3 provides real content, so wendy os install behaviour
> is unchanged for now. The sequencing and interface are in place for
> Part 3 to activate with a pure addition.
> ```

---

## Done

- `wendy-config.service` running early in boot on both RPi5 and Jetson
- Full lifecycle verified on hardware: locate → read → wipe → handlers → stamp
- Idempotency verified: stamp prevents re-run
- Go CLI plumbing in place and dormant: `writeWendyConf`,
  `waitForWendyConfigVolume`, `collectWendyConf` stub, post-dd hook
- Eject decoupled from `writeImageToDisk`
- Part 3 (WiFi) can be added as a pure addition with no structural changes
- Commit history (A–H) tells the full story: rationale, package-name
  findings, hardware results, and any fixups — readable by anyone
  picking up Part 3
