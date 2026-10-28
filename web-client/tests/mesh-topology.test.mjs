import assert from "node:assert/strict";
import test from "node:test";
import {
  buildMeshGroups,
  collectMeshSnapshots,
  isStale,
  mergeMeshObservation,
  meshIdentity,
  nodeState,
  routeState,
} from "../lib/mesh-topology.ts";

const tenant = "11111111-1111-4111-8111-111111111111";
const otherTenant = "22222222-2222-4222-8222-222222222222";
const a = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const b = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
const c = "cccccccc-cccc-4ccc-8ccc-cccccccccccc";
const principal = (id, group = tenant) =>
  `spiffe://wendy.sh/tenant/${group}/device/${id}`;
const device = (id, group = tenant) => ({
  id,
  name: id.slice(0, 8),
  organizationId: group,
  pkiDeviceName: id,
});
const snapshot = (id, extra = {}, group = tenant) => ({
  selfPrincipal: principal(id, group),
  selfTenantId: group,
  selfDeviceId: id,
  capturedAtUnixMs: "1000",
  runtimeAvailable: true,
  status: { state: "mesh-active" },
  ...extra,
});
const observe = (topology, time = 1000) =>
  mergeMeshObservation(undefined, { status: "ok", topology }, time);

test("tenant graphs require matching canonical identity and reject foreign links", () => {
  const observations = {
    [a]: observe(
      snapshot(a, {
        peers: [
          { principal: principal(b), transport: "ethernet" },
          { principal: principal(c, otherTenant) },
        ],
      }),
    ),
    [c]: observe(snapshot(c, {}, otherTenant)),
  };
  const { groups, unknown } = buildMeshGroups(
    [
      device(a),
      device(c, otherTenant),
      { id: "legacy", organizationId: "77", pkiDeviceName: "15" },
    ],
    observations,
    2000,
  );
  assert.equal(groups.length, 2);
  assert.deepEqual(
    groups[0].nodes.map((n) => n.deviceId),
    [a, b],
  );
  assert.equal(groups[0].links.length, 1);
  assert.equal(groups[1].links.length, 0);
  assert.deepEqual(
    unknown.map((d) => d.id),
    ["legacy"],
  );
  assert.equal(meshIdentity("spiffe://wendy.sh/tenant/77/device/15"), null);
  assert.equal(meshIdentity(`${principal(a)}/extra`), null);
  const conflict = buildMeshGroups(
    [device(a)],
    { [a]: observe(snapshot(a, { selfTenantId: otherTenant })) },
    2000,
  );
  assert.equal(conflict.groups.length, 0);
  assert.equal(conflict.unknown.length, 1);
});

test("learned destinations are not direct links and reciprocal sessions collapse to one edge", () => {
  const observations = {
    [a]: observe(
      snapshot(a, {
        peers: [
          { principal: principal(b), transport: "ethernet" },
          { principal: principal(b), transport: "wifi" },
        ],
        routes: [
          {
            originPrincipal: principal(c),
            nextHopPrincipal: principal(b),
            originAuthorized: true,
          },
        ],
        announcements: [{ principal: principal(c), name: "Indirect device" }],
      }),
    ),
    [b]: observe(snapshot(b, { peers: [{ principal: principal(a) }] })),
  };
  const group = buildMeshGroups([device(a), device(b)], observations, 2000)
    .groups[0];
  assert.equal(group.nodes.length, 3);
  assert.equal(group.links.length, 1);
  assert.equal(group.links[0].observations.length, 3);
  assert.equal(
    group.links.some((link) =>
      [link.source, link.target].includes(principal(c)),
    ),
    false,
  );
});

test("failed reads preserve stale evidence; successful disabled snapshots clear it", () => {
  const previous = observe(
    snapshot(a, { peers: [{ principal: principal(b) }] }),
  );
  const failure = mergeMeshObservation(
    previous,
    { status: "unavailable", error: "Timed out" },
    2000,
  );
  assert.equal(isStale(failure, 2000), true);
  assert.equal(failure.receivedAt, 1000);
  assert.equal(failure.topology.peers.length, 1);
  assert.equal(isStale(previous, 31_001), true);
  const disabled = mergeMeshObservation(
    failure,
    {
      status: "ok",
      topology: snapshot(a, {
        runtimeAvailable: false,
        status: { state: "disabled" },
      }),
    },
    3000,
  );
  const group = buildMeshGroups([device(a)], { [a]: disabled }, 4000).groups[0];
  assert.equal(group.links.length, 0);
  assert.equal(nodeState(group.nodes[0], 4000).label, "Disabled");
  const malformed = mergeMeshObservation(previous, { status: "ok" }, 3000);
  assert.equal(malformed.result.status, "error");
  assert.equal(isStale(malformed, 3000), true);
});

test("unsupported and denied observations do not claim a device is offline", () => {
  for (const [status, label] of [
    ["unsupported", "Unsupported"],
    ["unauthorized", "Access denied"],
    ["error", "Read failed"],
  ]) {
    const observation = mergeMeshObservation(undefined, { status }, 1000);
    const group = buildMeshGroups([device(a)], { [a]: observation }, 2000)
      .groups[0];
    assert.equal(nodeState(group.nodes[0], 2000).label, label);
    assert.equal(group.links.length, 0);
  }
});

