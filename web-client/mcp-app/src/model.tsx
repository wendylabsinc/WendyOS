import { useEffect, useRef, useState } from "react";
import * as THREE from "three";
import { GLTFLoader } from "three/examples/jsm/loaders/GLTFLoader.js";
import { call } from "./bridge";

// One renderer per app, regardless of fleet size. Render only on changes and
// scroll/resize; no idle animation, remote decoder, or GPU context per card.
const entries = new Map<HTMLElement, { model: string; angle: number }>();
const scenes = new Map<string, THREE.Scene>();
const pending = new Set<string>();
let renderer: THREE.WebGLRenderer | undefined,
  frame = 0, generation = 0;

function dispose(scene: THREE.Object3D) {
  scene.traverse(o => {
    if (o instanceof THREE.Mesh) {
      o.geometry.dispose();
      for (const m of Array.isArray(o.material) ? o.material : [o.material]) {
        for (const v of Object.values(m)) if (v instanceof THREE.Texture) v.dispose();
        m.dispose();
      }
    }
  });
}
const camera = new THREE.PerspectiveCamera(35, 1, 0.01, 100);
function schedule() {
  if (!frame) frame = requestAnimationFrame(render);
}
async function load(id: string) {
  if (pending.has(id) || scenes.has(id)) return;
  pending.add(id);
  const started = generation;
  try {
    const r = await call("get_device_model", { model: id }),
      raw = r._meta?.glb;
    if (typeof raw !== "string") throw Error("Model not available");
    const bytes = Uint8Array.from(atob(raw), (c) => c.charCodeAt(0));
    const gltf = await new GLTFLoader().parseAsync(bytes.buffer, "");
    if (started !== generation) { dispose(gltf.scene); return; }
    const box = new THREE.Box3().setFromObject(gltf.scene),
      size = box.getSize(new THREE.Vector3()),
      center = box.getCenter(new THREE.Vector3());
    const root = new THREE.Group();
    root.add(gltf.scene);
    gltf.scene.position.sub(center);
    root.scale.setScalar(2.4 / Math.max(size.x, size.y, size.z));
    const scene = new THREE.Scene();
    scene.add(root, new THREE.HemisphereLight(0xffffff, 0x768381, 3));
    const key = new THREE.DirectionalLight(0xffffff, 4);
    key.position.set(4, 7, 5);
    scene.add(key);
    scenes.set(id, scene);
  } catch {
    for (const [el, e] of entries)
      if (e.model === id) el.dataset.failed = "true";
  } finally {
    schedule();
  }
}
function render() {
  frame = 0;
  if (!renderer || document.hidden) return;
  const w = innerWidth,
    h = innerHeight;
  renderer.setSize(w, h, false);
  renderer.setScissorTest(false);
  renderer.clear();
  renderer.setScissorTest(true);
  for (const [el, e] of entries) {
    const b = el.getBoundingClientRect();
    if (
      b.bottom < 0 ||
      b.top > h ||
      b.right < 0 ||
      b.left > w ||
      !b.width ||
      !b.height
    )
      continue;
    const scene = scenes.get(e.model);
    if (!scene) {
      void load(e.model);
      continue;
    }
    el.dataset.loaded = "true";
    scene.children[0].rotation.y = e.angle;
    camera.aspect = b.width / b.height;
    camera.position.set(3, 2.0, 4.5);
    camera.lookAt(0, 0, 0);
    camera.updateProjectionMatrix();
    renderer.setViewport(b.left, h - b.bottom, b.width, b.height);
    renderer.setScissor(
      Math.max(0, b.left),
      Math.max(0, h - b.bottom),
      Math.min(b.width, w - b.left),
      Math.min(b.height, h - b.top),
    );
    renderer.render(scene, camera);
  }
}
export function ModelLayer() {
  const ref = useRef<HTMLCanvasElement>(null);
  useEffect(() => {
    try {
      renderer = new THREE.WebGLRenderer({
        canvas: ref.current!,
        alpha: true,
        antialias: true,
      });
      renderer.setPixelRatio(Math.min(devicePixelRatio, 1.5));
      renderer.setClearColor(0, 0);
    } catch {
      return;
    }
    const observer = new ResizeObserver(schedule);
    observer.observe(document.body);
    addEventListener("resize", schedule);
    addEventListener("scroll", schedule, true);
    document.addEventListener("visibilitychange", schedule);
    schedule();
    return () => {
      generation++;
      observer.disconnect();
      removeEventListener("resize", schedule);
      removeEventListener("scroll", schedule, true);
      document.removeEventListener("visibilitychange", schedule);
      cancelAnimationFrame(frame);
      frame = 0;
      renderer?.dispose();
      renderer = undefined;
      for (const scene of scenes.values())
        scene.traverse((o) => {
          if (o instanceof THREE.Mesh) {
            o.geometry.dispose();
            for (const m of Array.isArray(o.material)
              ? o.material
              : [o.material]) {
              for (const v of Object.values(m))
                if (v instanceof THREE.Texture) v.dispose();
              m.dispose();
            }
          }
        });
      scenes.clear();
      pending.clear();
    };
  }, []);
  return <canvas ref={ref} className="model-layer" aria-hidden="true" />;
}
export function Model({
  id = "generic",
  large = false,
}: {
  id?: string;
  large?: boolean;
}) {
  const ref = useRef<HTMLDivElement>(null),
    [angle, setAngle] = useState(-0.5);
  useEffect(() => {
    const el = ref.current!;
    if (id === "generic") return;
    entries.set(el, { model: id, angle });
    const o = new ResizeObserver(schedule);
    o.observe(el);
    schedule();
    return () => {
      entries.delete(el);
      o.disconnect();
      schedule();
    };
  }, [id, angle]);
  return (
    <div className={"model " + (large ? "large" : "")}>
      <div ref={ref} className="model-slot">
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
      </div>
      {large && id !== "generic" && (
        <button
          className="rotate"
          onClick={() => setAngle(angle + Math.PI / 4)}
        >
          Rotate model
        </button>
      )}
      <span className="model-label">
        {id === "generic" ? "Model unavailable" : "Display model"}
      </span>
    </div>
  );
}
