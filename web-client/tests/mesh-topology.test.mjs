import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import {
  buildMeshGroups,
  collectMeshSnapshots,
  isStale,
  mergeMeshObservation,
  mergeMeshInventory,
  meshIdentity,
  meshNodeFor,
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
  assert.equal(maximum, 3);
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
  assert.equal(calls, 3);
  assert.equal(late.length, 0);
});


test("one slow reporter does not block others and aliases stay serialized",async()=>{
 let release;const slow=new Promise(resolve=>{release=resolve});let active=0,maxAlias=0;const received=[];
 const done=collectMeshSnapshots([{id:"c6-lan",reporterKey:"c6"},{id:"c6-cloud",reporterKey:"c6"},{id:"os"}],async id=>{
  if(id.startsWith("c6")){maxAlias=Math.max(maxAlias,++active);await slow;active--}
  return {status:"ok"};
 },id=>received.push(id),new AbortController().signal);
 await new Promise(resolve=>setImmediate(resolve));assert.deepEqual(received,["os"]);release();await done;assert.equal(maxAlias,1);assert.equal(received.length,3);
});


const localTopology = JSON.parse(readFileSync(new URL("./fixtures/operator-topology.json", import.meta.url), "utf8"));
const localReporter = { id: "cli://local", name: "This computer's CLI router", platform: "wendy-cli" };

test("actual Go Node projection renders an operator full router and installation IDs", () => {
 const now=Number(localTopology.capturedAtUnixMs);
 const observation=mergeMeshObservation(undefined,{status:"ok",platform:"wendy-cli",topology:localTopology},now);
 const {groups,unknown}=buildMeshGroups([localReporter],{[localReporter.id]:observation},now);
 assert.deepEqual(unknown,[]);assert.equal(groups.length,1);
 const group=groups[0],self=group.nodes.find(node=>node.inventory?.id===localReporter.id);
 assert.equal(self.role,"operator");assert.equal(self.principal,localTopology.selfPrincipal);
 assert.equal(self.id,`${localTopology.selfPrincipal}#${localTopology.selfDeviceId}`);
 assert.equal(self.routingId,localTopology.selfDeviceId);assert.equal(self.deviceId,undefined);
 assert.equal(self.address,localTopology.selfMeshAddress);assert.match(self.address,/^fd45:b891:5680:/);
 assert.equal(group.links.length,2);assert.equal(group.nodes.filter(node=>node.role==="operator").length,2);
 const other=group.nodes.find(node=>node.role==="operator" && node.id!==self.id);
 assert.equal(other.principal,self.principal);assert.notEqual(other.routingId,self.routingId);
 assert.equal(meshNodeFor(group,other.principal,undefined,other.address)?.id,other.id);
 assert.equal(meshNodeFor(group,other.principal),undefined,"principal alone cannot choose between installations");
 assert.equal(meshIdentity(localTopology.selfPrincipal,localTopology.selfDeviceId).principal.endsWith("/Alice"),true);
 assert.equal(meshIdentity(localTopology.selfPrincipal.toLowerCase(),localTopology.selfDeviceId).id===self.id,false);
 const devicePeer=localTopology.peers.find(peer=>peer.principal.includes("/device/"));
 assert.equal(devicePeer.linkId,"18446744073709551615");
 assert.equal(localTopology.routes.find(route=>route.originPrincipal===devicePeer.principal).originRouterId,"18446744073709551614");
 assert.equal(typeof devicePeer.meshAddress,"string");
});

test("a busy C6 reporter stays unavailable while CLI and neighbor observations retain its links",()=>{
 const now=Number(localTopology.capturedAtUnixMs),c6=localTopology.peers.find(peer=>peer.principal.includes("/device/"));
 const inventory={id:"c6",name:"C6",organizationId:tenant,pkiDeviceName:c6.deviceId,platform:"wendy-lite"};
 const observations={
  [localReporter.id]:mergeMeshObservation(undefined,{status:"ok",platform:"wendy-cli",topology:localTopology},now),
  c6:mergeMeshObservation(undefined,{status:"unavailable",platform:"wendy-lite",error:"The native management slot is occupied."},now),
 };
 const {groups}=buildMeshGroups([localReporter,inventory],observations,now);
 const node=groups[0].nodes.find(node=>node.principal===c6.principal);
 assert.equal(nodeState(node,now).label,"Reporter unavailable");assert.equal(node.observation.topology,undefined);
 assert.ok(groups[0].links.some(link=>link.target===node.id || link.source===node.id));
 const partial=snapshot(c6.deviceId,{selfMeshAddress:c6.meshAddress,truncated:true,status:{state:"mesh-active",detail:"Partial endpoint directory."}});
 observations.c6=mergeMeshObservation(observations.c6,{status:"ok",platform:"wendy-lite",topology:partial},now+1);
 const updated=buildMeshGroups([localReporter,inventory],observations,now+1).groups[0].nodes.find(node=>node.principal===c6.principal);
 assert.equal(updated.observation.topology.truncated,true);
});

