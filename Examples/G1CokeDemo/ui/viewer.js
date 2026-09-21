import * as THREE from './vendor/three.module.js';
import { OrbitControls } from './vendor/OrbitControls.js';

// MuJoCo remains the authority: the browser only interpolates world poses.
// Geometry is fetched once per model; polling is bounded to one request at a time.
export class SandboxViewer {
  constructor(canvas, status) {
    this.canvas = canvas;
    this.status = status;
    this.active = true;
    this.disposed = false;
    this.samples = [];
    this.bodies = [];
    this.scene = new THREE.Scene();
    this.scene.background = new THREE.Color('#eeeae3');
    this.scene.fog = new THREE.Fog('#eeeae3', 12, 35);
    this.camera = new THREE.PerspectiveCamera(48, 16 / 9, 0.015, 100);
    this.camera.up.set(0, 0, 1);
    this.renderer = new THREE.WebGLRenderer({ canvas, antialias: true });
    this.renderer.setPixelRatio(Math.min(devicePixelRatio || 1, 2));
    this.renderer.outputColorSpace = THREE.SRGBColorSpace;
    this.renderer.shadowMap.enabled = true;
    this.renderer.shadowMap.type = THREE.PCFSoftShadowMap;
    this.controls = new OrbitControls(this.camera, canvas);
    this.controls.enableDamping = true;
    this.controls.dampingFactor = 0.12;
    this.controls.minDistance = 0.3;
    this.controls.maxDistance = 28;
    this.controls.maxPolarAngle = Math.PI / 2 - 0.015;
    this.controls.screenSpacePanning = false;
    this.controls.target.set(0, 0, 0.9);
    this.camera.position.set(2, -2.6, 1.95);
    this.controls.update();

    const sky = new THREE.HemisphereLight(0xe6f1ff, 0x4b5042, 2.2);
    sky.position.set(0, 0, 10);
    this.scene.add(sky);
    const sun = new THREE.DirectionalLight(0xfff1df, 3);
    sun.position.set(2, -3, 8);
    sun.castShadow = true;
    sun.shadow.mapSize.set(2048, 2048);
    Object.assign(sun.shadow.camera, { left: -7, right: 7, top: 7, bottom: -7, near: 0.5, far: 22 });
    sun.shadow.normalBias = 0.015;
    this.scene.add(sun);
    this.world = new THREE.Group();
    this.scene.add(this.world);
    const grid = new THREE.GridHelper(12, 24, 0xb1aaa0, 0xd6d0c6);
    grid.rotation.x = Math.PI / 2;
    grid.position.z = 0.002;
    this.scene.add(grid);
    this.nextPosition = new THREE.Vector3();
    this.nextQuaternion = new THREE.Quaternion();

    this.resize = new ResizeObserver(() => {
      const { width, height } = canvas.getBoundingClientRect();
      if (!width || !height) return;
      this.renderer.setSize(width, height, false);
      this.camera.aspect = width / height;
      this.camera.updateProjectionMatrix();
    });
    this.resize.observe(canvas);
    canvas.addEventListener('webglcontextlost', () => {
      this.contextLost = true;
      status.textContent = '3D graphics interrupted. Reload this page to restore the view.';
    });
    this.renderer.setAnimationLoop(now => this.draw(now));
    this.poll();
  }

  setActive(active) {
    this.active = active;
    this.controls.enabled = active;
  }

  resetView() {
    const target = new THREE.Vector3(0, 0, 0.9);
    // Reset damping as well as pose, so a previous drag cannot move the reset view.
    this.controls.reset();
    this.controls.target.copy(target);
    this.camera.position.copy(target).add(new THREE.Vector3(1.7, -2.1, 1.15));
    this.controls.update();
  }

