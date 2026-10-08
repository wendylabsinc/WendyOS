import { execFile } from "node:child_process";
import { promisify } from "node:util";
import {
  readFile,
  writeFile,
  mkdir,
  realpath,
  stat,
  rename,
} from "node:fs/promises";
import { join } from "node:path";
import { createServer } from "node:net";
import { randomUUID } from "node:crypto";
import { setTimeout as delay } from "node:timers/promises";
import { DeviceLogs } from "./device-logs.mjs";
import {
  installArgs,
  loopbackURL,
  runtimeCommand,
  sameInstallTarget,
  simulatorName,
  simulatorKind,
  deviceTarget,
  simRunArgs,
  text,
} from "./policy.mjs";

const execute = promisify(execFile);

export class DesktopService {
  constructor({
    resources,
    data,
    env,
    sessions,
    chooseDirectory,
    confirmFlash,
  }) {
    Object.assign(this, {
      resources,
      data,
      env,
      sessions,
      chooseDirectory,
      confirmFlash,
    });
    this.cli = join(
      resources,
      process.platform === "win32" ? "wendy.exe" : "wendy",
    );
    this.projects = new Set();
    this.plans = new Map();
    this.simulators = [];
    this.saving = Promise.resolve();
    this.viewerLeases = new Map();
    this.viewerChecks = new Map();
    this.deviceSnapshots = new Map();
    this.monitorLogs = new DeviceLogs({ cli: this.cli, env });
    this.monitorGeneration = 0;
  }

  async initialize() {
    this.sources = {};
    for (const kind of ["go2", "g1"]) {
      const digest = (
        await readFile(join(this.resources, `${kind}-digest.txt`), "utf8")
      ).trim();
      this.sources[kind] = {
        digest,
        image: `wendy-desktop-${kind}:${digest.slice(0, 16)}`,
      };
    }
    this.digest = this.sources.go2.digest;
    this.image = this.sources.go2.image;
    try {
      const records = JSON.parse(
        await readFile(join(this.data, "simulators.json"), "utf8"),
      );
      if (!Array.isArray(records))
        throw new Error("Invalid simulator registry.");
      this.simulators = records.filter((s) => {
        try {
          s.kind = simulatorKind(s.kind);
          simulatorName(s.name);
          if (!/^[a-f0-9-]{36}$/.test(s.id)) return false;
          if (s.kind === "raspberry-pi") {
            return (
              s.runtime === "qemu" &&
              s.vmName === this.vmName(s.id) &&
              s.target === `vm:${s.vmName}` &&
              Number.isInteger(s.port) &&
              s.port >= 1024 &&
              s.port <= 65535
            );
          }
          runtimeCommand(s.runtime);
          loopbackURL(s.url);
          return (
            /^[a-f0-9-]{36}$/.test(s.id) &&
            s.container === `wendy-desktop-${s.id}` &&
            Number.isInteger(s.port) &&
            s.port >= 1024 &&
            s.port <= 65535 &&
            s.url === `http://127.0.0.1:${s.port}/` &&
            /^[a-f0-9]{64}$/.test(s.digest)
          );
        } catch {
          return false;
        }
      });
    } catch (error) {
      if (error.code !== "ENOENT")
        throw new Error(`Cannot read simulator registry: ${error.message}`);
    }
  }

  async save() {
    const contents = JSON.stringify(this.simulators, null, 2);
    this.saving = this.saving
      .catch(() => {})
      .then(async () => {
        await mkdir(this.data, { recursive: true });
        // Only one writer, including during concurrent job completion.
        await writeFile(join(this.data, "simulators.json.tmp"), contents, {
          mode: 0o600,
        });
        await rename(
          join(this.data, "simulators.json.tmp"),
          join(this.data, "simulators.json"),
        );
      });
    return this.saving;
  }

  async run(file, args, options = {}) {
    try {
      const result = await execute(file, args, {
        env: this.env,
        timeout: 20_000,
        maxBuffer: 4 * 1024 * 1024,
        windowsHide: true,
        ...options,
      });
      return result.stdout.trim();
    } catch (error) {
      // Preserve structured verification failures, which exit nonzero on purpose.
      if (options.verification && error.stdout?.trim().startsWith("{"))
        return error.stdout.trim();
      if (error.killed && error.signal === "SIGTERM")
        throw new Error(`The request timed out after ${(options.timeout || 20_000) / 1000} seconds. Check the device connection and try again.`);
      throw new Error(
        (error.stderr || error.stdout || error.message).trim().slice(-6000),
      );
    }
  }

