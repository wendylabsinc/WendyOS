# LiDAR rotation alignment investigation

## Report

After confirming Go2 localization and successfully navigating, the operator observed that the robot's position matches the room but the LiDAR geometry is rotated relative to the captured room. Live red obstacle footprints and accumulated scan points must be checked independently.

Navigation destination selection is fixed in #2723. This follow-up tracks the rotation mismatch. It also removes implicit SceneKit pose animation after a separate report of the Go2 asset flickering around zero heading. The rendering change does not alter alignment or robot control.

## Current code path

- Localization creates an odometry-to-room alignment from the matched position and rotation. Confirmation installs that alignment on the Linux controller.
- Robot pose, live hazard centres, and mapping contributions use the same `Alignment.ToSite` transform. Room rendering converts planar Y to negative scene Z.
- Hazard corners apply the alignment rotation in the renderer.
- Persisted scan contributions are merged into room voxels. They currently retain no per-alignment provenance, so subsequent confirmation cannot reproject observations from an earlier alignment.

These observations do not yet establish which path caused the reported mismatch.

## Verification and fix criteria

1. Capture paired odometry, scan data, confirmed alignment, and model geometry without issuing movement commands.
2. Compare current transformed scan points and hazard footprints with the room, separately from persisted points.
3. Reproduce the mismatch with a deterministic replay before changing rotation conventions or matching behavior.
4. Verify the correction across nonzero translations and headings, including turns after confirmation. Keep navigation, hazards, robot rendering, and mapping in one frame.
5. If historical contributions are responsible, retain recoverable source observations and correct their registration rather than deleting the room or hiding evidence.
6. Add regression coverage, rebuild the clients and Linux service as required, and verify on the operator's room. Record hardware results in the draft PR.

Mesh refinement and Shift-click waypoint routes are separate requested follow-up work.
