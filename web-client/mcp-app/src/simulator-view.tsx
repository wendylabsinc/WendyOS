import { useEffect, useRef, useState } from "react";
import { SandboxViewer } from "../../../go/simulator/go2/go2_sim/viewer.js";
import { rosmasterScene } from "./rosmaster-scene.js";
import { sceneBridge } from "./bridge";
import {
  closeSceneSession,
  readScene,
  type SceneSession,
} from "./simulator-session";

// Reuse the simulator's world-pose renderer for all robot profiles. No physics
// or motion commands run in this component. Rendering stays local; the host
// tool bridge supplies geometry once and bounded pose snapshots.
export function SimulatorView({
  session,
  profile,
  name,
}: {
  session: SceneSession;
  profile?: string;
  name: string;
}) {
  const canvas = useRef<HTMLCanvasElement>(null);
  const status = useRef<HTMLParagraphElement>(null);
  const renderer = useRef<SandboxViewer | undefined>(undefined);
  const [failure, setFailure] = useState("");
  const [failureDetails, setFailureDetails] = useState("");
  const [follow, setFollow] = useState(true);

  useEffect(() => {
    if (!canvas.current || !status.current) return;
    let disposed = false;
    let inView = true;
    let terminal = false;
    let lifetime = new AbortController();
    let failures = 0;
    let received = false;
    setFailure("");
    setFailureDetails("");
    setFollow(true);
    let view: SandboxViewer;
    const fail = (message: string, details = message) => {
      if (disposed) return;
      terminal = true;
      lifetime.abort();
      view?.dispose();
      renderer.current = undefined;
      closeSceneSession(session, sceneBridge);
      setFailure(message);
      setFailureDetails(details);
      if (status.current) status.current.textContent = "Scene unavailable.";
    };
    try {
      view = new SandboxViewer(canvas.current, status.current, {
        lidar: false,
        frameInterval: session.resource_uri ? 250 : 1000 / 20,
        pollAfterResponse: !!session.resource_uri,
        replayHistory: !!session.resource_uri,
        viewOffset: profile === "g1" ? [2.4, -2.7, 1.5] : profile === "rosmaster-r2" ? [.85, -1.1, .72] : [1.35, -1.6, 0.95],
        request: async (path) => {
          const signal = lifetime.signal;
          try {
            const result = await readScene(session, path, signal, sceneBridge, path === "/api/scene/state" && session.resource_uri ? view?.lastSequence ?? 0 : undefined);
            if (signal.aborted) throw Error("Viewer suspended");
            const scene = path === "/api/scene" && profile === "rosmaster-r2"
              ? rosmasterScene(result)
              : result;
            failures = 0;
            if (path === "/api/scene/state") received = true;
            return scene;
          } catch (error) {
            if (disposed || signal.aborted) throw error;
            const message =
              error instanceof Error ? error.message : String(error);
            if (/session (?:ended|expired)/.test(message)) fail(message);
            else if (!received && ++failures >= 3) {
              fail(
                "The simulation scene could not load. Reconnect the view or open it in your browser.",
                message,
              );
            }
            throw error;
          }
        },
      });
      renderer.current = view;
    } catch (error) {
      closeSceneSession(session, sceneBridge);
      setFailure(
        "3D graphics are unavailable here. Open the simulation in your browser.",
      );
      setFailureDetails(error instanceof Error ? error.message : String(error));
      status.current.textContent = "Scene unavailable.";
      return () => {
        disposed = true;
        lifetime.abort();
      };
    }
    let wasVisible = true;
    const active = () => {
      const visible = !document.hidden && inView && !terminal;
      if (!visible) lifetime.abort();
      else if (lifetime.signal.aborted) lifetime = new AbortController();
      view.setActive(visible);
      if (!visible && wasVisible && session.resource_uri)
        void sceneBridge.pause?.(session.token).catch(() => {});
      wasVisible = visible;
    };
    const observer = new IntersectionObserver(([entry]) => {
      inView = entry.isIntersecting;
      active();
    });
    observer.observe(canvas.current);
    document.addEventListener("visibilitychange", active);
    const lost = () =>
      fail(
        "3D graphics were interrupted. Choose Reconnect view to restore the scene.",
      );
    canvas.current.addEventListener("webglcontextlost", lost);
    const expiry = setTimeout(
      () => fail("The viewer session expired. Choose Reconnect view."),
      Math.max(0, Date.parse(session.expires_at) - Date.now()),
    );
    const element = canvas.current;
    active();
    return () => {
      disposed = true;
      clearTimeout(expiry);
      lifetime.abort();
      observer.disconnect();
      document.removeEventListener("visibilitychange", active);
      element.removeEventListener("webglcontextlost", lost);
      view.dispose();
      renderer.current = undefined;
    };
  }, [session, profile]);

  return (
    <div className="simulator-live-view">
      <div className="simulator-view-controls">
        <button
          type="button"
          disabled={!!failure}
          onClick={() => renderer.current?.resetView()}
        >
          Reset camera
        </button>
        <button
          type="button"
          disabled={!!failure}
          aria-label="Zoom in"
          onClick={() => renderer.current?.zoom(0.8)}
        >
          Zoom in
        </button>
        <button
          type="button"
          disabled={!!failure}
          aria-label="Zoom out"
          onClick={() => renderer.current?.zoom(1.25)}
        >
          Zoom out
        </button>
        <label>
          <input
            type="checkbox"
            checked={follow}
            disabled={!!failure}
            onChange={(event) => {
              setFollow(event.target.checked);
              renderer.current?.setFollowRobot(event.target.checked);
            }}
          />{" "}
          Follow robot
        </label>
      </div>
      <canvas
        ref={canvas}
        className="simulator-viewer-frame"
        aria-label={`${name} live 3D simulation. Drag to orbit; scroll or use the buttons to zoom.`}
      />
      <p ref={status} className="simulator-render-status" role="status">
        Connecting to the simulation...
      </p>
      {failure && (
        <div className="simulator-embed-note" role="alert">
          <p>{failure}</p>
          <details>
            <summary>Error details</summary>
            <p>{failureDetails}</p>
          </details>
        </div>
      )}
    </div>
  );
}