  async status() {
    const probe = async (id, args) => {
      try {
        return {
          id,
          ready: true,
          detail: await this.run(runtimeCommand(id), args, { timeout: 6000 }),
        };
      } catch (error) {
        return { id, ready: false, detail: error.message };
      }
    };
    const [version, docker, apple] = await Promise.all([
      this.run(this.cli, ["--version"]),
      probe("docker", ["info", "--format", "{{.ServerVersion}}"]),
      process.platform === "darwin" && process.arch === "arm64"
        ? probe("apple-container", ["system", "status"])
        : {
            id: "apple-container",
            ready: false,
            detail: "Requires an Apple silicon Mac.",
          },
    ]);
    return {
      version,
      runtimes: [docker, apple],
      platform: process.platform,
      image: this.image,
    };
  }

  async containers(runtime) {
    const file = runtimeCommand(runtime);
    const output = await this.run(
      file,
      runtime === "docker"
        ? ["ps", "--all", "--format", "{{json .}}"]
        : ["list", "--all", "--format", "json"],
    );
    if (!output) return [];
    const rows =
      runtime === "docker"
        ? output.split("\n").map((line) => JSON.parse(line))
        : JSON.parse(output);
    return rows.map((row) =>
      runtime === "docker"
        ? {
            id: row.ID,
            name: row.Names,
            image: row.Image,
            state: row.State,
            ports: row.Ports,
          }
        : {
            id: row.configuration?.id ?? row.id,
            name: row.configuration?.id ?? row.id,
            image: row.configuration?.image?.reference ?? row.image ?? "",
            state:
              typeof row.status === "string"
                ? row.status
                : (row.status?.state ?? row.state ?? "unknown"),
            ports: (row.configuration?.publishedPorts || [])
              .map((port) => `${port.hostPort} → ${port.containerPort}`)
              .join(", "),
          },
    );
  }

  async deviceLogsStart({ source, target, app, level }) {
    const generation = this.monitorGeneration;
    const assertActive = () => {
      if (generation !== this.monitorGeneration) throw new Error("This log view is no longer open.");
    };
    if (!source) {
      target = await this.monitorTarget(target);
      assertActive();
      return this.monitorLogs.start({ target, app, level });
    }
    if (source.kind === "container") {
      const file = runtimeCommand(source.runtime);
      const id = text(source.id, "container ID");
      // Resolve against the live inventory, never accept arbitrary CLI options.
      const container = (await this.containers(source.runtime)).find((c) => c.id === id);
      assertActive();
      if (!container || id.startsWith("-")) throw new Error("Container no longer exists.");
      return this.monitorLogs.startCommand({ target, file, args: ["logs", "--follow", source.runtime === "docker" ? "--tail" : "-n", "100", id] });
    }
    if (source.kind === "simulator") {
      const sim = this.sim(source.id);
      if (sim.kind === "raspberry-pi")
        return this.monitorLogs.startCommand({ target, args: ["vm", "logs", sim.vmName, "--follow"] });
      await this.inspectSim(sim);
      assertActive();
      return this.monitorLogs.startCommand({ target, file: runtimeCommand(sim.runtime), args: ["logs", "--follow", sim.runtime === "docker" ? "--tail" : "-n", "100", sim.container] });
    }
    throw new Error("Unsupported log source.");
  }

  virtualMachines() {
    return this.run(this.cli, ["vm", "list", "--json"]).then(JSON.parse);
  }

  async monitorTarget(target) {
    target = deviceTarget(target);
    if (!target.startsWith("vm:")) return target;
    const vm = (await this.virtualMachines()).find((vm) => vm.name === target.slice(3));
    if (!vm || vm.state !== "running" || !vm.address)
      throw new Error("This virtual machine is not running. Start it before opening its dashboard or logs.");
    // The CLI's read-only monitoring path connects to an existing running VM
    // while retaining its vm:<name> identity pin, independent of loopback ports.
    return target;
  }

