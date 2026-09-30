import dgx from "./assets/devices/dgx.webp";
import orin from "./assets/devices/orin.webp";
import thor from "./assets/devices/thor.webp";
import dragonwing from "./assets/devices/dragonwing.webp";
import macbook from "./assets/devices/macbook.webp";
import rpi from "./assets/devices/rpi.webp";
import desktop from "./assets/devices/desktop.webp";
import g1 from "./assets/devices/g1.webp";
import go2 from "./assets/devices/go2.webp";
import { displayModel } from "./device-visual";

const images: Record<string, { src: string; name: string }> = {
  dgx: { src: dgx, name: "NVIDIA DGX Spark" },
  orin: { src: orin, name: "NVIDIA Jetson Orin Nano" },
  thor: { src: thor, name: "NVIDIA Jetson AGX Thor" },
  dragonwing: { src: dragonwing, name: "Qualcomm Dragonwing" },
  macbook: { src: macbook, name: "MacBook" },
  rpi: { src: rpi, name: "Raspberry Pi" },
  desktop: { src: desktop, name: "Desktop computer" },
  g1: { src: g1, name: "Unitree G1" },
  go2: { src: go2, name: "Unitree Go2" },
};

export function SimulatorIcon({ className }: { className?: string }) {
  return (
    <svg
      className={className}
      viewBox="0 0 64 64"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      aria-hidden="true"
    >
      <rect x="8" y="10" width="48" height="34" rx="3" />
      <path d="M24 54h16M32 44v10M24 21l-6 6 6 6M40 21l6 6-6 6M35 19l-6 16" />
    </svg>
  );
}

export function Model({
  id,
  name = "",
  kind,
  large = false,
}: {
  id?: string;
  name?: string;
  kind?: "simulator";
  large?: boolean;
}) {
  const image = images[displayModel(id, name)];
  const simulation = kind === "simulator";
  return (
    <div className={"model " + (large ? "large" : "")}>
      {image ? (
        <img
          className="device-image"
          src={image.src}
          alt={image.name}
          loading="lazy"
          decoding="async"
        />
      ) : simulation ? (
        <SimulatorIcon className="model-fallback simulator-illustration" />
      ) : (
        <svg
          className="model-fallback"
          viewBox="0 0 100 70"
          aria-label="Device illustration"
        >
          <path
            d="m20 20 30-13 30 13v30L50 64 20 50Z M20 20l30 14 30-14M50 34v30"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
          />
          <path d="m27 28 16 8v10l-16-8Z" fill="currentColor" />
        </svg>
      )}
      {simulation && (
        <span className="simulation-label">
          <SimulatorIcon />
          {image?.name.startsWith("Unitree")
            ? "MuJoCo · VM"
            : "Virtual machine"}
        </span>
      )}
    </div>
  );
}
