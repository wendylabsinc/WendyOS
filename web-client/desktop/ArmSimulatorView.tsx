import { useCallback, useEffect, useRef, useState } from "react";
import { SandboxViewer } from "../../go/simulator/go2/go2_sim/viewer.js";
import type { DesktopAPI, Simulator } from "./types";

type Joint = {
  name: string;
  position_deg: number;
  target_deg: number;
  min_deg: number;
  max_deg: number;
};
type ArmStatus = {
  healthy: boolean;
  paused: boolean;
  error?: string;
  joints: Joint[];
};

export default function ArmSimulatorView({
  api,
  simulator,
}: {
  api: DesktopAPI;
  simulator: Simulator;
}) {
  const canvas = useRef<HTMLCanvasElement>(null);
  const label = useRef<HTMLSpanElement>(null);
  const viewer = useRef<SandboxViewer | null>(null);
  const [status, setStatus] = useState<ArmStatus>();
  const [error, setError] = useState("");
  // Cleared by the next successful poll, unlike an action's error.
  const [pollError, setPollError] = useState("");
  const [busy, setBusy] = useState(false);
  const pending = useRef(false);
  const request = useCallback(
    (path: string, body?: Record<string, unknown>) =>
      api.simulatorRequest({
        id: simulator.id,
        path,
        ...(body === undefined ? {} : { body }),
      }),
    [api, simulator.id],
  );
  useEffect(() => {
    if (!canvas.current || !label.current) return;
    try {
      const view = new SandboxViewer(canvas.current, label.current, {
        request,
        lidar: false,
        viewOffset: [0.5, -0.65, 0.4],
      });
      viewer.current = view;
      return () => {
        viewer.current = null;
        view.dispose();
      };
    } catch (e) {
      label.current.textContent = String(e);
    }
  }, [request]);
  useEffect(() => {
    let disposed = false;
    let timer: ReturnType<typeof setTimeout>;
    async function poll() {
      try {
        const next = (await request("/api/status")) as ArmStatus;
        if (!disposed) {
          setStatus(next);
          setPollError("");
        }
      } catch (e) {
        if (!disposed) setPollError(String(e));
      } finally {
        if (!disposed) timer = setTimeout(poll, 500);
      }
    }
    void poll();
    return () => {
      disposed = true;
      clearTimeout(timer);
    };
  }, [request]);
  const act = async (path: string, body: Record<string, unknown> = {}) => {
    if (pending.current) return;
    pending.current = true;
    setBusy(true);
    setError("");
    try {
      setStatus((await request(path, body)) as ArmStatus);
    } catch (e) {
      setError(String(e));
    } finally {
      pending.current = false;
      setBusy(false);
    }
  };
  return (
    <div className="desktop-robot">
      <div className="desktop-robot-layout">
        <div className="desktop-robot-scene">
          <div className="desktop-robot-canvas">
            <canvas
              ref={canvas}
              aria-label="Live SO-101 virtual arm. Drag to orbit, scroll to zoom."
            />
            <span
              ref={label}
              className="desktop-robot-view-status"
              role="status"
            >
              Loading arm…
            </span>
          </div>
          <p>Drag to orbit · Shift-drag to pan · Scroll to zoom</p>
          <button onClick={() => viewer.current?.resetView()}>
            Reset camera
          </button>
        </div>
        <aside
          className="desktop-robot-controls"
          aria-label="Virtual joint controls"
        >
          <h3>SO-101 joint targets</h3>
          <p>Virtual arm only · degrees</p>
          {status?.joints.map((joint) => (
            <label key={joint.name} style={{ display: "block" }}>
              {joint.name.replaceAll("_", " ")} ·{" "}
              {joint.position_deg.toFixed(1)}°
              <input
                type="range"
                aria-label={`${joint.name} target degrees`}
                min={joint.min_deg}
                max={joint.max_deg}
                step={0.1}
                defaultValue={joint.target_deg}
                key={`${joint.name}:${joint.target_deg}`}
                disabled={busy || !status.healthy}
                onPointerUp={(e) =>
                  void act("/api/pose", {
                    joints_deg: { [joint.name]: Number(e.currentTarget.value) },
                  })
                }
                onKeyUp={(e) => {
                  if (
                    [
                      "ArrowLeft",
                      "ArrowRight",
                      "ArrowUp",
                      "ArrowDown",
                      "Home",
                      "End",
                      "PageUp",
                      "PageDown",
                    ].includes(e.key)
                  )
                    void act("/api/pose", {
                      joints_deg: {
                        [joint.name]: Number(e.currentTarget.value),
                      },
                    });
                }}
              />
            </label>
          ))}
          <div className="desktop-actions">
            <button
              disabled={busy}
              onClick={() =>
                void act("/api/pause", { paused: !status?.paused })
              }
            >
              {status?.paused ? "Resume" : "Pause"}
            </button>
            <button disabled={busy} onClick={() => void act("/api/reset")}>
              Reset arm
            </button>
            <button disabled={busy} onClick={() => void act("/api/demo")}>
              Wave
            </button>
          </div>
        </aside>
      </div>
      {(error || pollError || status?.error) && (
        <p className="desktop-error" role="alert">
          {error || pollError || status?.error}
        </p>
      )}
    </div>
  );
}