test("device inventory outage retains prior sources while the independent CLI reporter is queried",()=>{
 const old=[device(a),device(b)];
 const merged=mergeMeshInventory(old,[localReporter],true);
 assert.deepEqual(merged.map(item=>item.id),["cli://local",a,b]);
 assert.deepEqual(mergeMeshInventory(old,[localReporter]),[localReporter]);
});

const liteTopology = JSON.parse(readFileSync(new URL("./fixtures/lite-operator-topology.json", import.meta.url), "utf8"));
const c5Reporter = { id: "cloud://cloud.example:443/tenant/11111111-1111-4111-8111-111111111111/asset/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", name: "C5 Cloud", platform: "wendy-lite" };

test("actual nanopb wire through the production Go normalizer renders typed C5 operator origins", () => {
  const now = Number(liteTopology.capturedAtUnixMs);
  const observation = mergeMeshObservation(undefined, { status: "ok", platform: "wendy-lite", topology: liteTopology }, now);
  const { groups, unknown } = buildMeshGroups([c5Reporter], { [c5Reporter.id]: observation }, now);
  assert.deepEqual(unknown, []);
  const group = groups[0];
  const operators = group.nodes.filter(node => node.role === "operator");
  assert.equal(operators.length, 2);
  assert.equal(operators[0].principal, operators[1].principal);
  assert.notEqual(operators[0].id, operators[1].id);
  assert.deepEqual(operators.map(node => node.routingId).sort(), [a, c]);
  for (const node of operators) {
    assert.equal(node.deviceId, undefined, "installation is not an enrolled device");
    assert.match(node.address, /^fd45:b891:5680:/);
    assert.ok(node.name.startsWith("CLI "));
    assert.equal(node.agentPort, undefined);
    const route = liteTopology.routes.find(route => route.prefix === node.address + "/128");
    assert.equal(meshNodeFor(group, route.originPrincipal, undefined, route.prefix)?.id, node.id);
    assert.equal(routeState(route, now), "Authorized at snapshot");
  }
  assert.equal(group.links.length, 2);
  assert.ok(group.links.every(link => !operators.some(node => node.id === link.source || node.id === link.target)), "learned operator routes are not direct Lite carriers");
  const addressOnly = liteTopology.routes.find(route => !route.originPrincipal);
  assert.equal(addressOnly.prefix, "fd45:b891:5680::99/128");
  assert.equal(routeState(addressOnly, now), "Unreachable");
  assert.equal(meshNodeFor(group, addressOnly.originPrincipal, undefined, addressOnly.prefix), undefined);
  assert.equal(liteTopology.peers[0].linkId, "18446744073709551615");
  assert.equal(liteTopology.announcements[0].revision, "18446744073709551615");
});

test("typed C5 evidence survives a busy or partial C6 reporter without inventing a C6 operator registry", () => {
  const now = Number(liteTopology.capturedAtUnixMs);
  const peer = liteTopology.peers.find(peer => peer.transport === "ble");
  const c6 = { id: "c6", name: "C6", platform: "wendy-lite", organizationId: tenant, pkiDeviceName: peer.deviceId };
  const observations = {
    [c5Reporter.id]: observe(liteTopology, now),
    c6: mergeMeshObservation(undefined, { status: "unavailable", platform: "wendy-lite", error: "The native slot is occupied." }, now),
  };
  let group = buildMeshGroups([c5Reporter, c6], observations, now).groups[0];
  const node = group.nodes.find(node => node.principal === peer.principal);
  assert.equal(nodeState(node, now).label, "Reporter unavailable");
  assert.ok(group.links.some(link => link.source === node.id || link.target === node.id));
  const partial = snapshot(peer.deviceId, { selfMeshAddress: peer.meshAddress, truncated: true, status: { state: "mesh-active", detail: "Partial endpoint directory." } });
  observations.c6 = mergeMeshObservation(observations.c6, { status: "ok", platform: "wendy-lite", topology: partial }, now + 1);
  group = buildMeshGroups([c5Reporter, c6], observations, now + 1).groups[0];
  assert.equal(group.nodes.find(node => node.principal === peer.principal).observation.topology.truncated, true);
  assert.equal(group.nodes.filter(node => node.role === "operator").length, 2);
});

test("a successful unavailable-runtime observation clears retained relationships from every frontend surface", () => {
  const now = Number(liteTopology.capturedAtUnixMs);
  const stopped = mergeMeshObservation(observe(liteTopology, now), {
    status: "ok",
    topology: { ...liteTopology, runtimeAvailable: false, status: { state: "stopping" } },
  }, now + 1);
  assert.equal(stopped.topology.peers.length + stopped.topology.routes.length + stopped.topology.announcements.length, 0);
  assert.equal(stopped.result.topology, stopped.topology);
  const group = buildMeshGroups([c5Reporter], { [c5Reporter.id]: stopped }, now + 1).groups[0];
  assert.equal(group.nodes.length, 1);
  assert.equal(group.links.length, 0);
});
