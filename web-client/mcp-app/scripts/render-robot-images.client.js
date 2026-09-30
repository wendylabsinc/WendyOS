import * as THREE from "three";
import { GLTFLoader } from "three/examples/jsm/loaders/GLTFLoader.js";
import { STLLoader } from "three/examples/jsm/loaders/STLLoader.js";

const floats = (text, fallback) => text ? text.trim().split(/\s+/).map(Number) : fallback;

function applyPose(object, element) {
  object.position.fromArray(floats(element.getAttribute("pos"), [0, 0, 0]));
  const [w, x, y, z] = floats(element.getAttribute("quat"), [1, 0, 0, 0]);
  object.quaternion.set(x, y, z, w).normalize();
}

async function loadG1() {
  const xml = await (await fetch("/models/g1.xml")).text();
  const document = new DOMParser().parseFromString(xml, "application/xml");
  const assets = new Map([...document.querySelectorAll("asset > mesh")].map(mesh => [mesh.getAttribute("name"), mesh]));
  const loader = new STLLoader();
  const geometries = new Map();
  for (const [name, asset] of assets) {
    geometries.set(name, loader.loadAsync("/models/g1/meshes/" + asset.getAttribute("file")));
  }
  await Promise.all(geometries.values());
  async function body(element) {
    const group = new THREE.Group();
    group.name = element.getAttribute("name") || "world";
    applyPose(group, element);
    for (const child of element.children) {
      if (child.tagName === "body") group.add(await body(child));
      // Group 1 is the MJCF visual mesh. Omit duplicate collision geometry.
      if (child.tagName !== "geom" || child.getAttribute("type") !== "mesh" || child.getAttribute("group") !== "1") continue;
      const name = child.getAttribute("mesh");
      const rgba = floats(child.getAttribute("rgba"), [0.7, 0.7, 0.7, 1]);
      const mesh = new THREE.Mesh(await geometries.get(name), new THREE.MeshStandardMaterial({
        color: new THREE.Color().setRGB(...rgba.slice(0, 3)),
        roughness: 0.48, metalness: 0.28,
      }));
      mesh.name = name;
      applyPose(mesh, child);
      mesh.scale.fromArray(floats(assets.get(name).getAttribute("scale"), [1, 1, 1]));
      group.add(mesh);
    }
    return group;
  }
  const root = await body(document.querySelector("worldbody"));
  // MuJoCo is Z-up; Three.js and the existing Go2 GLB are Y-up.
  root.rotation.x = -Math.PI / 2;
  return root;
}

async function render() {
  const id = new URL(location.href).searchParams.get("model");
  const robot = id === "g1" ? await loadG1() : (await new GLTFLoader().loadAsync("/models/go2.glb")).scene;
  robot.updateMatrixWorld(true);
  const bounds = new THREE.Box3().setFromObject(robot);
  const center = bounds.getCenter(new THREE.Vector3());
  const dimensions = bounds.getSize(new THREE.Vector3());
  const root = new THREE.Group();
  root.add(robot);
  robot.position.sub(center);
  root.scale.setScalar(2.8 / Math.max(dimensions.x, dimensions.y, dimensions.z));
  const scene = new THREE.Scene();
  scene.add(root, new THREE.HemisphereLight(0xffffff, 0x7b8592, 2.1));
  const key = new THREE.DirectionalLight(0xffffff, 3);
  key.position.set(4, 6, 5);
  scene.add(key);
  const fill = new THREE.DirectionalLight(0xffffff, 1.3);
  fill.position.set(-4, 3, -4);
  scene.add(fill);
  const camera = new THREE.OrthographicCamera(-1.9, 1.9, 1.9, -1.9, 0.1, 100);
  camera.position.set(...(id === "g1" ? [4.5, 1.3, 3] : [4.5, 2.2, 5]));
  camera.lookAt(0, 0, 0);
  const renderer = new THREE.WebGLRenderer({ antialias: true, alpha: true, preserveDrawingBuffer: true });
  renderer.setSize(768, 768);
  renderer.setClearColor(0x000000, 0);
  renderer.outputColorSpace = THREE.SRGBColorSpace;
  renderer.toneMapping = THREE.ACESFilmicToneMapping;
  renderer.toneMappingExposure = 1.05;
  renderer.render(scene, camera);
  const response = await fetch("/result", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({
    id, image: renderer.domElement.toDataURL("image/webp", 0.96), dimensions: dimensions.toArray(),
  }) });
  if (!response.ok) throw new Error("Could not save rendered image");
  renderer.dispose();
}

render().catch(async error => {
  await fetch("/error", { method: "POST", body: String(error.stack || error) });
});