  deviceSnapshot({ target }) {
    target = deviceTarget(target);
    const pending = this.deviceSnapshots.get(target);
    if (pending) return pending;
    if (this.deviceSnapshots.size >= 8)
      throw new Error("Too many device dashboards are refreshing. Try again shortly.");
    const request = this.monitorTarget(target).then((address) => this.run(
      this.cli,
      ["device", "top", "--device", address, "--json", "--read-only"],
      { timeout: 12_000, maxBuffer: 2 * 1024 * 1024 },
    )).then((output) => {
      const snapshot = JSON.parse(output);
      if (!snapshot || typeof snapshot.host !== "object" || !snapshot.host ||
          (snapshot.containers != null && !Array.isArray(snapshot.containers)))
        throw new Error("The device returned an invalid dashboard snapshot.");
      return { ...snapshot, containers: snapshot.containers || [], target, sampledAt: new Date().toISOString() };
    }).finally(() => this.deviceSnapshots.delete(target));
    this.deviceSnapshots.set(target, request);
    return request;
  }

  deviceLogsSnapshot(id) {
    return this.monitorLogs.snapshot(id);
  }

  deviceLogsStop(id) {
    return this.monitorLogs.stop(id);
  }

  close() {
    this.monitorGeneration++;
    this.monitorLogs.close();
  }

  vmName(id) {
    return `desktop-${id.replaceAll("-", "").slice(0, 24)}`;
  }

  async vmStatus(sim) {
    try {
      const vm = (await this.virtualMachines()).find(
        (vm) => vm.name === sim.vmName,
      );
      if (!vm || vm.state !== "running" || !vm.address)
        return {
          ...sim,
          ready: false,
          healthy: false,
          reachable: false,
          mode: vm?.state || "Not created",
        };
      // Process existence alone does not establish that the guest agent booted.
      // The VM selector has its own identity handling. A raw loopback address
      // can collide with a user's enrolled-device pin for another forwarded VM.
      await this.run(
        this.cli,
        ["device", "info", "--device", sim.target, "--json", "--read-only"],
        { timeout: 5000 },
      );
      return {
        ...sim,
        ready: true,
        healthy: true,
        reachable: false,
        mode: "Ready",
        address: vm.address,
      };
    } catch (error) {
      return {
        ...sim,
        ready: false,
        healthy: false,
        reachable: false,
        error: error.message,
      };
    }
  }

  async waitForVM(sim, session) {
    this.sessions.append(
      session,
      "\r\nWaiting for the WendyOS guest agent...\r\n",
    );
    const deadline = Date.now() + 180_000;
    let detail = "";
    while (Date.now() < deadline && !session.cancelled) {
      const state = await this.vmStatus(sim);
      detail = state.error || state.mode || "";
      if (state.ready) {
        this.sessions.append(
          session,
          `WendyOS is ready. Build, chat, or monitor with target ${sim.target}.\r\n`,
        );
        return;
      }
      await delay(1500);
    }
    throw new Error(
      `WendyOS did not become ready. Open boot logs to inspect startup. ${detail}`,
    );
  }

  vmStartSpec(sim) {
    return this.cliSpec([
      "vm",
      "start",
      sim.vmName,
      "--detach",
      "--port",
      String(sim.port),
      "--memory",
      "2048",
      "--cpus",
      "2",
    ]);
  }

  async vmAction(sim, action) {
    if (!["start", "stop", "retry"].includes(action))
      throw new Error("Unsupported simulator action.");
    if (action === "retry") {
      const exists = (await this.virtualMachines()).some(
        (vm) => vm.name === sim.vmName,
      );
      const commands = exists
        ? []
        : [this.cliSpec(["vm", "create", sim.vmName, "--profile", "generic"])];
      commands.push(this.vmStartSpec(sim));
      return this.sessions.start(`Create WendyOS · ${sim.name}`, commands, {
        exclusive: `sim:${sim.id}`,
        onSuccess: (session) => this.waitForVM(sim, session),
      });
    }
    return this.sessions.start(
      `${action} · ${sim.name}`,
      [
        action === "start"
          ? this.vmStartSpec(sim)
          : this.cliSpec(["vm", "stop", sim.vmName]),
      ],
      {
        exclusive: `sim:${sim.id}`,
        onSuccess:
          action === "start"
            ? (session) => this.waitForVM(sim, session)
            : undefined,
      },
    );
  }

  async pickProject() {
    const selected = await this.chooseDirectory();
    if (!selected) return null;
    const project = await realpath(selected);
    if (!(await stat(project)).isDirectory())
      throw new Error("Choose a project folder.");
    this.projects.add(project);
    return project;
  }

