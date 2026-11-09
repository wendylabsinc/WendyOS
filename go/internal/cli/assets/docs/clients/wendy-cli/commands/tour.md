# `wendy tour`

Watch a simulated Go2 walk in your browser, or set up your first physical Wendy device.

## Choose your path

```sh
wendy tour
```

In an interactive terminal, **Walk a simulated robot** appears first and is
selected by default. Choose **Set up a physical device** when you have hardware ready.

You can select either route directly:

```sh
wendy tour --simulator
wendy tour --hardware
```

## Simulated robot

The managed Go2 virtual device requires macOS or Linux, QEMU with ARM64 firmware,
and a Docker builder with Buildx for the initial build or runtime updates. It uses
four virtual CPUs and 4 GiB of memory. Apple Silicon uses hardware acceleration;
x86 hosts use slower emulation. The first run downloads the OS and robot assets
and builds the runtime.
Allow several minutes and disk space for the images. No physical robot, cloud
account, or host Python installation is required.

On macOS with Homebrew, the tour offers to install QEMU and repair missing
firmware. On Linux with apt-get or dnf, it offers to install QEMU and its
firmware; that install runs `sudo`, which may ask for your password. On macOS it
also offers to install or start a Docker runtime when a build needs one; on
Linux, install and start Docker yourself. These prompts default to Yes. A healthy
existing simulation can run again without starting Docker.

See the [quickstart](/docs/getting-started/robot) for prerequisites. Windows users
can use the physical-device route or the separate
[local Docker robot demo](https://wendy.dev/quickstart?target=simulator).
For device inspection with a generic virtual device on native Windows, see
[Windows foreground VMs](/docs/installation/wendyos-virtual-machine#windows-foreground-vms).
To build and deploy local apps to a VM on Windows, run the Linux Wendy CLI, QEMU,
and your container builder inside WSL2.

The tour creates a dedicated Go2 VM and opens its browser viewer. The robot walks
a short arc. Success requires a measured change in its position and confirmation
that the movement stopped. An interactive guide then helps you program the robot,
change its walk, or drive it in the browser.

### Program your robot

Choose **Build a robot app** after the first walk. Pick an idea:

| App | What you can build |
| --- | --- |
| Patrol | Follow a square or your own waypoints. |
| Roam | React to lidar readings and avoid obstacles. |
| Teleop | Build a browser remote control. |
| Sensors | Display the camera, lidar, and robot state. |

The guide creates an editable Python project under your tour's `apps` directory,
shows you a first change to make, and offers to open the code in your editor.
Choose **Deploy** to run it on this tour's virtual Go2, then edit and deploy again
without leaving the guide. Returning to a project preserves your edits.

These apps use ROS 2 to read sensors and send movement commands. The simulator
handles balance and walking. Driving apps have explicit controls; deploying
Patrol alone does not start its route. The next menu selects **Start** or **Open
your app** for you. Returning to the main guide stops movement apps and releases
their robot controls, so you can try another idea or drive in the viewer. Your code
stays saved for the next deployment.

Choose **Finish for now** when done. The guide offers to stop the simulator or
leave it running, and shows how to resume later. Ctrl+C exits the current tour.

### Change the movement and repeat

Choose **Change the walk** to try a straight line or turn the other way immediately.
The tour saves your choice in `~/Documents/wendy-robot-tour/tour.json`. You can also
edit `turn_rps` to `0` for a straight walk, then rerun:

```sh
wendy tour --simulator
```

This reuses the saved VM. You can edit `duration_seconds` within the supported
range of 1 to 8 seconds, or `turn_rps` between -1 and 1. The starter's
`forward_mps` is bounded between 0.55 and 0.6 metres per second. These settings
control the simulated robot only.

To keep a separate starter and VM:

```sh
wendy tour --simulator --project ./my-robot-tour
```

If a step fails in an interactive terminal, the tour explains the error and offers
to retry that step. Fix the issue and press Enter to continue, or decline and
resume later with the same command. Existing VMs are reused. If VM creation was
interrupted, the tour preserves the incomplete files and restarts creation under
the saved VM name. If another controller owns the simulated robot, release
its controls in the viewer before retrying. Ctrl+C interrupts the demonstration;
use the printed VM stop command when you also want to shut down its virtual device.

## Physical device

`wendy tour --hardware` guides you through device setup, discovery, optional
Wi-Fi configuration, and a sample app. Use an interactive terminal. Check the
[board-specific setup guide](/docs/installation) for storage, power,
recovery mode, and cable requirements. Review the selected storage before
confirming a flash, which erases that storage.

At the Wi-Fi step, the wizard tries to prefill the host computer's current SSID.
If it cannot detect one, enter the network name yourself. Cloud enrollment and
coding-agent setup are optional. The physical-device route does not command
robot motion.

### Cancellation and exit status

Declining an installer's erase confirmation cancels that setup step. The tour
does not treat it as a completed installation or proceed to first-boot discovery.

An installer launched by the tour returns exit status `130` to its parent when
cancelled. The tour translates that internal signal into user cancellation. A
normally launched `wendy tour` exits with status `0` for this cancellation, as it
does when you quit the wizard, so status `0` alone does not prove setup completed.
Other failures use the CLI's [exit status contract](../global-flags.md#exit-status).
