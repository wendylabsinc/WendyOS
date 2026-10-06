/* Camera frames are local JPEG blobs, not Next.js image assets. */
/* eslint-disable @next/next/no-img-element */
import { useCallback, useEffect, useRef, useState } from "react";
import {
  ArrowDown,
  ArrowLeft,
  ArrowRight,
  ArrowUp,
  Minus,
  Plus,
  RotateCcw,
  RotateCw,
  Square,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { SandboxViewer } from "../../go/simulator/go2/go2_sim/viewer.js";
import type { DesktopAPI, Simulator } from "./types";

type Source = {
  publisher_gid: string;
  node_name?: string;
  kind: string;
  age_ms: number;
  requires_restart?: boolean;
};
type Status = {
  mode: string;
  armed: boolean;
  error?: string;
  metrics?: { real_time_factor: number; policy_p95_ms?: number };
  sensor_settings?: {
    lidar_enabled: boolean;
    camera_enabled: boolean;
    lidar_dropout: number;
  };
  ros_commands?: { owner: string | null; sources: Source[] };
};

export default function SimulatorView({
  api,
  simulator,
}: {
  api: DesktopAPI;
  simulator: Simulator;
}) {
  const root = useRef<HTMLDivElement>(null);
  const canvas = useRef<HTMLCanvasElement>(null);
  const viewStatus = useRef<HTMLSpanElement>(null);
  const viewer = useRef<SandboxViewer | null>(null);
  const token = useRef<string | null>(null);
  const mounted = useRef(false);
  const keys = useRef(new Set<string>());
  const [status, setStatus] = useState<Status>();
  const [armed, setArmed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [camera, setCamera] = useState(false);
  const [cameraURL, setCameraURL] = useState("");
  const [follow, setFollow] = useState(true);
  const [lidar, setLidar] = useState(true);
  const [source, setSource] = useState("");
  const [obstacle, setObstacle] = useState([2.5, 1.5]);
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
    if (!canvas.current || !viewStatus.current) return;
    try {
      const instance = new SandboxViewer(canvas.current, viewStatus.current, {
        request,
        wheelZoom: false,
        lidar: simulator.kind === "go2",
        viewOffset:
          simulator.kind === "g1" ? [2.4, -2.7, 1.5] : [1.35, -1.6, 0.95],
      });
      viewer.current = instance;
      return () => {
        viewer.current = null;
        instance.dispose();
      };
    } catch (error) {
      viewStatus.current.textContent = `3D view unavailable: ${error instanceof Error ? error.message : String(error)}`;
    }
  }, [request, simulator.kind]);

  useEffect(() => {
    viewer.current?.setActive(!camera);
  }, [camera]);
  useEffect(() => {
    viewer.current?.setFollowRobot(follow);
  }, [follow]);
  useEffect(() => {
    viewer.current?.setLidarEnabled(lidar);
  }, [lidar]);

  useEffect(() => {
    let disposed = false;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      try {
        const lease = token.current;
        const next = (await request("/api/status")) as Status;
        if (disposed) return;
        setStatus(next);
        if (!next.armed && token.current && token.current === lease) {
          token.current = null;
          setArmed(false);
          keys.current.clear();
        }
      } catch (error) {
        if (!disposed)
          setError(error instanceof Error ? error.message : String(error));
      } finally {
        if (!disposed) timer = setTimeout(poll, 500);
      }
    };
    void poll();
    return () => {
      disposed = true;
      clearTimeout(timer);
    };
  }, [request]);

  useEffect(() => {
    mounted.current = true;
    let disposed = false,
      sending = false;
    const clear = () => keys.current.clear();
    const timer = setInterval(async () => {
      const lease = token.current;
      if (!lease || sending) return;
      sending = true;
      const held = (key: string) => Number(keys.current.has(key));
      const velocity = keys.current.has(" ")
        ? [0, 0, 0]
        : [
            0.35 * (held("w") - held("s")),
            0.25 * (held("a") - held("d")),
            0.6 * (held("q") - held("e")),
          ];
      try {
        await request("/api/command", { token: lease, velocity });
      } catch (error) {
        if (!disposed && token.current === lease) {
          token.current = null;
          clear();
          setArmed(false);
          setError(error instanceof Error ? error.message : String(error));
        }
      } finally {
        sending = false;
      }
    }, 50);
    window.addEventListener("blur", clear);
    document.addEventListener("visibilitychange", clear);
    return () => {
      mounted.current = false;
      disposed = true;
      clearInterval(timer);
      clear();
      window.removeEventListener("blur", clear);
      document.removeEventListener("visibilitychange", clear);
      const lease = token.current;
      token.current = null;
      if (lease) void request("/api/release", { token: lease }).catch(() => {});
    };
  }, [request]);

  useEffect(() => {
    if (!camera) return;
    let disposed = false,
      objectURL = "";
    let timer: ReturnType<typeof setTimeout>;
    const frame = async () => {
      try {
        if (document.hidden) return;
        const { bytes } = (await request("/camera.jpg")) as {
          bytes: Uint8Array;
        };
        if (disposed) return;
        const next = URL.createObjectURL(
          new Blob([new Uint8Array(bytes)], { type: "image/jpeg" }),
        );
        if (objectURL) URL.revokeObjectURL(objectURL);
        objectURL = next;
        setCameraURL(next);
      } catch (error) {
        if (!disposed)
          setError(error instanceof Error ? error.message : String(error));
      } finally {
        if (!disposed) timer = setTimeout(frame, 100);
      }
    };
    void frame();
    return () => {
      disposed = true;
      clearTimeout(timer);
      if (objectURL) URL.revokeObjectURL(objectURL);
      setCameraURL("");
    };
  }, [camera, request]);

  const act = async (action: string, body: Record<string, unknown> = {}) => {
    keys.current.clear();
    setBusy(true);
    setError("");
    try {
      const result = (await request(`/api/${action}`, {
        token: token.current,
        ...body,
      })) as { token?: string };
      if (!mounted.current) {
        if (action === "arm" && result.token)
          void request("/api/release", { token: result.token }).catch(() => {});
        return;
      }
      if (action === "arm") {
        token.current = result.token || null;
        root.current?.focus({ preventScroll: true });
      }
      if (["release", "pause", "resume", "reset", "arm_ros"].includes(action))
        token.current = null;
      setArmed(!!token.current);
    } catch (error) {
      setError(error instanceof Error ? error.message : String(error));
    } finally {
      setBusy(false);
    }
  };
  const sources =
    status?.ros_commands?.sources.filter(
      (s) => s.age_ms < 1500 || s.publisher_gid === status.ros_commands?.owner,
    ) || [];
  const selected = sources.find((s) => s.publisher_gid === source);
  const sensors = status?.sensor_settings;
  return (
    <div
      className="desktop-robot"
      ref={root}
      tabIndex={0}
      aria-label={`${simulator.name} simulator controls`}
      onKeyDown={(event) => {
        if (
          (event.target as HTMLElement).closest(
            "input,select,textarea,button,[contenteditable]",
          )
        )
          return;
        const key = event.key.toLowerCase();
        if (armed && ["w", "a", "s", "d", "q", "e", " "].includes(key)) {
          event.preventDefault();
          keys.current.add(key);
        }
      }}
      onKeyUp={(event) => keys.current.delete(event.key.toLowerCase())}
      onBlur={(event) => {
        if (!event.currentTarget.contains(event.relatedTarget))
          keys.current.clear();
      }}
    >
      <div className="desktop-robot-toolbar">
        <div className="desktop-actions">
          <Button
            size="sm"
            variant={!camera ? "default" : "ghost"}
            aria-pressed={!camera}
            onClick={() => setCamera(false)}
          >
            3D view
          </Button>
          <Button
            size="sm"
            variant={camera ? "default" : "ghost"}
            aria-pressed={camera}
            onClick={() => setCamera(true)}
          >
            Robot camera
          </Button>
        </div>
        {!camera && (
          <div className="desktop-actions">
            <Button
              size="sm"
              variant="ghost"
              aria-pressed={follow}
              onClick={() => setFollow(!follow)}
            >
              Follow robot {follow ? "on" : "off"}
            </Button>
            {simulator.kind === "go2" && (
              <Button
                size="sm"
                variant="ghost"
                aria-pressed={lidar}
                onClick={() => setLidar(!lidar)}
              >
                Lidar {lidar ? "on" : "off"}
              </Button>
            )}
            <Button
              size="icon-sm"
              variant="ghost"
              aria-label="Zoom in"
              onClick={() => viewer.current?.zoom(0.8)}
            >
              <Plus />
            </Button>
            <Button
              size="icon-sm"
              variant="ghost"
              aria-label="Zoom out"
              onClick={() => viewer.current?.zoom(1.25)}
            >
              <Minus />
            </Button>
            <Button
              size="sm"
              variant="ghost"
              onClick={() => viewer.current?.resetView()}
            >
              Reset view
            </Button>
          </div>
        )}
      </div>
      <div className="desktop-robot-layout">
        <div className="desktop-robot-scene">
          <div
            className="desktop-robot-canvas"
            onPointerDown={() => root.current?.focus({ preventScroll: true })}
          >
            <canvas
              ref={canvas}
              hidden={camera}
              aria-label={`Live ${simulator.kind === "g1" ? "G1" : "Go2"} 3D view`}
            />
            {camera &&
              (cameraURL ? (
                <img src={cameraURL} alt="Live robot sensor camera" />
              ) : (
                <span className="desktop-camera-loading">
                  Connecting to robot camera…
                </span>
              ))}
            <span
              ref={viewStatus}
              className="desktop-robot-view-status"
              hidden={camera}
              role="status"
            >
              Loading 3D scene…
            </span>
          </div>
          <div className="desktop-robot-caption">
            <span>
              {camera
                ? "Live front sensor camera"
                : "Drag to orbit · Shift-drag to pan · + / − to zoom"}
            </span>
            {status?.metrics && (
              <span>
                {status.metrics.real_time_factor.toFixed(2)}× real time
              </span>
            )}
          </div>
        </div>
        <aside className="desktop-robot-controls" aria-label="Robot controls">
          <div className="desktop-robot-control-heading">
            <h3>Robot controls</h3>
            <span>{status?.mode || simulator.mode}</span>
          </div>
          <Button
            disabled={busy}
            aria-pressed={armed}
            onClick={() => void act(armed ? "release" : "arm")}
          >
            {armed ? "Release controls" : "Enable controls"}
          </Button>
          <div className="desktop-robot-drive">
            {(
              [
                ["q", "Turn left", RotateCcw],
                ["w", "Walk forward", ArrowUp],
                ["e", "Turn right", RotateCw],
                ["a", "Strafe left", ArrowLeft],
                ["s", "Walk backward", ArrowDown],
                ["d", "Strafe right", ArrowRight],
              ] as const
            ).map(([key, label, Icon]) => (
              <Button
                key={key}
                variant="outline"
                aria-label={`Hold to ${label.toLowerCase()}`}
                title={`${label} · ${key.toUpperCase()}`}
                disabled={!armed || busy}
                onPointerDown={(event) => {
                  event.preventDefault();
                  event.currentTarget.setPointerCapture(event.pointerId);
                  keys.current.add(key);
                }}
                onPointerUp={() => keys.current.delete(key)}
                onLostPointerCapture={() => keys.current.delete(key)}
                onKeyDown={(event) => {
                  if ([" ", "Enter"].includes(event.key)) {
                    event.preventDefault();
                    keys.current.add(key);
                  }
                }}
                onKeyUp={() => keys.current.delete(key)}
                onBlur={() => keys.current.delete(key)}
              >
                <Icon />
                <kbd>{key.toUpperCase()}</kbd>
              </Button>
            ))}
          </div>
          <Button
            variant="outline"
            disabled={busy}
            onClick={() => void act("stop")}
          >
            <Square /> Stop motion
          </Button>
          <p className="desktop-muted">
            Enable controls, then hold a direction. Click the view to use W A S
            D, Q / E to turn, or Space to stop.
          </p>
          <div className="desktop-actions">
            <Button
              size="sm"
              variant="outline"
              disabled={busy}
              onClick={() =>
                void act(status?.mode === "paused" ? "resume" : "pause")
              }
            >
              {status?.mode === "paused" ? "Resume" : "Pause"}
            </Button>
            <Button
              size="sm"
              variant="ghost"
              disabled={busy}
              onClick={() => void act("reset")}
            >
              Reset world
            </Button>
          </div>
        </aside>
      </div>
      {(error || status?.error) && (
        <p className="desktop-error" role="alert">
          {error || status?.error}
        </p>
      )}
      <div className="desktop-robot-settings">
        <details>
          <summary>App control</summary>
          <div className="desktop-actions">
            <select
              aria-label="ROS command source"
              value={source}
              onChange={(event) => setSource(event.target.value)}
            >
              <option value="">
                {sources.length ? "Choose an app" : "Waiting for a ROS app…"}
              </option>
              {sources.map((s) => (
                <option
                  key={s.publisher_gid}
                  value={s.publisher_gid}
                  disabled={s.requires_restart || s.kind === "motion_switcher"}
                >
                  {s.node_name || s.kind}
                  {s.requires_restart ? " · restart app" : ""}
                </option>
              ))}
            </select>
            <Button
              size="sm"
              disabled={
                busy ||
                !selected ||
                selected.requires_restart ||
                selected.age_ms >= 1000 ||
                selected.kind === "motion_switcher"
              }
              onClick={() => void act("arm_ros", { publisher_gid: source })}
            >
              Give app control
            </Button>
            <Button
              size="sm"
              variant="ghost"
              disabled={busy}
              onClick={() => void act("disarm_ros")}
            >
              Release app control
            </Button>
          </div>
          <p>
            {status?.ros_commands?.owner
              ? "An app has control."
              : "Publish ROS commands in the simulator runtime, then select the app here."}
          </p>
        </details>
        <details>
          <summary>World & sensors</summary>
          <div className="desktop-actions">
            {obstacle.map((value, i) => (
              <label key={i}>
                Box {i ? "Y" : "X"}
                <input
                  type="number"
                  min={-5.3}
                  max={5.3}
                  step={0.1}
                  value={value}
                  onChange={(e) =>
                    setObstacle((old) =>
                      old.map((v, index) =>
                        index === i ? e.target.valueAsNumber : v,
                      ),
                    )
                  }
                />
              </label>
            ))}
            <Button
              size="sm"
              variant="outline"
              disabled={busy || obstacle.some((v) => !Number.isFinite(v))}
              onClick={() => void act("obstacle", { position: obstacle })}
            >
              Place box
            </Button>
            {sensors && (
              <>
                <label>
                  <input
                    type="checkbox"
                    checked={sensors.lidar_enabled}
                    disabled={busy}
                    onChange={(e) =>
                      void act("sensors", { lidar_enabled: e.target.checked })
                    }
                  />{" "}
                  Lidar active
                </label>
                <label>
                  <input
                    type="checkbox"
                    checked={sensors.camera_enabled}
                    disabled={busy}
                    onChange={(e) =>
                      void act("sensors", { camera_enabled: e.target.checked })
                    }
                  />{" "}
                  Camera active
                </label>
                <label>
                  Lidar dropout %
                  <input
                    type="number"
                    min={0}
                    max={100}
                    step={5}
                    defaultValue={Math.round(sensors.lidar_dropout * 100)}
                    onBlur={(e) => {
                      if (e.target.checkValidity())
                        void act("sensors", {
                          lidar_dropout: e.target.valueAsNumber / 100,
                        });
                    }}
                  />
                </label>
              </>
            )}
          </div>
        </details>
      </div>
    </div>
  );
}