  project(value) {
    if (!this.projects.has(value))
      throw new Error("Open the project folder first.");
    return value;
  }

  cliSpec(args, cwd, label) {
    return { file: this.cli, args, cwd, label };
  }

  projectAction({ action, project, target, runtime }) {
    const cwd = this.project(project);
    runtimeCommand(runtime);
    target = text(target, "target");
    if (target.startsWith("-")) throw new Error("Invalid target.");
    if (!["chat", "chat-setup", "build", "run"].includes(action))
      throw new Error("Unsupported project action.");
    const args = action.startsWith("chat")
      ? ["chat", "--directory", cwd]
      : [action, "--device", target];
    // Local providers select their own builder and reject a --builder override.
    if (
      !action.startsWith("chat") &&
      !["docker", "apple-container", "local"].includes(target)
    )
      args.push("--builder", runtime);
    if (action === "chat-setup") args.push("--setup");
    if (action === "chat") args.push("--device", target);
    return this.sessions.start(
      action === "chat"
        ? "Wendy chat"
        : action === "chat-setup"
          ? "Set up chat"
          : `${action === "build" ? "Build" : "Run"} · ${target}`,
      [this.cliSpec(args, cwd)],
      { exclusive: `${action}:${cwd}` },
    );
  }

  startApple() {
    if (process.platform !== "darwin" || process.arch !== "arm64")
      throw new Error("Requires an Apple silicon Mac.");
    return this.sessions.start(
      "Start Apple Container",
      [
        { file: "container", args: ["system", "start"] },
        { file: "container", args: ["builder", "start"] },
      ],
      { exclusive: "apple-start" },
    );
  }

  async freePort() {
    const server = createServer();
    await new Promise((resolve, reject) => {
      server.once("error", reject);
      server.listen(0, "127.0.0.1", resolve);
    });
    const port = server.address().port;
    await new Promise((resolve) => server.close(resolve));
    return port;
  }

  sim(id) {
    const sim = this.simulators.find((s) => s.id === id);
    if (!sim) throw new Error("Simulator not found.");
    return sim;
  }

  async inspectSim(sim) {
    const raw = JSON.parse(
      await this.run(runtimeCommand(sim.runtime), ["inspect", sim.container]),
    );
    const entry = Array.isArray(raw) ? raw[0] : raw;
    const labels =
      sim.runtime === "docker"
        ? entry.Config?.Labels
        : entry.configuration?.labels;
    if (labels?.["dev.wendy.desktop.simulator"] !== sim.id)
      throw new Error("Container identity does not match this simulator.");
    return entry;
  }

  async simStatus(id) {
    const sim = this.sim(id);
    if (sim.kind === "raspberry-pi") return this.vmStatus(sim);
    try {
      await this.inspectSim(sim);
      const response = await fetch(
        new URL("api/status", loopbackURL(sim.url)),
        { signal: AbortSignal.timeout(3000), redirect: "error" },
      );
      if (!response.ok)
        throw new Error(`Simulator returned HTTP ${response.status}.`);
      const status = await response.json();
      if (
        status.simulation !== true ||
        status.robot !== (sim.kind || "go2") ||
        status.source_digest !== sim.digest
      )
        throw new Error("The viewer endpoint belongs to a different runtime.");
      this.viewerLeases.set(id, Date.now() + 12_000);
      return {
        ...sim,
        reachable: true,
        ready: status.ready === true,
        healthy: status.healthy === true,
        mode: status.mode,
        error: status.error || "",
      };
    } catch (error) {
      this.viewerLeases.delete(id);
      return {
        ...sim,
        reachable: false,
        ready: false,
        healthy: false,
        error: error.message,
      };
    }
  }

