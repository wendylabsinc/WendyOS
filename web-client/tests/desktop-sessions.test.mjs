import test from "node:test";
import assert from "node:assert/strict";
import { setImmediate as tick } from "node:timers/promises";
import { Sessions } from "../electron/sessions.mjs";

function fixture() {
  const children = [],
    events = [];
  const sessions = new Sessions({
    env: {},
    cwd: "/tmp",
    emit: (event) => events.push(event),
    spawn: (file, args, options) => {
      const child = {
        file,
        args,
        options,
        onData: (fn) => {
          child.data = fn;
          return { dispose() {} };
        },
        onExit: (fn) => {
          child.exit = fn;
        },
        write: (data) => {
          child.input = data;
        },
        resize: (cols, rows) => {
          child.size = [cols, rows];
        },
        kill() {},
      };
      children.push(child);
      return child;
    },
  });
  return { sessions, children, events };
}

test("sessions sequence output and preserve it across renderer subscriptions", async () => {
  const { sessions, children, events } = fixture();
  const s = sessions.start("Chat", [{ file: "wendy", args: ["chat"] }]);
  await tick();
  children[0].data("hello");
  sessions.write(s.id, "yes\r");
  sessions.resize(s.id, 120, 40);
  const snapshot = sessions.snapshot(s.id);
  assert.ok(snapshot.output.endsWith("hello"));
  assert.equal(snapshot.sequence, events.at(-1).sequence);
  assert.equal(children[0].input, "yes\r");
  assert.deepEqual(children[0].size, [120, 40]);
  children[0].exit({ exitCode: 0 });
  await tick();
  assert.equal(sessions.snapshot(s.id).running, false);
  assert.equal(sessions.snapshot(s.id).exitCode, 0);
});

test("a failed build never starts the next container command or reports readiness", async () => {
  const { sessions, children } = fixture();
  let ready = false;
  const s = sessions.start(
    "Simulator",
    [
      { file: "docker", args: ["build"] },
      { file: "docker", args: ["run"] },
    ],
    {
      exclusive: "sim",
      onSuccess: () => {
        ready = true;
      },
    },
  );
  assert.throws(
    () => sessions.start("Duplicate", [], { exclusive: "sim" }),
    /already running/,
  );
  await tick();
  children[0].exit({ exitCode: 1 });
  await tick();
  assert.equal(children.length, 1);
  assert.equal(ready, false);
  assert.equal(sessions.snapshot(s.id).exitCode, 1);
});

test("cancelling a job interrupts the PTY and suppresses remaining steps", async () => {
  const { sessions, children } = fixture();
  const s = sessions.start("Build", [
    { file: "wendy", args: ["build"] },
    { file: "wendy", args: ["run"] },
  ]);
  await tick();
  sessions.cancel(s.id);
  assert.equal(children[0].input, "\x03");
  children[0].exit({ exitCode: 0 });
  await tick();
  assert.equal(children.length, 1);
  assert.equal(sessions.snapshot(s.id).exitCode, 130);
});