  geometry(geom, meshes) {
    const [x, y, z] = geom.size;
    switch (geom.type) {
      case 'mesh': {
        const mesh = meshes.get(geom.mesh);
        if (!mesh) throw Error('Scene references a missing mesh');
        return mesh;
      }
      case 'plane': return new THREE.PlaneGeometry(x ? x * 2 : 80, y ? y * 2 : 80);
      case 'box': return new THREE.BoxGeometry(x * 2, y * 2, z * 2);
      case 'sphere': return new THREE.SphereGeometry(x, 24, 16);
      case 'ellipsoid': return new THREE.SphereGeometry(1, 24, 16).scale(x, y, z);
      // MuJoCo primitives extend along Z; Three.js cylinder/capsule axes are Y.
      case 'cylinder': return new THREE.CylinderGeometry(x, x, y * 2, 24).rotateX(Math.PI / 2);
      case 'capsule': return new THREE.CapsuleGeometry(x, y * 2, 6, 16).rotateX(Math.PI / 2);
      default: throw Error(`Unsupported scene geometry: ${geom.type}`);
    }
  }

  loadScene(model) {
    if (model.version !== 1) throw Error('This scene requires a newer viewer. Reload the page.');
    const meshes = new Map();
    for (const mesh of model.meshes) {
      const geometry = new THREE.BufferGeometry();
      geometry.setAttribute('position', new THREE.Float32BufferAttribute(mesh.positions, 3));
      geometry.setIndex(mesh.indices);
      geometry.computeVertexNormals();
      meshes.set(mesh.id, geometry);
    }
    const world = new THREE.Group();
    const byId = new Map();
    const bodies = model.bodies.map(body => {
      const group = new THREE.Group();
      group.name = body.name;
      byId.set(body.id, group);
      world.add(group);
      return group;
    });
    for (const geom of model.geoms) {
      const material = new THREE.MeshStandardMaterial({
        color: new THREE.Color().setRGB(...geom.rgba.slice(0, 3)),
        roughness: 0.72, metalness: 0.08,
        opacity: geom.rgba[3], transparent: geom.rgba[3] < 1,
        side: geom.type === 'plane' ? THREE.DoubleSide : THREE.FrontSide,
      });
      if (geom.name === 'floor') material.color.set('#ded8cd');
      const mesh = new THREE.Mesh(this.geometry(geom, meshes), material);
      mesh.name = geom.name || geom.type;
      mesh.position.fromArray(geom.position);
      mesh.quaternion.fromArray(geom.quaternion);
      mesh.castShadow = geom.type !== 'plane';
      mesh.receiveShadow = true;
      byId.get(geom.body).add(mesh);
    }
    const oldGeometries = new Set();
    this.world.traverse(object => {
      if (object.geometry) oldGeometries.add(object.geometry);
      object.material?.dispose();
    });
    oldGeometries.forEach(geometry => geometry.dispose());
    this.scene.remove(this.world);
    this.world = world;
    this.bodies = bodies;
    this.robotIndex = model.bodies.findIndex(body => body.id === model.robot_body);
    this.scene.add(world);
    this.sceneId = model.id;
    this.samples = [];
    this.needsFraming = true;
  }

  acceptState(state) {
    const count = this.bodies.length;
    if (state.positions.length !== count * 3 || state.quaternions.length !== count * 4 ||
        !state.positions.every(Number.isFinite) || !state.quaternions.every(Number.isFinite)) {
      throw Error('Invalid scene pose received');
    }
    const previous = this.samples.at(-1);
    // Reset, obstacle edits and reconnects must never blend between worlds.
    if (previous && (state.epoch !== previous.epoch || state.generation !== previous.generation ||
        state.time < previous.time || performance.now() - previous.received > 500)) {
      this.samples = [];
    }
    this.samples.push({ ...state, received: performance.now() });
    if (this.samples.length > 5) this.samples.shift();
    this.status.textContent = state.valid === false ? 'Physics fault · last valid pose' :
      state.mode === 'paused' ? 'Paused · camera controls available' : 'Live 3D';
    this.canvas.dataset.epoch = String(state.epoch);
  }

