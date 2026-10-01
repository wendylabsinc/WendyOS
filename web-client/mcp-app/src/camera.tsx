import { useEffect, useRef, useState } from "react";
import { app, call, toolErrorMessage } from "./bridge";

type Frame = { data: string; mimeType: string; meta: Record<string, unknown> };
type Camera = { id: number; name: string };

export function CameraPanel({
  robot,
  cameras,
}: {
  robot: string;
  cameras: Camera[];
}) {
  const [camera, setCamera] = useState(String(cameras[0]?.id ?? ""));
  const [frame, setFrame] = useState<Frame>();
  const [live, setLive] = useState(false);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const session = useRef("");
  const epoch = useRef(0);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const readController = useRef<AbortController | undefined>(undefined);

  async function stop() {
    epoch.current++;
    clearTimeout(timer.current);
    readController.current?.abort();
    readController.current = undefined;
    setLive(false);
    setBusy("");
    const id = session.current;
    session.current = "";
    if (id) {
      try {
        await call("stop_camera_preview", { robot_id: robot, preview_id: id });
      } catch {
        /* A disconnected host also releases the server's short viewer lease. */
      }
    }
  }
  useEffect(() => {
    const hidden = () => {
      if (document.hidden) void stop();
    };
    document.addEventListener("visibilitychange", hidden);
    window.addEventListener("pagehide", stop);
    return () => {
      document.removeEventListener("visibilitychange", hidden);
      window.removeEventListener("pagehide", stop);
      void stop();
    };
  }, [robot, camera]);

  async function start() {
    const current = ++epoch.current;
    setBusy("Opening camera");
    setError("");
    try {
      const result = await call("start_camera_preview", {
        robot_id: robot,
        camera_id: Number(camera),
      });
      const id = String(result.structuredContent?.preview_id || "");
      if (!id) throw Error("The camera did not return a preview.");
      if (current !== epoch.current) {
        await call("stop_camera_preview", { robot_id: robot, preview_id: id });
        return;
      }
      session.current = id;
      const controller = new AbortController();
      readController.current = controller;
      let sequence: number | undefined;
      setLive(true);
      setBusy("");
      const next = async () => {
        try {
          const r = await call(
            "read_camera_preview",
            {
              robot_id: robot,
              preview_id: id,
              ...(sequence !== undefined && { after_sequence: sequence }),
            },
            {
              signal: controller.signal,
              priority: "background",
              timeout: 10_000,
              maxTotalTimeout: 10_000,
            },
          );
          if (current !== epoch.current) return;
          if (typeof r.structuredContent?.sequence === "number")
            sequence = r.structuredContent.sequence;
          const f = r._meta?.frame as
            | { data?: string; mimeType?: string }
            | undefined;
          if (f?.data && f.mimeType)
            setFrame({
              data: f.data,
              mimeType: f.mimeType,
              meta: {
                ...r.structuredContent,
                robot_id: robot,
                camera_id: Number(camera),
              },
            });
          // Backpressure: one read at a time, capped below the decoder's 5 fps.
          timer.current = setTimeout(next, 250);
        } catch (e) {
          if (current === epoch.current) {
            setError(toolErrorMessage(e));
            await stop();
          }
        }
      };
      await next();
    } catch (e) {
      if (current === epoch.current) {
        setError(toolErrorMessage(e));
        setBusy("");
      }
    }
  }
  async function snapshot() {
    const current = ++epoch.current;
    setBusy("Capturing frame");
    setError("");
    try {
      const r = await call("capture_robot_image", {
        robot_id: robot,
        camera_id: Number(camera),
      });
      const image = r.content.find((c) => c.type === "image");
      if (current === epoch.current && image?.type === "image")
        setFrame({ ...image, meta: r.structuredContent || {} });
    } catch (e) {
      if (current === epoch.current) setError(toolErrorMessage(e));
    } finally {
      if (current === epoch.current) setBusy("");
    }
  }
  async function attach() {
    if (!frame) return;
    const captured = frame;
    setBusy("Attaching frame");
    setError("");
    try {
      await app.updateModelContext({
        content: [
          {
            type: "text",
            text: JSON.stringify({
              ...captured.meta,
              live: false,
              kind: "camera_snapshot",
            }),
            _meta: { "openai/title": "Camera frame" },
          },
          { type: "image", data: captured.data, mimeType: captured.mimeType },
        ],
      });
    } catch (e) {
      setError(toolErrorMessage(e));
    } finally {
      setBusy("");
    }
  }
  const time =
    typeof frame?.meta.received_at === "string"
      ? new Date(frame.meta.received_at).toLocaleTimeString()
      : "";
  return (
    <section className="panel camera-panel">
      <div className="camera-heading">
        <h2>Camera</h2>
        <span className="badge">
          {live ? "Live preview" : "Preview stopped"}
        </span>
      </div>
      <p>
        View the camera here. Attach a frame when you want ChatGPT to see it.
      </p>
      {error && (
        <div className="error" role="alert">
          {error}
        </div>
      )}
      <div className="actions">
        <select
          aria-label="Camera"
          disabled={live || !!busy}
          value={camera}
          onChange={(e) => {
            setCamera(e.target.value);
            setFrame(undefined);
          }}
        >
          {cameras.map((c) => (
            <option key={c.id} value={c.id}>
              {c.name}
            </option>
          ))}
        </select>
        {live || busy === "Opening camera" ? (
          <button onClick={stop}>■ Stop preview</button>
        ) : (
          <button
            className="primary"
            disabled={!camera || !!busy}
            onClick={start}
          >
            ▶ Start live preview
          </button>
        )}
        <button disabled={!camera || live || !!busy} onClick={snapshot}>
          Capture frame
        </button>
      </div>
      {frame ? (
        <figure className="camera-frame">
          <img
            className="snapshot"
            src={`data:${frame.mimeType};base64,${frame.data}`}
            alt={
              live
                ? "Live device camera preview"
                : "Captured device camera frame"
            }
          />
          <figcaption>
            {live ? "Live" : "Last frame"}
            {time && ` · ${time}`}
            {frame.meta.width != null &&
              ` · ${frame.meta.width} × ${frame.meta.height}`}
          </figcaption>
        </figure>
      ) : (
        <div className="camera-empty">
          {busy || "Start the preview or capture a frame"}
        </div>
      )}
      {frame && (
        <button disabled={!!busy} onClick={attach}>
          Attach this frame to chat
        </button>
      )}
      <span className="camera-status" role="status">
        {busy}
      </span>
    </section>
  );
}
