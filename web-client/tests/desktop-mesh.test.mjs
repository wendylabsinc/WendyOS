import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { DesktopService } from "../electron/service.mjs";

const cloudAsset = "00000000-0000-4000-8000-000000000001";
const cloudTenant = "00000000-0000-4000-8000-000000000002";
const cloudSelector = `cloud://cloud.example:443/tenant/${cloudTenant}/asset/${cloudAsset}`;

function fixture() {
  return new DesktopService({ resources: "/resources", data: "/unused", env: {}, sessions: {} });
}

test("mesh discovery combines independent inventories without trusting advertised tenant hints", async () => {
  const service = fixture();
  const calls = [];
  service.run = async (_file, args) => {
    calls.push(args);
    return JSON.stringify(args[0] === "cloud" ? [{id: cloudAsset, selector: cloudSelector, name: "Remote"}] : {
      lanDevices: [{isWendyDevice: true, hostname: "one.local", port: 50052, displayName: "One", tenantUuid: "untrusted", principalUri: "untrusted"}],
      ethernetDevices: [{isWendyDevice: true, hostname: "one.local", port: 50052}],
    });
  };
  const result = await service.meshDiscover();
  assert.deepEqual(result.devices.map((d) => d.id), ["cli://local", "one.local:50052", cloudSelector]);
  assert.ok(result.devices.every((d) => !d.organizationId && !d.pkiDeviceName));
  assert.deepEqual(result.warnings, []);
  assert.ok(calls[0].includes("5s"));
  assert.ok(calls[1].includes("--all"));
});

test("Cloud discovery preserves complete selectors across tenants, endpoints and legacy organizations", async () => {
  const service = fixture();
  const selectors = [
    cloudSelector,
    cloudSelector.replace(cloudTenant, "00000000-0000-4000-8000-000000000003"),
    cloudSelector.replace("cloud.example:443", "other.example:7443"),
    "cloud://legacy.example:443/org/7/asset/77",
  ];
  service.run = async (_file, args) => JSON.stringify(args[0] === "cloud"
    ? selectors.map((selector, index) => ({id: index === 3 ? 77 : cloudAsset, selector, name: "Same label"}))
    : {});
  const result = await service.meshDiscover();
  assert.deepEqual(result.devices.map((d) => d.id), ["cli://local", ...selectors]);
  assert.deepEqual(result.warnings, []);
  let command;
  service.run = async (_file, args) => {
    command = args;
    return JSON.stringify({status: "unsupported"});
  };
  await service.meshTopology({target: result.devices[2].id});
  assert.equal(command[4], selectors[1]);
});

test("Cloud discovery rejects incomplete or mismatched selectors without discarding local results", async () => {
  const service = fixture();
  const invalidRows = [
    null,
    {id: cloudAsset},
    {id: cloudAsset, selector: `cloud://${cloudAsset}`},
    {id: cloudAsset, selector: cloudSelector.replace("cloud:", "https:")},
    {id: cloudAsset, selector: cloudSelector.replace("cloud.example", "user@cloud.example")},
    {id: cloudAsset, selector: cloudSelector + "?other=tenant"},
    {id: cloudAsset, selector: cloudSelector + "#other"},
    {id: cloudAsset, selector: cloudSelector.replace(cloudTenant, "not-a-tenant")},
    {id: cloudAsset, selector: cloudSelector.replace(cloudTenant, "00000000-0000-0000-0000-000000000000")},
    {id: cloudAsset, selector: cloudSelector.replace(cloudAsset, cloudTenant)},
    {id: 77, selector: "cloud://legacy.example:443/org/0/asset/77"},
    {id: 77, selector: "cloud://legacy.example:443/org/2147483648/asset/77"},
  ];
  service.run = async (_file, args) => JSON.stringify(args[0] === "cloud" ? invalidRows : {
    lanDevices: [{isWendyDevice: true, hostname: "one.local", port: 50052}],
  });
  const result = await service.meshDiscover();
  assert.deepEqual(result.devices.map((d) => d.id), ["cli://local", "one.local:50052"]);
  assert.equal(result.warnings.length, 1);
  assert.match(result.warnings[0], /without a valid scoped selector/);
});

test("mesh discovery retains local results and exposes unavailable Cloud inventory", async () => {
  const service = fixture();
  service.run = async (_file, args) => {
    if (args[0] === "cloud") throw new Error("Sign in first");
    return JSON.stringify({lanDevices: [{isWendyDevice: true, ipAddress: "192.0.2.1", port: 50052}]});
  };
  const result = await service.meshDiscover();
  assert.equal(result.devices.length, 2);
  assert.deepEqual(result.warnings, ["Cloud discovery: Sign in first"]);
});