  async json(path) {
    const response = await fetch(path, { cache: 'no-store', signal: AbortSignal.timeout(15000) });
    if (!response.ok) throw Error(`Scene unavailable (${response.status})`);
    return response.json();
  }

  async poll() {
    if (this.disposed) return;
    let delay = 100;
    const started = performance.now();
    try {
      if (this.active && !document.hidden && !this.contextLost) {
        if (!this.sceneId) {
          this.status.textContent = 'Loading 3D scene…';
          this.loadScene(await this.json('/api/scene'));
        }
        const state = await this.json('/api/scene/state');
        if (state.scene_id !== this.sceneId) {
          this.sceneId = null;
          this.samples = [];
        } else {
          this.acceptState(state);
        }
        delay = Math.max(0, 1000 / 30 - (performance.now() - started));
      }
    } catch (error) {
      this.status.textContent = 'Connection interrupted · retrying…';
      this.status.title = error.message;
      delay = 1000;
    }
    if (!this.disposed) this.pollTimer = setTimeout(() => this.poll(), delay);
  }

  draw(now) {
    if (!this.active || document.hidden || this.contextLost) return;
    const samples = this.samples;
    if (samples.length) {
      const target = now - 65;
      let before = samples[0], after = samples.at(-1);
      for (const sample of samples) {
        if (sample.received <= target) before = sample;
        if (sample.received >= target) { after = sample; break; }
      }
      const span = after.received - before.received;
      const alpha = span > 0 ? THREE.MathUtils.clamp((target - before.received) / span, 0, 1) : 1;
      this.bodies.forEach((body, index) => {
        body.position.fromArray(before.positions, index * 3);
        this.nextPosition.fromArray(after.positions, index * 3);
        body.position.lerp(this.nextPosition, alpha);
        body.quaternion.fromArray(before.quaternions, index * 4);
        this.nextQuaternion.fromArray(after.quaternions, index * 4);
        body.quaternion.slerp(this.nextQuaternion, alpha);
      });
      if (this.needsFraming) {
        this.resetView();
        this.needsFraming = false;
      }
      if (now - samples.at(-1).received > 2000) this.status.textContent = 'Connection interrupted · last received pose';
    }
    this.controls.update();
    this.renderer.render(this.scene, this.camera);
  }

  dispose() {
    this.disposed = true;
    this.active = false;
    clearTimeout(this.pollTimer);
    this.renderer.setAnimationLoop(null);
    this.resize.disconnect();
    this.controls.dispose();
    const geometries = new Set();
    this.scene.traverse(object => {
      if (object.geometry) geometries.add(object.geometry);
      object.material?.dispose();
    });
    geometries.forEach(geometry => geometry.dispose());
    this.renderer.dispose();
  }
}

// Mount into a sized container, or pass an existing canvas. The returned
// viewer offers resetView(), setActive(boolean) and dispose().
export function mountViewer(element) {
  const canvas = element instanceof HTMLCanvasElement ? element : document.createElement('canvas');
  const status = document.createElement('span');
  status.className = 'scene-viewer-status';
  status.setAttribute('role', 'status');
  status.style.cssText = 'position:absolute;left:14px;bottom:14px;padding:5px 9px;border-radius:6px;background:rgba(255,255,255,.82);color:#555;font:12px system-ui;pointer-events:none';
  status.textContent = 'Loading 3D scene';
  if (canvas !== element) {
    if (getComputedStyle(element).position === 'static') element.style.position = 'relative';
    canvas.style.cssText = 'display:block;width:100%;height:100%;touch-action:none';
    element.append(canvas, status);
  } else {
    element.parentElement?.append(status);
  }
  try {
    return new SandboxViewer(canvas, status);
  } catch (error) {
    status.textContent = '3D view unavailable: ' + error.message;
    throw error;
  }
}
