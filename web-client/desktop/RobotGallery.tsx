import { useEffect, useRef } from "react";
import { ArrowUpRight, Circle } from "lucide-react";
import { SandboxViewer } from "../../go/simulator/go2/go2_sim/viewer.js";
import type { DesktopAPI, Simulator } from "./types";

export default function RobotGallery({
  api,
  robots,
  onSelect,
}: {
  api: DesktopAPI;
  robots: Simulator[];
  onSelect: (id: string) => void;
}) {
  return (
    <div className="desktop-robot-gallery" aria-label="Online robots">
      {robots.map((robot) => (
        <RobotPreview
          key={robot.id}
          api={api}
          robot={robot}
          onSelect={onSelect}
        />
      ))}
    </div>
  );
}

function RobotPreview({
  api,
  robot,
  onSelect,
}: {
  api: DesktopAPI;
  robot: Simulator;
  onSelect: (id: string) => void;
}) {
  const card = useRef<HTMLButtonElement>(null);
  const canvas = useRef<HTMLCanvasElement>(null);
  const status = useRef<HTMLSpanElement>(null);
  useEffect(() => {
    if (!canvas.current || !status.current || !card.current) return;
    try {
      const viewer = new SandboxViewer(canvas.current, status.current, {
        request: (path: string) => api.simulatorRequest({ id: robot.id, path }),
        wheelZoom: false,
        lidar: false,
        frameInterval: 100,
        viewOffset: robot.kind === "g1" ? [2.4, -2.7, 1.5] : [1.35, -1.6, 0.95],
      });
      const observer = new IntersectionObserver(([entry]) =>
        viewer.setActive(entry.isIntersecting),
      );
      observer.observe(card.current);
      return () => {
        observer.disconnect();
        viewer.dispose();
      };
    } catch {
      status.current.textContent = "Open robot controls";
    }
  }, [api, robot.id, robot.kind]);
  return (
    <button
      ref={card}
      className="desktop-robot-preview"
      onClick={() => onSelect(robot.id)}
      aria-label={`Open ${robot.name} controls`}
    >
      <span className="desktop-robot-preview-scene">
        <canvas ref={canvas} aria-hidden="true" />
        <span ref={status} className="desktop-robot-view-status">
          Connecting to live view…
        </span>
        <span className="desktop-robot-preview-kind">
          {robot.kind === "g1" ? "Unitree G1" : "Unitree Go2"}
        </span>
      </span>
      <span className="desktop-robot-preview-caption">
        <span>
          <strong>{robot.name}</strong>
          <span className="desktop-muted">
            {robot.runtime === "docker" ? "Docker" : "Apple Container"}
          </span>
        </span>
        <span className="desktop-connected">
          <Circle size={7} fill="currentColor" />
          {robot.mode || "Online"}
          <ArrowUpRight size={17} />
        </span>
      </span>
    </button>
  );
}
