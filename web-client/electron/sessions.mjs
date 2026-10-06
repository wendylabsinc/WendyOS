import { randomUUID } from "node:crypto";

// PTYs preserve Wendy's chat history, tool approvals, sudo prompts and progress UI.
// Only the main process constructs commands; the renderer receives session IDs.
export class Sessions {
  constructor({ spawn, emit, env, cwd }) {
    this.spawn = spawn;
    this.emit = emit;
    this.env = env;
    this.cwd = cwd;
    this.items = new Map();
  }

  start(title, commands, { exclusive, onSuccess } = {}) {
    if (
      exclusive &&
      [...this.items.values()].some(
        (s) => s.running && s.exclusive === exclusive,
      )
    ) {
      throw new Error(`${title} is already running.`);
    }
    if ([...this.items.values()].filter((s) => s.running).length >= 8)
      throw new Error("Stop a session before opening another.");
    const session = {
      id: randomUUID(),
      title,
      running: true,
      output: "",
      sequence: 0,
      exclusive,
      cols: 110,
      rows: 28,
    };
    this.items.set(session.id, session);
    // Run after returning the ID. A snapshot handles any early output.
    setImmediate(() => void this.run(session, commands, onSuccess));
    return this.summary(session);
  }

  summary(s) {
    return {
      id: s.id,
      title: s.title,
      running: s.running,
      exitCode: s.exitCode,
      sequence: s.sequence,
    };
  }

  list() {
    return [...this.items.values()].map((s) => this.summary(s));
  }

  get(id) {
    const session = this.items.get(id);
    if (!session) throw new Error("Session no longer exists.");
    return session;
  }

  append(session, data) {
    session.output = (session.output + data).slice(-512_000);
    this.emit({
      ...this.summary(session),
      type: "data",
      sequence: ++session.sequence,
      data,
    });
  }

  async run(session, commands, onSuccess) {
    let code = 0;
    try {
      for (const command of commands) {
        if (session.cancelled) {
          code = 130;
          break;
        }
        const spec = typeof command === "function" ? await command() : command;
        if (session.cancelled) {
          code = 130;
          break;
        }
        this.append(
          session,
          `\r\n\x1b[32m${spec.label || session.title}\x1b[0m\r\n`,
        );
        code = await new Promise((resolve, reject) => {
          let child;
          try {
            child = this.spawn(spec.file, spec.args, {
              name: "xterm-256color",
              cols: session.cols,
              rows: session.rows,
              cwd: spec.cwd || this.cwd,
              env: { ...this.env, ...spec.env },
            });
          } catch (error) {
            reject(error);
            return;
          }
          session.child = child;
          const data = child.onData((data) => this.append(session, data));
          child.onExit(({ exitCode }) => {
            data.dispose();
            session.child = null;
            resolve(exitCode);
          });
        });
        if (code !== 0) break;
      }
      if (code === 0 && !session.cancelled && onSuccess)
        await onSuccess(session);
      if (session.cancelled) code = 130;
    } catch (error) {
      code = 1;
      this.append(session, `\r\n${error.message}\r\n`);
    } finally {
      session.running = false;
      session.exitCode = code;
      this.append(
        session,
        `\r\n[${code === 0 ? "Finished" : `Exited with code ${code}`}]\r\n`,
      );
      this.emit({ ...this.summary(session), type: "exit" });
      // Bound retained output even after many jobs. Live sessions are never evicted.
      const completed = [...this.items.values()].filter((s) => !s.running);
      for (const old of completed.slice(0, -20)) this.items.delete(old.id);
    }
  }

  snapshot(id) {
    const s = this.get(id);
    return { ...this.summary(s), output: s.output };
  }

  write(id, data) {
    if (typeof data !== "string" || data.length > 65_536)
      throw new Error("Invalid terminal input.");
    this.get(id).child?.write(data);
  }

  resize(id, cols, rows) {
    if (![cols, rows].every((n) => Number.isInteger(n) && n >= 2 && n <= 500))
      throw new Error("Invalid terminal size.");
    const s = this.get(id);
    s.cols = cols;
    s.rows = rows;
    s.child?.resize(cols, rows);
  }

  cancel(id) {
    const s = this.get(id);
    s.cancelled = true;
    // Let the CLI clean up its own children before terminating the PTY.
    s.child?.write("\x03");
    const child = s.child;
    const timer = setTimeout(() => {
      if (s.child === child) child?.kill();
    }, 5000);
    timer.unref();
  }

  close() {
    for (const s of this.items.values()) if (s.running) s.child?.kill();
  }
}
