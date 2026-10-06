import { useEffect, useRef, useState } from "react";
import type { DesktopAPI, SessionEvent } from "./types";
import "@xterm/xterm/css/xterm.css";

export default function Terminal({ api, id }: { api: DesktopAPI; id: string }) {
  const mount = useRef<HTMLDivElement>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    let disposed = false;
    let cleanup = () => {};
    void (async () => {
      const [{ Terminal }, { FitAddon }] = await Promise.all([
        import("@xterm/xterm"),
        import("@xterm/addon-fit"),
      ]);
      if (disposed || !mount.current) return;
      const terminal = new Terminal({
        cursorBlink: true,
        fontSize: 14,
        fontFamily: '"SFMono-Regular", Consolas, monospace',
        scrollback: 5000,
        theme: {
          background: "#101416",
          foreground: "#e4ece7",
          cursor: "#b8f5ce",
          selectionBackground: "#385043",
          green: "#b8f5ce",
        },
      });
      const fit = new FitAddon();
      terminal.loadAddon(fit);
      terminal.open(mount.current);
      let ready = false;
      let sequence = 0;
      const buffered: SessionEvent[] = [];
      const receive = (event: SessionEvent) => {
        if (event.id !== id || event.type !== "data") return;
        if (!ready) {
          buffered.push(event);
          return;
        }
        if (event.sequence > sequence) {
          terminal.write(event.data || "");
          sequence = event.sequence;
        }
      };
      const unsubscribe = api.onSession(receive);
      const input = terminal.onData((data) => {
        void api.input(id, data).catch((e) => setError(e.message));
      });
      const resize = () => {
        if (mount.current?.clientWidth && mount.current.clientHeight) {
          fit.fit();
          void api.resize(id, terminal.cols, terminal.rows).catch(() => {});
        }
      };
      const observer = new ResizeObserver(resize);
      observer.observe(mount.current);
      cleanup = () => {
        unsubscribe();
        input.dispose();
        observer.disconnect();
        terminal.dispose();
      };
      const snapshot = await api.snapshot(id);
      if (disposed) return;
      terminal.write(snapshot.output);
      sequence = snapshot.sequence;
      ready = true;
      for (const event of buffered) receive(event);
      resize();
      terminal.focus();
    })().catch((error) => {
      if (!disposed) setError(error.message);
    });
    return () => {
      disposed = true;
      cleanup();
    };
  }, [api, id]);
  return (
    <>
      <div
        className="desktop-terminal"
        ref={mount}
        aria-label="Interactive Wendy terminal"
      />
      {error && (
        <p role="alert" className="desktop-error">
          {error}
        </p>
      )}
    </>
  );
}