test("re-reading a cached runtime snapshot does not reset its capture age", () => {
  const first = observe(snapshot(a), 1000);
  const repeated = mergeMeshObservation(
    first,
    { status: "ok", topology: snapshot(a) },
    40_000,
  );
  assert.equal(isStale(repeated, 40_000), true);
  for (const capturedAtUnixMs of [undefined, "invalid", "0", "90000"]) {
    assert.equal(
      isStale(observe(snapshot(a, { capturedAtUnixMs }), 40_000), 40_000),
      true,
    );
  }
  const disabled = observe(
    snapshot(a, {
      capturedAtUnixMs: undefined,
      runtimeAvailable: false,
      status: { state: "disabled" },
    }),
    40_000,
  );
  assert.equal(isStale(disabled, 40_000), false);
});

test("disabled control runtime needs fresh reads but no node capture timestamp", () => {
  const disabled = mergeMeshObservation(
    undefined,
    {
      status: "ok",
      observedAt: new Date(40_000).toISOString(),
      topology: snapshot(a, {
        runtimeAvailable: false,
        capturedAtUnixMs: "0",
        status: {
          runtimeAvailable: true,
          state: "disabled",
          configured: { participate: false },
        },
      }),
    },
    40_000,
  );
  const group = buildMeshGroups([device(a)], { [a]: disabled }, 40_000)
    .groups[0];
  assert.equal(isStale(disabled, 40_000), false);
  assert.equal(nodeState(group.nodes[0], 40_000).label, "Disabled");
  assert.equal(isStale(disabled, 70_001), true);
  assert.equal(nodeState(group.nodes[0], 70_001).label, "Stale");

  const failure = mergeMeshObservation(
    disabled,
    { status: "unavailable", error: "Timed out" },
    41_000,
  );
  assert.equal(failure.topology, disabled.topology);
  assert.equal(failure.receivedAt, 40_000);
  assert.equal(isStale(failure, 41_000), true);
  assert.equal(
    nodeState({ ...group.nodes[0], observation: failure }, 41_000).label,
    "Stale",
  );
});

test("active or retained node snapshots still require valid capture times", () => {
  for (const capturedAtUnixMs of [undefined, "0", "invalid", "1000", "90000"]) {
    // An active-state claim still requires a capture even if the top-level
    // flag was omitted or incorrectly returned false.
    assert.equal(
      isStale(
        observe(snapshot(a, { runtimeAvailable: false, capturedAtUnixMs }), 40_000),
        40_000,
      ),
      true,
    );
    // Disabling participation cannot make retained, old runtime data fresh.
    assert.equal(
      isStale(
        observe(
          snapshot(a, {
            capturedAtUnixMs,
            status: { state: "disabled", configured: { participate: false } },
          }),
          40_000,
        ),
        40_000,
      ),
      true,
    );
  }
});

test("same principal reached by two targets keeps its newest successful snapshot", () => {
  const group = buildMeshGroups(
    [{ id: "cloud" }, { id: "lan" }],
    {
      cloud: observe(snapshot(a), 2000),
      lan: observe(snapshot(a), 1000),
    },
    3000,
  ).groups[0];
  assert.equal(group.nodes.length, 1);
  assert.equal(group.nodes[0].inventory.id, "cloud");
});

test("route proof expiry and unreachable withdrawals override prior authorization", () => {
  assert.equal(
    routeState(
      { originAuthorized: true, signedRouteExpiresAtUnixMs: "2000" },
      1000,
    ),
    "Authorized at snapshot",
  );
  assert.equal(
    routeState(
      { originAuthorized: true, signedRouteExpiresAtUnixMs: "2000" },
      2000,
    ),
    "Proof expired",
  );
  assert.equal(
    routeState({ metric: 65535, originAuthorized: true }, 1000),
    "Unreachable",
  );
  assert.equal(routeState({}, 1000), "Origin unverified");
  assert.equal(
    routeState({ originAuthorized: true }, 1000),
    "Proof expiry unknown",
  );
  assert.equal(
    routeState(
      { originAuthorized: true, signedRouteExpiresAtUnixMs: "invalid" },
      1000,
    ),
    "Proof expiry unknown",
  );
});

test("collection bounds concurrency, retains per-device failures and ignores cancelled replies", async () => {
  const devices = Array.from({ length: 10 }, (_, index) => ({
    id: String(index),
  }));
  let active = 0,
    maximum = 0;
  const received = [];
  await collectMeshSnapshots(
    devices,
    async (id) => {
      maximum = Math.max(maximum, ++active);
      await new Promise((resolve) => setTimeout(resolve, 3));
      active--;
      if (id === "2") throw new Error("denied by connection");
      return { status: "unsupported" };
    },
    (id, result) => received.push([id, result]),
    new AbortController().signal,
  );
  assert.equal(maximum, 1);
  assert.equal(received.length, 10);
  assert.equal(received.find(([id]) => id === "2")[1].status, "error");

  const controller = new AbortController();
  let calls = 0;
  const late = [];
  await collectMeshSnapshots(
    devices,
    async () => {
      calls++;
      await new Promise((resolve) => setTimeout(resolve, 3));
      controller.abort();
      return { status: "ok" };
    },
    (...args) => late.push(args),
    controller.signal,
  );
  assert.equal(calls, 1);
  assert.equal(late.length, 0);
});