  async simulatorRequest({ id, path, body }) {
    const sim = this.sim(id);
    if (sim.kind === "raspberry-pi")
      throw new Error("This VM has no robot viewer.");
    const reads = [
      "/api/status",
      "/api/scene",
      "/api/scene/state",
      "/api/scene/lidar",
      "/camera.jpg",
    ];
    const writes = [
      "arm",
      "release",
      "stop",
      "pause",
      "resume",
      "reset",
      "arm_ros",
      "disarm_ros",
      "obstacle",
      "sensors",
      "command",
    ].map((action) => `/api/${action}`);
    const write = body !== undefined;
    if (!(write ? writes : reads).includes(path))
      throw new Error("Unsupported simulator request.");
    const payload = write ? JSON.stringify(body) : undefined;
    if (
      write &&
      (!body ||
        typeof body !== "object" ||
        Array.isArray(body) ||
        Buffer.byteLength(payload) > 4096)
    )
      throw new Error("Invalid simulator control request.");
    // Renderer requests can select only a registered, identity-checked robot,
    // never a URL or executable code from its container. Share concurrent checks.
    if ((this.viewerLeases.get(id) || 0) < Date.now()) {
      if (!this.viewerChecks.has(id))
        this.viewerChecks.set(
          id,
          this.simStatus(id).finally(() => this.viewerChecks.delete(id)),
        );
      const status = await this.viewerChecks.get(id);
      if (!status.reachable)
        throw new Error(status.error || "Simulator unavailable.");
    }
    const response = await fetch(new URL(path, loopbackURL(sim.url)), {
      method: write ? "POST" : "GET",
      ...(write
        ? { body: payload, headers: { "Content-Type": "application/json" } }
        : {}),
      signal: AbortSignal.timeout(path === "/api/scene" ? 15_000 : 5000),
      redirect: "error",
    });
    if (path === "/camera.jpg" && response.ok)
      return { bytes: new Uint8Array(await response.arrayBuffer()) };
    const result = await response.json();
    if (!response.ok)
      throw new Error(
        result.error || `Simulator returned HTTP ${response.status}.`,
      );
    return result;
  }

  async waitForSim(sim, session) {
    this.sessions.append(
      session,
      `\r\nWaiting for ${(sim.kind || "go2").toUpperCase()} physics and the browser viewer...\r\n`,
    );
    const deadline = Date.now() + 120_000;
    while (Date.now() < deadline && !session.cancelled) {
      const state = await this.simStatus(sim.id);
      if (state.ready && state.healthy) {
        this.sessions.append(session, `${sim.name} is ready at ${sim.url}\r\n`);
        return;
      }
      await delay(1000);
    }
    throw new Error(
      "The robot did not become ready. Open simulator logs to inspect startup; the container may still be running.",
    );
  }

  async createSimulator({ name, runtime, kind = "go2" }) {
    kind = simulatorKind(kind);
    name = simulatorName(name);
    const file = kind === "raspberry-pi" ? this.cli : runtimeCommand(runtime);
    if (kind === "raspberry-pi") runtime = "qemu";
    if (this.simulators.some((s) => s.name === name && s.runtime === runtime))
      throw new Error(
        "A simulator with this name already exists on that runtime.",
      );
    const id = randomUUID();
    const port = await this.freePort();
    const source = this.sources[kind];
    const sim =
      kind === "raspberry-pi"
        ? {
            id,
            name,
            kind,
            runtime,
            port,
            vmName: this.vmName(id),
            target: `vm:${this.vmName(id)}`,
          }
        : {
            id,
            name,
            kind,
            runtime,
            container: `wendy-desktop-${id}`,
            port,
            url: `http://127.0.0.1:${port}/`,
            digest: source.digest,
          };
    this.simulators.push(sim);
    await this.save();
    try {
      if (kind === "raspberry-pi") {
        const session = await this.vmAction(sim, "retry");
        return { simulator: sim, session };
      }
      const session = this.sessions.start(
        `Create ${kind.toUpperCase()} · ${name}`,
        [
          {
            file,
            args: [
              "build",
              "--platform",
              "linux/arm64",
              "--tag",
              source.image,
              ".",
            ],
            cwd: join(this.resources, kind),
            label: `Build ${kind.toUpperCase()} runtime from pinned sources`,
          },
          {
            file,
            args: simRunArgs(sim, source.image),
            label: `Start ${name} on ${runtime}`,
          },
        ],
        {
          exclusive: `sim:${id}`,
          onSuccess: (session) => this.waitForSim(sim, session),
        },
      );
      return { simulator: sim, session };
    } catch (error) {
      this.simulators = this.simulators.filter((s) => s.id !== id);
      await this.save();
      throw error;
    }
  }