test("topology calls use bounded read-only command and retain typed unsupported results", async () => {
  const service = fixture();
  let command;
  service.run = async (file, args, options) => {
    command = {file, args, options};
    return JSON.stringify({status: "unsupported", error: "Older agent", observedAt: new Date().toISOString()});
  };
  const result = await service.meshTopology({target: "one.local:50052"});
  assert.equal(result.status, "unsupported");
  assert.deepEqual(command.args, ["device", "wendy-net", "topology", "--device", "one.local:50052", "--json", "--observation"]);
  assert.ok(command.options.timeout > 159000 && command.options.timeout <= 160000);
  assert.equal(service.meshSnapshots.size, 0);
  assert.throws(() => service.meshTopology({target: "--device=other"}));
});

test("mesh refresh deduplicates a target and bounds simultaneous subprocesses", async () => {
  const service = fixture();
  let release;
  const gate = new Promise((resolve) => { release = resolve; });
  service.run = async () => { await gate; return JSON.stringify({status: "unavailable"}); };
  const first = service.meshTopology({target: "one.local"});
  assert.equal(first, service.meshTopology({target: "one.local"}));
  let active=0,maximum=0;
  const original=service.run;service.run=async(...args)=>{maximum=Math.max(maximum,++active);try{return await original(...args)}finally{active--}};
  const rest = ["two.local", "three.local", "four.local"].map((target) => service.meshTopology({target}));
  release();
  await Promise.all([first, ...rest]);
  assert.equal(maximum,3);
  assert.equal(service.meshSnapshots.size, 0);
});

test("topology monitoring never boots a stopped VM", async () => {
  const service = fixture();
  service.virtualMachines = async () => [{name: "stopped", state: "stopped"}];
  service.run = async () => { throw new Error("must not invoke a device command"); };
  await assert.rejects(service.meshTopology({target: "vm:stopped"}), /not running/);
});

test("invalid local advertisements preserve valid local and Cloud inventory", async () => {
  const service = fixture();
  service.run = async (_file, args) => JSON.stringify(args[0] === "cloud"
    ? [{id: cloudAsset, selector: cloudSelector, name: "Remote"}]
    : {lanDevices: [null, {isWendyDevice: true, hostname: "--other"},
        {isWendyDevice: true, hostname: 42, port: 50052},
        {isWendyDevice: true, hostname: "one.local", port: 50052}],
       usbDevices: {unexpected: true}});
  const result = await service.meshDiscover();
  assert.deepEqual(result.devices.map((device) => device.id), ["cli://local", "one.local:50052", cloudSelector]);
  assert.deepEqual(result.warnings, ["Local discovery omitted devices with invalid addresses."]);
});

test("a total discovery outage keeps the independent CLI reporter and retains prior inventory", async () => {
  const service = fixture();
  service.run = async (_file, args) => { throw new Error(args[0] === "cloud" ? "Cloud offline" : "Network unavailable"); };
  const inventory=await service.meshDiscover();
  assert.deepEqual(inventory.devices.map(device=>device.id),["cli://local"]);
  assert.equal(inventory.retainPrevious,true);
  assert.deepEqual(inventory.warnings,["Local discovery: Network unavailable","Cloud discovery: Cloud offline"]);
});


test("physical Lite reporters retain provider selectors and platform",async()=>{
 const service=fixture();service.run=async(_file,args)=>JSON.stringify(args[0]==="cloud"?[]:{externalDevices:[
  {id:"wendy-lite:/dev/cu.usbmodem1",displayName:"C6",providerKey:"wendy-lite",isWendyDevice:true,connectionInfo:{type:"USB"}},
  {id:"wendy-lite:board.local",displayName:"C5",providerKey:"wendy-lite",isWendyDevice:true,connectionInfo:{type:"LAN"}},
  {id:"wendy-lite:ble-id",providerKey:"wendy-lite",isWendyDevice:true,connectionInfo:{type:"BLE",address:"ble-id"}},
 ]});
 const inventory=await service.meshDiscover();assert.deepEqual(inventory.devices.map(d=>d.id),["cli://local","wendy-lite:/dev/cu.usbmodem1","wendy-lite:board.local","wendy-lite:ble:ble-id"]);
 assert.ok(inventory.devices.slice(1).every(d=>d.platform==="wendy-lite"));
 service.run=async(_file,args)=>{assert.equal(args[4],inventory.devices[1].id);return JSON.stringify({status:"ok",platform:"wendy-lite",topology:{selfPrincipal:"spiffe://wendy.sh/tenant/t/device/d"}})};
 assert.equal((await service.meshTopology({target:inventory.devices[1].id})).platform,"wendy-lite");
});


