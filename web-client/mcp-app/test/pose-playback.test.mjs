import assert from "node:assert/strict";
import test from "node:test";
import { build } from "esbuild";
const compiled = await build({
  stdin: {
    contents: `export {posePlayback, SandboxViewer} from '../../../go/simulator/go2/go2_sim/viewer.js';export * as THREE from '../../../go/simulator/go2/go2_sim/vendor/three.module.js';`,
    resolveDir: new URL(".", import.meta.url).pathname,
    loader: "js",
  },
  bundle: true,
  write: false,
  format: "esm",
  platform: "node",
});
const { posePlayback, SandboxViewer, THREE } = await import(
  "data:text/javascript;base64," + Buffer.from(compiled.outputFiles[0].text).toString("base64")
);
const pose = (received) => ({ received, time: received / 1000 });
const value = ({ before, after, alpha }) => before.time + (after.time - before.time) * alpha;

test("interpolation uses closely spaced capture times and never spans long gaps", () => {
  assert.equal(value(posePlayback([0, 50].map(pose), 90)), 0.025);
  assert.equal(value(posePlayback([0, 350].map(pose), 240)), 0);
  assert.equal(value(posePlayback([0, 350].map(pose), 415)), 0.35);
  const mismatchedPhysics = [pose(0), { received: 50, time: 1 }];
  assert.equal(value(posePlayback(mismatchedPhysics, 90)), 0);
});

test("jitter never reverses playback and outages hold the final received pose", () => {
  const samples = [0, 250, 500].map(pose);
  const beforeJitter = posePlayback(samples, 740);
  samples.push(pose(900));
  const afterJitter = posePlayback(samples, 900, beforeJitter.target);
  assert.ok(value(afterJitter) >= value(beforeJitter));
  assert.equal(value(posePlayback(samples, 5000, afterJitter.target)), 0.9);
  assert.equal(value(posePlayback([pose(10)], 1000)), 0.01);
});

function viewerFixture() {
  return Object.assign(Object.create(SandboxViewer.prototype), {
    replayHistory: true, lastSequence: 0, sceneId: "fixture", samples: [],
    bodies: [{ position: new THREE.Vector3(), quaternion: new THREE.Quaternion() }],
    nextPosition: new THREE.Vector3(), nextQuaternion: new THREE.Quaternion(),
    active: true, canvas: { dataset: {} }, status: {}, robotIndex: -1,
    lidar: { setStateIdentity() {}, setActive() {}, update() { return false; } },
    controls: { update() { return false; } },
    renderer: { shadowMap: {}, render() {} },
  });
}
const frame = (sequence, captured, height = 0, epoch = 1) => ({
  sequence, captured_ms: captured,
  state: { scene_id: "fixture", epoch, generation: 1, time: captured / 1000,
    positions: [0, 0, height], quaternions: [0, 0, 0, 1] },
});

test("a foot lift and landing between host polls are replayed from intermediate captures", (t) => {
  const original = globalThis.document;
  globalThis.document = { hidden: false };
  t.after(() => { globalThis.document = original; });
  const viewer = viewerFixture();
  viewer.acceptBatch({ frames: [frame(1, 0)] });
  viewer.draw(0);
  viewer.draw(200);
  assert.equal(viewer.playbackTarget, 0, "Waiting for a tool reply must not advance past captured history");
  viewer.acceptBatch({ frames: [frame(2, 33, 0.18), frame(3, 66)] });
  viewer.draw(233);
  assert.equal(viewer.bodies[0].position.z, 0.18, "The intermediate foot-off-ground pose must actually render");
  viewer.draw(266);
  assert.equal(viewer.bodies[0].position.z, 0, "The following landing must render too");
});

test("replayed poses deduplicate replies and clear on physics resets or suspension", () => {
  const viewer = viewerFixture();
  const packet = { frames: [frame(1, 0), frame(2, 33, 0.18)] };
  viewer.acceptBatch(packet);
  viewer.acceptBatch(packet);
  assert.equal(viewer.samples.length, 2);
  viewer.acceptBatch({ frames: [frame(3, 66, 0, 2)] });
  assert.equal(viewer.samples.length, 1);
  assert.equal(viewer.playbackTarget, undefined);
  viewer.setActive(false);
  assert.equal(viewer.samples.length, 0);
  assert.equal(viewer.replayTime, undefined);
});

test("a slow render frame cannot jump past a captured foot lift", (t) => {
  const original = globalThis.document;
  globalThis.document = { hidden: false };
  t.after(() => { globalThis.document = original; });
  const viewer = viewerFixture();
  viewer.acceptBatch({ frames: [frame(1, 0), frame(2, 33, 0.18), frame(3, 66)] });
  viewer.draw(0);
  viewer.draw(200);
  assert.equal(viewer.bodies[0].position.z, 0.18);
  viewer.draw(400);
  assert.equal(viewer.bodies[0].position.z, 0);
});

test("the first pose of a late batch is displayed before later captures", (t) => {
  const original = globalThis.document;
  globalThis.document = { hidden: false };
  t.after(() => { globalThis.document = original; });
  const viewer = viewerFixture();
  viewer.lastDrawTime = 100;
  viewer.acceptBatch({ frames: [frame(1, 0, 0.18), frame(2, 33)] });
  viewer.draw(300);
  assert.equal(viewer.bodies[0].position.z, 0.18);
});

test("missing history is explicit and malformed capture ordering is rejected", () => {
  const viewer = viewerFixture();
  viewer.acceptBatch({ frames: [frame(1, 0), frame(2, 33)] });
  viewer.acceptBatch({ dropped: true, frames: [frame(10, 1000)] });
  assert.equal(viewer.samples.length, 1);
  assert.match(viewer.messageText, /movement history unavailable/);
  assert.throws(() => viewer.acceptBatch({ frames: [frame(11, 999)] }), /timestamps/);
});
