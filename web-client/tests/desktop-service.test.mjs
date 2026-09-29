import test from "node:test";
import assert from "node:assert/strict";
import { DesktopService } from "../electron/service.mjs";

function fixture() {
  const calls = [];
  const jobs = [];
  const plan = {
    device_type: "raspberry-pi-5",
    version: "2026.09.01",
    method: "removable-media",
    artifact_sha256: "a".repeat(64),
    target: { id: "/dev/disk7", name: "SD", capacity_bytes: 32e9 },
    command: [
      "wendy",
      "install",
      "--device-type",
      "raspberry-pi-5",
      "--version",
      "2026.09.01",
      "--drive",
      "/dev/disk7",
    ],
  };
  const service = new DesktopService({
    resources: "/resources",
    data: "/unused",
    env: {},
    sessions: {
      list: () => [],
      start: (...args) => {
        jobs.push(args);
        return { id: "job" };
      },
    },
    confirmFlash: async () => true,
  });
  service.run = async (file, args) => {
    calls.push({ file, args });
    return JSON.stringify(plan);
  };
  return { service, plan, calls, jobs };
}

test("flash rechecks the exact image and media before confirmation, preserving installer prompts", async () => {
  const { service, calls, jobs } = fixture();
  const plan = await service.plan({
    deviceType: "raspberry-pi-5",
    drive: "/dev/disk7",
  });
  let confirms = 0;
  service.confirmFlash = async () => {
    confirms++;
    assert.equal(calls.length, 2);
    return true;
  };
  await service.flash(plan.id);
  assert.equal(confirms, 1);
  assert.ok(calls[1].args.includes("2026.09.01"));
  assert.equal(jobs.length, 1);
  assert.equal(jobs[0][1][0].file, "/resources/wendy");
  assert.ok(!jobs[0][1][0].args.includes("--yes"));
  await assert.rejects(service.flash(plan.id), /Refresh/);
  await service.verify({ address: "pi.local", planId: plan.id });
  assert.ok(calls[2].args.includes("--expected-os-version"));
  assert.ok(calls[2].args.includes("raspberry-pi-5"));
});

test("changed drives, rejected confirmation and expired plans cannot launch an installer", async () => {
  const { service, plan, jobs } = fixture();
  const planned = await service.plan({
    deviceType: "raspberry-pi-5",
    drive: "/dev/disk7",
  });
  let confirms = 0;
  service.confirmFlash = async () => {
    confirms++;
    return false;
  };
  service.run = async () =>
    JSON.stringify({
      ...plan,
      target: { ...plan.target, name: "Replacement drive" },
    });
  await assert.rejects(service.flash(planned.id), /drive or image changed/);
  assert.equal(confirms, 0);
  service.run = async () => JSON.stringify(plan);
  assert.equal(await service.flash(planned.id), null);
  assert.equal(jobs.length, 0);
  service.plans.get(planned.id).created = 0;
  await assert.rejects(service.flash(planned.id), /Refresh/);
});

test("project commands require a native folder selection and never invoke a shell", () => {
  const { service, jobs } = fixture();
  const args = {
    action: "run",
    project: "/project $(touch sentinel)",
    target: "docker",
    runtime: "docker",
  };
  assert.throws(() => service.projectAction(args), /Open the project/);
  service.projects.add(args.project);
  service.projectAction(args);
  assert.deepEqual(jobs[0][1][0].args, ["run", "--device", "docker"]);
  assert.equal(jobs[0][1][0].cwd, args.project);
  assert.throws(
    () => service.projectAction({ ...args, action: "shell" }),
    /Unsupported/,
  );
  assert.throws(
    () => service.projectAction({ ...args, target: "--help" }),
    /target/,
  );
  service.projectAction({
    ...args,
    target: "pi.local",
    runtime: "apple-container",
  });
  assert.deepEqual(jobs[1][1][0].args, [
    "run",
    "--device",
    "pi.local",
    "--builder",
    "apple-container",
  ]);
});

