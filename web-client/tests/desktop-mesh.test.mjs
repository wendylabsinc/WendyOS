import test from "node:test";
import assert from "node:assert/strict";
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
  assert.deepEqual(result.devices.map((d) => d.id), ["one.local:50052", cloudSelector]);
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
  assert.deepEqual(result.devices.map((d) => d.id), selectors);
  assert.deepEqual(result.warnings, []);
  let command;
  service.run = async (_file, args) => {
    command = args;
    return JSON.stringify({status: "unsupported"});
  };
  await service.meshTopology({target: result.devices[1].id});
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
  assert.deepEqual(result.devices.map((d) => d.id), ["one.local:50052"]);
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
  assert.equal(result.devices.length, 1);
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
  assert.equal(command.options.timeout, 15000);
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
  const rest = ["two.local", "three.local", "four.local"].map((target) => service.meshTopology({target}));
  assert.throws(() => service.meshTopology({target: "five.local"}), /already in progress/);
  release();
  await Promise.all([first, ...rest]);
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
  assert.deepEqual(result.devices.map((device) => device.id), ["one.local:50052", cloudSelector]);
  assert.deepEqual(result.warnings, ["Local discovery omitted devices with invalid addresses."]);
});

test("a total discovery outage fails the refresh instead of replacing prior inventory with an empty graph", async () => {
  const service = fixture();
  service.run = async (_file, args) => { throw new Error(args[0] === "cloud" ? "Cloud offline" : "Network unavailable"); };
  await assert.rejects(service.meshDiscover(), /Local discovery: Network unavailable; Cloud discovery: Cloud offline/);
});
