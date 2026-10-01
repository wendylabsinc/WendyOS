import test from "node:test";
import assert from "node:assert/strict";
import {
  installArgs,
  loopbackURL,
  runtimeCommand,
  sameInstallTarget,
  simRunArgs,
  simulatorName,
  simulatorKind,
  trustedSender,
} from "../electron/policy.mjs";

test("native bridge rejects remote pages and simulator subframes", () => {
  const frame = { url: "wendy://app/index.html" };
  const contents = { mainFrame: frame };
  assert.equal(
    trustedSender({ sender: contents, senderFrame: frame }, contents),
    true,
  );
  assert.equal(
    trustedSender({ sender: {}, senderFrame: frame }, contents),
    false,
  );
  assert.equal(
    trustedSender(
      { sender: contents, senderFrame: { url: "http://127.0.0.1:8890/" } },
      contents,
    ),
    false,
  );
  frame.url = "https://attacker.example/";
  assert.equal(
    trustedSender({ sender: contents, senderFrame: frame }, contents),
    false,
  );
  frame.url = "http://127.0.0.1:5174/";
  assert.equal(
    trustedSender(
      { sender: contents, senderFrame: frame },
      contents,
      frame.url,
    ),
    true,
  );
  assert.equal(
    trustedSender(
      { sender: contents, senderFrame: frame },
      contents,
      "http://127.0.0.1:5175/",
    ),
    false,
  );
});

test("viewer URLs cannot select a remote or credential-bearing endpoint", () => {
  assert.equal(loopbackURL("http://127.0.0.1:8890/"), "http://127.0.0.1:8890/");
  for (const url of [
    "file:///etc/passwd",
    "http://localhost:8890/",
    "https://example.com/",
    "http://user:pass@127.0.0.1:8890/",
    "http://127.0.0.1:8890/redirect",
    "http://127.0.0.1:8890/?url=example.com",
  ])
    assert.throws(() => loopbackURL(url));
});

test("runtime and simulator names are allowlisted", () => {
  assert.equal(runtimeCommand("apple-container"), "container");
  assert.equal(simulatorName("go2-lab"), "go2-lab");
  for (const name of ["--privileged", "go2; rm -rf /", "a\nb", "../../etc"])
    assert.throws(() => simulatorName(name));
  assert.throws(() => runtimeCommand("sh"));
  assert.throws(() => simulatorKind("shell"));
});

test("simulators publish only loopback and do not mount host paths or take host networking", () => {
  for (const runtime of ["docker", "apple-container"]) {
    for (const kind of ["go2", "g1"]) {
      const args = simRunArgs(
        {
          runtime,
          kind,
          id: "test",
          container: "wendy-test",
          port: 19345,
          digest: "digest",
        },
        "image:tag",
      );
      assert.equal(args[args.indexOf("--publish") + 1], "127.0.0.1:19345:8890");
      assert.ok(args.includes(`${kind.toUpperCase()}_SOURCE_DIGEST=digest`));
      assert.ok(
        !args.includes("--privileged") &&
          !args.includes("--network") &&
          !args.includes("--volume"),
      );
    }
  }
});

test("installation planning requires explicit Pi media and only supported boards", () => {
  assert.deepEqual(
    installArgs({ deviceType: "raspberry-pi-5", drive: "/dev/disk7" }),
    [
      "install",
      "plan",
      "--device-type",
      "raspberry-pi-5",
      "--drive",
      "/dev/disk7",
    ],
  );
  assert.throws(() => installArgs({ deviceType: "raspberry-pi-5" }));
  assert.throws(() => installArgs({ deviceType: "unitree-g1" }));
  assert.ok(
    installArgs({ deviceType: "jetson-orin-nano" }).includes("developer-kit"),
  );
  const plan = {
    device_type: "raspberry-pi-5",
    version: "v1",
    artifact_sha256: "abc",
    method: "removable-media",
    target: { id: "/dev/disk7", name: "SD", capacity_bytes: 16e9 },
    command: ["wendy", "install"],
  };
  assert.equal(sameInstallTarget(plan, structuredClone(plan)), true);
  for (const changed of [
    { version: "v2" },
    { artifact_sha256: "def" },
    { target: { ...plan.target, name: "Other" } },
    { command: ["wendy", "install", "--yes"] },
  ])
    assert.equal(sameInstallTarget(plan, { ...plan, ...changed }), false);
});