test("simulator lifecycle rejects containers owned by someone else", async () => {
  const { service, jobs } = fixture();
  service.simulators = [
    {
      id: "owned-id",
      name: "go2",
      runtime: "docker",
      container: "wendy-desktop-owned-id",
    },
  ];
  service.run = async () =>
    JSON.stringify([
      { Config: { Labels: { "dev.wendy.desktop.simulator": "different-id" } } },
    ]);
  await assert.rejects(
    service.simulatorAction({ id: "owned-id", action: "stop" }),
    /identity/,
  );
  assert.equal(jobs.length, 0);
});

test("a paused simulator keeps its authenticated viewer reachable", async (t) => {
  const { service } = fixture();
  const sim = {
    id: "sim",
    url: "http://127.0.0.1:8890/",
    digest: "abc",
    runtime: "docker",
  };
  service.simulators = [sim];
  service.inspectSim = async () => ({});
  t.mock.method(globalThis, "fetch", async () => ({
    ok: true,
    json: async () => ({
      simulation: true,
      robot: "go2",
      source_digest: "abc",
      healthy: true,
      ready: false,
      mode: "paused",
    }),
  }));
  const state = await service.simStatus("sim");
  assert.equal(state.reachable, true);
  assert.equal(state.ready, false);
  assert.equal(state.mode, "paused");
});

test("Apple Container's nested status becomes displayable container metadata", async () => {
  const { service } = fixture();
  service.run = async () =>
    JSON.stringify([
      {
        configuration: {
          id: "buildkit",
          image: { reference: "builder:latest" },
          publishedPorts: [{ hostPort: 12345, containerPort: 8890 }],
        },
        status: { state: "running", networks: [] },
      },
    ]);
  assert.deepEqual(await service.containers("apple-container"), [
    {
      id: "buildkit",
      name: "buildkit",
      image: "builder:latest",
      state: "running",
      ports: "12345 → 8890",
    },
  ]);
});

test("dashboard snapshots are structured, bounded, and deduplicate repeated opens without sessions", async () => {
  const { service, jobs } = fixture();
  let resolve;
  const calls = [];
  service.virtualMachines = async () => [{ name: "pi-lab", state: "running", address: "127.0.0.1:50051" }];
  service.run = (file, args, options) => {
    calls.push({ file, args, options });
    return new Promise((done) => { resolve = done; });
  };
  for (const target of ["docker", "apple-container", "local", "sim", "simulator", "--help", ""])
    assert.throws(() => service.deviceSnapshot({ target }));
  const first = service.deviceSnapshot({ target: "vm:pi-lab" });
  const again = service.deviceSnapshot({ target: "vm:pi-lab" });
  assert.equal(first, again);
  await new Promise((done) => setImmediate(done));
  assert.deepEqual(calls[0].args, ["device", "top", "--device", "vm:pi-lab", "--json", "--read-only"]);
  assert.equal(calls[0].options.timeout, 12000);
  assert.equal(calls[0].options.maxBuffer, 2 * 1024 * 1024);
  resolve(JSON.stringify({ host: { cpuPercent: 4.5, cpuCount: 2, memUsedBytes: 123 }, containers: null }));
  const snapshot = await first;
  assert.equal(snapshot.target, "vm:pi-lab");
  assert.equal(snapshot.host.cpuPercent, 4.5);
  assert.deepEqual(snapshot.containers, []);
  assert.ok(!Number.isNaN(Date.parse(snapshot.sampledAt)));
  assert.equal(service.deviceSnapshots.size, 0);
  assert.equal(jobs.length, 0);
});