  async simulatorAction({ id, action }) {
    const sim = this.sim(id);
    if (!["start", "stop", "retry"].includes(action))
      throw new Error("Unsupported simulator action.");
    if (sim.kind === "raspberry-pi") return this.vmAction(sim, action);
    const file = runtimeCommand(sim.runtime);
    const kind = sim.kind || "go2";
    if (action === "retry") {
      const source = this.sources[kind];
      if (sim.digest !== source.digest)
        throw new Error(
          "This simulator was created with a different runtime source. Create a new simulator to use this desktop version.",
        );
      // A failed build has no container yet. Never replace a container at this name.
      try {
        await this.run(file, ["inspect", sim.container]);
      } catch {
        return this.sessions.start(
          `Retry ${kind.toUpperCase()} · ${sim.name}`,
          [
            {
              file,
              args: [
                "build",
                "--platform",
                "linux/arm64",
                "--tag",
                source.image,
                ".",
              ],
              cwd: join(this.resources, kind),
            },
            { file, args: simRunArgs(sim, source.image) },
          ],
          {
            exclusive: `sim:${id}`,
            onSuccess: (session) => this.waitForSim(sim, session),
          },
        );
      }
      throw new Error(
        "This container already exists. Use Start or inspect its logs.",
      );
    }
    await this.inspectSim(sim);
    return this.sessions.start(
      `${action} · ${sim.name}`,
      [
        {
          file,
          args: [action, sim.container],
        },
      ],
      {
        exclusive: `sim:${id}`,
        onSuccess:
          action === "start"
            ? (session) => this.waitForSim(sim, session)
            : undefined,
      },
    );
  }

  async drives() {
    return JSON.parse(
      await this.run(this.cli, ["os", "list-drives", "--all", "--json"]),
    );
  }

  async plan(options) {
    const args = installArgs(options);
    const plan = JSON.parse(
      await this.run(this.cli, args, { timeout: 60_000 }),
    );
    if (
      !plan.command?.length ||
      plan.command[0] !== "wendy" ||
      plan.command[1] !== "install"
    )
      throw new Error("The installer did not return an executable plan.");
    const id = randomUUID();
    this.plans.clear();
    this.plans.set(id, { args, plan, created: Date.now() });
    return { ...plan, id };
  }

  async flash(id) {
    const record = this.plans.get(id);
    if (!record || record.started || Date.now() - record.created > 15 * 60_000)
      throw new Error("Refresh the installation plan before flashing.");
    if (
      this.sessions
        .list()
        .some((s) => s.running && s.title.startsWith("Flash "))
    )
      throw new Error("An installation is already running.");
    // Re-enumerate the target and pin the resolved version, then require native confirmation.
    const args = [...record.args];
    if (!args.includes("--version"))
      args.push("--version", record.plan.version);
    const current = JSON.parse(
      await this.run(this.cli, args, { timeout: 60_000 }),
    );
    if (!sameInstallTarget(record.plan, current))
      throw new Error(
        "The drive or image changed. Review a new installation plan.",
      );
    const approved = await this.confirmFlash(current);
    if (!approved) return null;
    record.started = true;
    this.lastFlashedPlan = current;
    // Do not pass --yes: Wendy still asks for name, networking, elevation and disk confirmation.
    return this.sessions.start(
      `Flash ${current.device_type}`,
      [this.cliSpec(current.command.slice(1))],
      { exclusive: "flash" },
    );
  }

  async verify({ address, planId }) {
    const args = [
      "install",
      "verify",
      "--address",
      text(address, "device address"),
      "--timeout",
      "15s",
    ];
    const plan = this.plans.get(planId)?.plan ?? this.lastFlashedPlan;
    if (plan)
      args.push(
        "--expected-device-type",
        plan.device_type,
        "--expected-os-version",
        plan.version,
      );
    return JSON.parse(
      await this.run(this.cli, args, { timeout: 25_000, verification: true }),
    );
  }

  async discover() {
    const collection = JSON.parse(
      await this.run(this.cli, ["discover", "--json"], { timeout: 20_000 }),
    );
    const devices = new Map();
    for (const [key, transport] of [["lanDevices", "Network"], ["ethernetDevices", "Ethernet"], ["usbDevices", "USB"]]) {
      for (const device of collection[key] || []) {
        if (!device.isWendyDevice) continue;
        const host = device.hostname || device.ipAddress;
        if (!host) continue;
        const address = device.port ? `${host.includes(":") ? `[${host}]` : host}:${device.port}` : host;
        const target = deviceTarget(address);
        if (!devices.has(target)) devices.set(target, { name: device.displayName || device.name || host, target, address, transport, version: device.agentVersion });
      }
    }
    return [...devices.values()];
  }
}
