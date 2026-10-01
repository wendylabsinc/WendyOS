"use client";
import { useEffect, useRef, useState } from "react";
import { Play, Square, Terminal as TerminalIcon } from "lucide-react";
import { Button } from "@/components/ui/button";
import type { Device, WendyClient } from "@/lib/client";
import "@xterm/xterm/css/xterm.css";

export default function WendyTerminal({
  device,
  client,
}: {
  device: Device;
  client?: WendyClient;
}) {
  const target = useRef<HTMLDivElement>(null);
  const [session, setSession] = useState(0);
  const [active, setActive] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    if (!target.current || !session || !client) return;
    let disposed = false;
    let cleanup = () => {};
    void (async () => {
      const [{ Terminal }, { FitAddon }] = await Promise.all([
        import("@xterm/xterm"),
        import("@xterm/addon-fit"),
      ]);
      if (disposed) return;
      const term = new Terminal({
        cursorBlink: true,
        fontSize: 14,
        fontFamily: '"SFMono-Regular", Consolas, monospace',
        lineHeight: 1.4,
        convertEol: false,
        theme: {
          background: "#101516",
          foreground: "#d6e2db",
          cursor: "#b8f5ce",
          selectionBackground: "#385043",
          green: "#b8f5ce",
          red: "#f19c94",
          yellow: "#e9cb8f",
        },
        scrollback: 3000,
      });
      const fit = new FitAddon();
      term.loadAddon(fit);
      term.open(target.current!);
      fit.fit();
      term.focus();
      let isOpen = false;
      const data = term.onData((text) => {
        if (isOpen)
          void client
            .call("shell-input", { data: text })
            .catch((e) => setError(e.message));
      });
      const unsubscribe = client?.onEvent(({ event, data }) => {
        if (event === "shell-data" && typeof data === "string") {
          term.write(Uint8Array.from(atob(data), (c) => c.charCodeAt(0)));
        }
        if (event === "shell-exit") {
          term.writeln("\r\n" + String(data));
          isOpen = false;
          setActive(false);
        }
      });
      const observer = new ResizeObserver(() => {
        fit.fit();
        if (client && isOpen)
          void client
            .call("shell-resize", { rows: term.rows, cols: term.cols })
            .catch(() => {});
      });
      observer.observe(target.current!);
      cleanup = () => {
        observer.disconnect();
        data.dispose();
        unsubscribe?.();
        term.dispose();
        if (client) void client.call("shell-close").catch(() => {});
      };
      try {
        term.writeln("Opening authenticated shell…");
        await client.call("shell-open", { rows: term.rows, cols: term.cols });
        isOpen = true;
        if (!disposed) setActive(true);
      } catch (e) {
        if (!disposed) {
          setError(e instanceof Error ? e.message : String(e));
          setActive(false);
        }
      }
    })().catch((e) => setError(String(e)));
    return () => {
      disposed = true;
      cleanup();
    };
  }, [session, device.id, device.name, device.version, client]);
  return (
    <section className="panel terminal-panel">
      <div className="panel-heading">
        <div className="actions">
          <TerminalIcon size={18} />
          <h2>{device.name.toLowerCase()} / host shell</h2>
          {active && <span className="tag live">Session open</span>}
        </div>
        <Button
          variant={active ? "outline" : "default"}
          size="sm"
          disabled={!device.online || !client}
          onClick={() => {
            setError("");
            if (active) {
              setSession(0);
              setActive(false);
            } else setSession((s) => s + 1);
          }}
        >
          {active ? (
            <>
              <Square />
              End session
            </>
          ) : (
            <>
              <Play />
              Open terminal
            </>
          )}
        </Button>
      </div>
      {error && (
        <div className="inline-error" role="alert">
          {error}
        </div>
      )}
      <div
        ref={target}
        className="terminal-mount"
        style={{ display: session ? "block" : "none" }}
      />
      {!session && (
        <div className="terminal-welcome">
          <TerminalIcon size={32} />
          <h2>A direct line to {device.name}.</h2>
          <p>
            {client
              ? "Open an interactive shell on the device. Commands run with your operator permissions."
              : "Connect to this device to open a shell."}
          </p>
          <Button
            onClick={() => setSession(1)}
            disabled={!device.online || !client}
          >
            <Play />
            Open terminal
          </Button>
        </div>
      )}
      <div className="terminal-foot">
        <span>
          {client ? "Encrypted device session" : "No device connection"}
        </span>
        <span>Ctrl+C to interrupt</span>
      </div>
    </section>
  );
}