test("closing a topology view discards queued observations before dialing",async()=>{
 const service=fixture();let release,calls=0;const gate=new Promise(resolve=>{release=resolve});
 service.run=async()=>{calls++;await gate;return JSON.stringify({status:"unavailable"})};
 const first=service.meshTopology({target:"one.local"});
 await new Promise(resolve=>setImmediate(resolve));
 service.meshReporterKeys.set("two.local","one.local");
 const second=service.meshTopology({target:"two.local"});
 service.close();release();
 const results=await Promise.allSettled([first,second]);assert.equal(calls,1);assert.ok(results.every(r=>r.status==="rejected"));
});


test("a slow Lite reporter does not block OS reads and aliases share admission",async()=>{
 const service=fixture();let release;const gate=new Promise(resolve=>{release=resolve});let active=0,maximum=0;
 service.meshReporterKeys.set("lite-lan","c6");service.meshReporterKeys.set("lite-cloud","c6");
 service.run=async(_file,args)=>{if(args[4].startsWith("lite")){maximum=Math.max(maximum,++active);await gate;active--}return JSON.stringify({status:"unavailable"})};
 const first=service.meshTopology({target:"lite-lan"});const alias=service.meshTopology({target:"lite-cloud"});
 assert.equal((await service.meshTopology({target:"os.local"})).status,"unavailable");assert.equal(active,1);release();await Promise.all([first,alias]);assert.equal(maximum,1);
});


test("queued reporter admission expires within the request ceiling",async(t)=>{
 t.mock.timers.enable({apis:["setTimeout"]});const service=fixture();let release,calls=0;const gate=new Promise(resolve=>{release=resolve});
 service.run=async()=>{calls++;await gate;return JSON.stringify({status:"unavailable"})};
 const active=["a.local","b.local","c.local"].map(target=>service.meshTopology({target}));
 await new Promise(resolve=>setImmediate(resolve));
 const queued=service.meshTopology({target:"d.local"});const expired=assert.rejects(queued,/waiting for admission/);
 t.mock.timers.tick(160000);await expired;assert.equal(calls,3);release();await Promise.all(active);
});


test("local CLI reporter invokes only private-owner topology with normalized protobuf JSON",async()=>{
 const service=fixture(),topology=JSON.parse(readFileSync(new URL("./fixtures/operator-topology.json",import.meta.url),"utf8"));
 service.monitorTarget=async()=>{throw new Error("local reporter must not resolve a target agent")};
 service.run=async(_file,args,options)=>{
  assert.deepEqual(args,["device","wendy-net","topology","--local-cli","--json","--observation"]);
  assert.ok(options.timeout<=5000);
  return JSON.stringify({status:"ok",platform:"wendy-cli",topology});
 };
 const report=await service.meshTopology({target:"cli://local"});assert.equal(report.platform,"wendy-cli");
 assert.equal(report.topology.selfPrincipal,topology.selfPrincipal);assert.equal(report.topology.selfMeshAddress,topology.selfMeshAddress);
 assert.equal(report.topology.peers.find(peer=>peer.linkId==="18446744073709551615").linkId,"18446744073709551615");
});

test("slow or unavailable Lite leaves the existing local CLI router responsive",async()=>{
 const service=fixture();let release;const gate=new Promise(resolve=>{release=resolve});
 service.run=async(_file,args)=>{if(args.includes("--local-cli"))return JSON.stringify({status:"unavailable",platform:"wendy-cli",error:"No existing local router."});await gate;return JSON.stringify({status:"unavailable",platform:"wendy-lite"})};
 const pending=service.meshTopology({target:"wendy-lite:board.local"});
 assert.equal((await service.meshTopology({target:"cli://local"})).platform,"wendy-cli");release();assert.equal((await pending).status,"unavailable");
});

const cli4Models = JSON.parse(readFileSync(new URL("./fixtures/cli4-mesh-inventory.json", import.meta.url), "utf8"));
const cli4Inventory = cli4Models.collection;
function freshCLI4Inventory() {
  const inventory = structuredClone(cli4Inventory);
  inventory.meshDiscovery.capturedAtUnixMs = Date.now();
  for (const device of inventory.meshDevices) device.expiresAtUnixMs = Date.now() + 300_000;
  return inventory;
}
const c5Cloud = `cloud://cloud.example:443/tenant/${cli4Inventory.meshDevices[1].tenant}/asset/${cli4Inventory.meshDevices[1].device}`;
async function discoverCLI4(service) {
  service.run = async (_file, args) => JSON.stringify(args[0] === "cloud"
    ? cli4Models.cloud : freshCLI4Inventory());
  return service.meshDiscover();
}