test("failed or stopped dashboard targets can be retried without starting a VM", async () => {
  const { service, jobs } = fixture();
  service.virtualMachines = async () => [{ name: "pi-lab", state: "stopped", address: "127.0.0.1:50051" }];
  await assert.rejects(service.deviceSnapshot({ target: "vm:pi-lab" }), /not running/);
  assert.equal(service.deviceSnapshots.size, 0);
  service.run = async () => JSON.stringify({ wrong: true });
  await assert.rejects(service.deviceSnapshot({ target: "pi.local" }), /invalid dashboard/);
  service.run = async () => JSON.stringify({ host: {}, containers: [] });
  assert.equal((await service.deviceSnapshot({ target: "pi.local" })).target, "pi.local");
  assert.equal(jobs.length, 0);
});

test("device and container logs use structured pipes and pin the selected source", async () => {
  const { service, jobs } = fixture();
  const streams = [];
  service.monitorLogs = { start: (args) => { streams.push(args); return { id: "device" }; }, startCommand: (args) => { streams.push(args); return { id: "container" }; } };
  service.virtualMachines = async () => [{ name: "pi-lab", state: "running", address: "127.0.0.1:50051" }];
  await service.deviceLogsStart({ target: "vm:pi-lab", app: "my app; echo bad", level: "warn", connectionTarget: "wrong.local" });
  assert.deepEqual(streams[0], { target: "vm:pi-lab", app: "my app; echo bad", level: "warn" });
  service.containers = async () => [{ id: "abc", name: "app" }];
  await assert.rejects(service.deviceLogsStart({ target: "app", source: { kind: "container", runtime: "docker", id: "--help" } }), /no longer exists/);
  await assert.rejects(service.deviceLogsStart({ target: "app", source: { kind: "container", runtime: "apple-container", id: "gone" } }), /no longer exists/);
  await service.deviceLogsStart({ target: "app", source: { kind: "container", runtime: "apple-container", id: "abc" } });
  assert.equal(streams[1].file, "container");
  assert.deepEqual(streams[1].args, ["logs", "--follow", "-n", "100", "abc"]);
  assert.equal(jobs.length, 0);
});

test("renderer reload cancels pending log opens before spawning a child", async () => {
  const { service } = fixture();
  let resolve;
  let starts = 0;
  service.monitorLogs = { startCommand: () => starts++, close: () => {} };
  service.containers = () => new Promise((done) => { resolve = done; });
  const opening = service.deviceLogsStart({ target: "app", source: { kind: "container", runtime: "docker", id: "abc" } });
  service.close();
  resolve([{ id: "abc", name: "app" }]);
  await assert.rejects(opening, /no longer open/);
  assert.equal(starts, 0);
});

test("network discovery provides actionable device rows instead of raw JSON", async () => {
  const { service } = fixture();
  service.run = async () => JSON.stringify({ lanDevices: [
    { displayName: "Pi", hostname: "pi.local", port: 50051, isWendyDevice: true },
    { displayName: "Pi duplicate", hostname: "pi.local", port: 50051, isWendyDevice: true },
    { displayName: "Other", hostname: "other.local", isWendyDevice: false },
  ], usbDevices: [], ethernetDevices: [] });
  assert.deepEqual(await service.discover(), [{ name: "Pi", target: "pi.local:50051", address: "pi.local:50051", transport: "Network", version: undefined }]);
});


test("a running QEMU process is not ready until its guest agent responds", async () => {
  const { service, calls } = fixture();
  const sim = {
    id: "vm",
    kind: "raspberry-pi",
    vmName: "desktop-test",
    target: "vm:desktop-test",
  };
  service.simulators = [sim];
  service.virtualMachines = async () => [
    { name: sim.vmName, state: "running", address: "127.0.0.1:12345" },
  ];
  service.run = async () => {
    throw new Error("Agent still booting");
  };
  assert.equal((await service.simStatus("vm")).ready, false);
  service.run = async (file, args) => {
    calls.push({ file, args });
    return "{}";
  };
  assert.equal((await service.simStatus("vm")).ready, true);
  assert.deepEqual(calls[0].args, [
    "device",
    "info",
    "--device",
    "vm:desktop-test",
    "--json",
    "--read-only",
  ]);
  service.virtualMachines = async () => [
    { name: sim.vmName, state: "stopped" },
  ];
  assert.equal((await service.simStatus("vm")).ready, false);
});

