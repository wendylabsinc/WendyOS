export const APP_URL = "wendy://app/index.html";
export const RUNTIMES = new Set(["docker", "apple-container"]);

export function runtimeCommand(runtime) {
  if (!RUNTIMES.has(runtime))
    throw new Error("Choose Docker or Apple Container.");
  return runtime === "docker" ? "docker" : "container";
}

export function text(value, label, max = 256) {
  if (
    typeof value !== "string" ||
    !value.trim() ||
    value.length > max ||
    /[\x00-\x1f]/.test(value)
  ) {
    throw new Error(`Invalid ${label}.`);
  }
  return value.trim();
}

export function simulatorName(value) {
  const name = text(value, "simulator name", 48);
  if (!/^[a-z][a-z0-9-]*$/.test(name))
    throw new Error("Use a lowercase name with letters, numbers, and hyphens.");
  return name;
}

export function simulatorKind(value = "go2") {
  if (!["go2", "g1", "raspberry-pi"].includes(value))
    throw new Error("Choose Go2, G1, or Raspberry Pi app simulation.");
  return value;
}

export function deviceTarget(value) {
  const target = text(value, "device target");
  if (
    target.startsWith("-") ||
    ["docker", "apple-container", "local", "sim", "simulator"].includes(target)
  )
    throw new Error(
      "Choose an explicit Wendy device address or vm:name for monitoring.",
    );
  return target;
}

export function loopbackURL(value) {
  const url = new URL(value);
  if (
    url.protocol !== "http:" ||
    url.hostname !== "127.0.0.1" ||
    !url.port ||
    url.username ||
    url.password ||
    url.pathname !== "/" ||
    url.search ||
    url.hash
  ) {
    throw new Error("The simulator must use a local loopback address.");
  }
  return url.href;
}

export function trustedSender(event, contents, devURL) {
  if (event.sender !== contents || event.senderFrame !== contents.mainFrame)
    return false;
  const url = new URL(event.senderFrame.url);
  if (devURL) return url.origin === new URL(devURL).origin;
  return url.protocol === "wendy:" && url.hostname === "app";
}

export function installArgs(options) {
  const boards = new Set([
    "raspberry-pi-3",
    "raspberry-pi-4",
    "raspberry-pi-5",
    "jetson-orin-nano",
    "jetson-agx-orin",
    "jetson-agx-thor",
  ]);
  if (!options || !boards.has(options.deviceType))
    throw new Error("Choose a supported board.");
  const args = ["install", "plan", "--device-type", options.deviceType];
  if (options.deviceType.startsWith("jetson-"))
    args.push("--carrier", "developer-kit");
  if (options.deviceType === "jetson-agx-orin") args.push("--storage", "nvme");
  if (options.deviceType.startsWith("raspberry-"))
    args.push("--drive", text(options.drive, "drive path"));
  if (options.version)
    args.push("--version", text(options.version, "version", 100));
  return args;
}

export function sameInstallTarget(before, after) {
  return (
    before.device_type === after.device_type &&
    before.version === after.version &&
    before.artifact_sha256 === after.artifact_sha256 &&
    before.method === after.method &&
    JSON.stringify(before.target) === JSON.stringify(after.target) &&
    JSON.stringify(before.command) === JSON.stringify(after.command)
  );
}

export function simRunArgs(sim, image) {
  runtimeCommand(sim.runtime);
  const kind = simulatorKind(sim.kind);
  if (kind === "raspberry-pi")
    throw new Error("Raspberry Pi app simulation uses QEMU.");
  const prefix = kind.toUpperCase();
  const args = [
    "run",
    "--detach",
    "--name",
    sim.container,
    "--label",
    `dev.wendy.desktop.simulator=${sim.id}`,
    "--publish",
    `127.0.0.1:${sim.port}:8890`,
    "--cpus",
    "4",
    "--memory",
    "4g",
    "--env",
    `${prefix}_SOURCE_DIGEST=${sim.digest}`,
    "--env",
    `${prefix}_RENDER=1`,
    "--env",
    `${prefix}_AUTO_APP_CONTROL=1`,
  ];
  args.push(image);
  return args;
}