test("real CLI4 model JSON exposes mesh-only C6 through its exact Lite selector", async () => {
  const service = fixture();
  const inventory = await discoverCLI4(service);
  const c6 = cli4Inventory.meshDevices[0];
  assert.equal(c6.revision, "18446744073709551615");
  assert.deepEqual(inventory.devices.map(device => device.id), ["cli://local", cli4Inventory.externalDevices[0].id, c6.selector]);
  assert.equal(inventory.devices[2].platform, "wendy-lite");
  assert.equal(inventory.devices[2].reporterKey, `device:${c6.tenant}/${c6.device}`);
  assert.deepEqual(inventory.warnings, []);
  let partial = false;
  service.run = async (_file, args) => {
    assert.deepEqual(args, ["device", "wendy-net", "topology", "--device", c6.selector, "--json", "--observation"]);
    return JSON.stringify(partial
      ? { status: "ok", platform: "wendy-lite", topology: { selfPrincipal: c6.principal, selfTenantId: c6.tenant, selfDeviceId: c6.device, selfMeshAddress: c6.address.slice(1, c6.address.indexOf("]")), capturedAtUnixMs: String(Date.now()), runtimeAvailable: true, truncated: true, status: { state: "mesh-active", detail: "Partial endpoint directory." } } }
      : { status: "unavailable", platform: "wendy-lite", error: "The native slot is occupied." });
  };
  assert.equal((await service.meshTopology({ target: c6.selector })).status, "unavailable");
  partial = true;
  const observed = await service.meshTopology({ target: c6.selector });
  assert.equal(observed.topology.truncated, true);
  assert.match(observed.topology.status.detail, /Partial/);
});

test("physical Cloud and mesh aliases share one reporter and one in-flight read", async () => {
  const service = fixture();
  await discoverCLI4(service);
  const physical = cli4Inventory.externalDevices[0].id;
  const mesh = cli4Inventory.meshDevices[1].selector;
  assert.equal(service.meshReporterKeys.get(physical), service.meshReporterKeys.get(mesh));
  assert.equal(service.meshReporterKeys.get(mesh), service.meshReporterKeys.get(c5Cloud));
  let release, reads = 0;
  const waiting = new Promise(resolve => { release = resolve; });
  service.run = async (_file, args) => {
    if (args.includes("--local-cli")) return JSON.stringify({ status: "unavailable", platform: "wendy-cli" });
    reads++;
    await waiting;
    return JSON.stringify({ status: "unavailable", platform: "wendy-lite" });
  };
  const first = service.meshTopology({ target: physical });
  assert.equal(first, service.meshTopology({ target: mesh }));
  assert.equal(first, service.meshTopology({ target: c5Cloud }));
  assert.equal((await service.meshTopology({ target: "cli://local" })).platform, "wendy-cli");
  release();
  await first;
  assert.equal(reads, 1);
});

test("partial or unavailable signed mesh inventory retains prior observations explicitly", async () => {
  const service = fixture();
  const inventory = freshCLI4Inventory();
  inventory.meshDiscovery.truncated = true;
  service.run = async (_file, args) => JSON.stringify(args[0] === "cloud" ? [] : inventory);
  let result = await service.meshDiscover();
  assert.equal(result.retainPrevious, true);
  assert.match(result.warnings[0], /partial signed inventory/);
  inventory.meshDiscovery.state = "not-connected";
  inventory.meshDevices = [];
  result = await service.meshDiscover();
  assert.equal(result.retainPrevious, true);
  assert.match(result.warnings[0], /not-connected/);
});


test("a delayed but live signed mesh candidate stays selectable with an explicit stale warning", async () => {
  const service = fixture(), inventory = freshCLI4Inventory();
  inventory.meshDiscovery.capturedAtUnixMs -= 20_000;
  service.run = async (_file, args) => JSON.stringify(args[0] === "cloud" ? [] : inventory);
  const result = await service.meshDiscover();
  assert.equal(result.retainPrevious, true);
  assert.match(result.warnings[0], /stale signed inventory/);
  assert.ok(result.devices.some(device => device.id === inventory.meshDevices[0].selector));
});