test("VM setup uses the generic profile, resumes a created disk, and retains CLI prompts", async () => {
  const { service, jobs } = fixture();
  const sim = {
    id: "pi",
    name: "pi-lab",
    kind: "raspberry-pi",
    vmName: "desktop-test",
    port: 12345,
  };
  service.virtualMachines = async () => [];
  await service.vmAction(sim, "retry");
  assert.deepEqual(jobs[0][1][0].args, [
    "vm",
    "create",
    "desktop-test",
    "--profile",
    "generic",
  ]);
  assert.deepEqual(jobs[0][1][1].args, [
    "vm",
    "start",
    "desktop-test",
    "--detach",
    "--port",
    "12345",
    "--memory",
    "2048",
    "--cpus",
    "2",
  ]);
  service.virtualMachines = async () => [{ name: sim.vmName }];
  await service.vmAction(sim, "retry");
  assert.equal(jobs[1][1].length, 1);
  assert.equal(jobs[1][1][0].args[1], "start");
});

test("G1 status rejects a Go2 endpoint even when the source digest matches", async (t) => {
  const { service } = fixture();
  service.simulators = [
    { id: "g1", kind: "g1", digest: "abc", url: "http://127.0.0.1:8890/" },
  ];
  service.inspectSim = async () => ({});
  let robot = "go2";
  t.mock.method(globalThis, "fetch", async () => ({
    ok: true,
    json: async () => ({
      robot,
      simulation: true,
      source_digest: "abc",
      ready: true,
      healthy: true,
    }),
  }));
  assert.equal((await service.simStatus("g1")).reachable, false);
  robot = "g1";
  assert.equal((await service.simStatus("g1")).ready, true);
});

test("integrated viewer accepts only data and control endpoints on an identity-checked simulator", async (t) => {
  const { service } = fixture();
  service.simulators = [
    { id: "robot", kind: "go2", url: "http://127.0.0.1:8890/", digest: "abc" },
  ];
  service.inspectSim = async () => ({});
  const requests = [];
  t.mock.method(globalThis, "fetch", async (url, options) => {
    requests.push({ url: String(url), options });
    return {
      ok: true,
      json: async () => ({
        simulation: true,
        robot: "go2",
        source_digest: "abc",
        accepted: true,
      }),
    };
  });
  for (const path of [
    "https://example.com/",
    "/viewer.js",
    "/api/../secret",
    "/api/status?redirect=x",
    "/api/arm",
  ])
    await assert.rejects(
      service.simulatorRequest({ id: "robot", path }),
      /Unsupported/,
    );
  await assert.rejects(
    service.simulatorRequest({ id: "missing", path: "/api/status" }),
    /not found/,
  );
  await assert.rejects(
    service.simulatorRequest({
      id: "robot",
      path: "/api/command",
      body: { token: "x".repeat(5000) },
    }),
    /Invalid/,
  );
  assert.equal(requests.length, 0);
  await service.simulatorRequest({ id: "robot", path: "/api/scene" });
  await service.simulatorRequest({
    id: "robot",
    path: "/api/command",
    body: { token: "lease", velocity: [0, 0, 0] },
  });
  assert.deepEqual(
    requests.map((r) => r.url),
    [
      "http://127.0.0.1:8890/api/status",
      "http://127.0.0.1:8890/api/scene",
      "http://127.0.0.1:8890/api/command",
    ],
  );
  assert.equal(requests[2].options.redirect, "error");
  assert.equal(requests[2].options.method, "POST");
  assert.deepEqual(JSON.parse(requests[2].options.body), {
    token: "lease",
    velocity: [0, 0, 0],
  });
  service.viewerLeases.clear();
  service.inspectSim = async () => {
    throw new Error("Wrong container identity");
  };
  await assert.rejects(
    service.simulatorRequest({ id: "robot", path: "/api/arm", body: {} }),
    /identity/,
  );
  assert.equal(requests.length, 3);
});
