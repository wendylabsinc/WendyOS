import * as THREE from './vendor/three.module.js';
import { OrbitControls } from './vendor/OrbitControls.js';
import { LidarView } from './lidar-view.js';

// Only blend closely spaced captured poses. A long gap holds the preceding
// pose until the next actual sample, rather than inventing a path between them.
export function posePlayback(samples, now, previousTarget) {
  const target = Math.min(samples.at(-1).received, Math.max(previousTarget ?? samples[0].received, now - 65));
  let before = samples[0], after = samples.at(-1);
  for (const sample of samples) {
    if (sample.received <= target) before = sample;
    if (sample.received >= target) { after = sample; break; }
  }
  const span = after.received - before.received;
  const physicsSpan = (after.time - before.time) * 1000;
  const close = span <= 100 && physicsSpan >= 0 && physicsSpan <= 100;
  const alpha = span > 0 ? (close ? Math.max(0, Math.min(1, (target - before.received) / span)) : 0) : 1;
  return { before, after, alpha, target };
}

// MuJoCo remains the authority: the browser only interpolates world poses.
// Geometry is fetched once per model; polling is bounded to one request at a time.
export class SandboxViewer {
  constructor(canvas, status, options = {}) {
    this.canvas = canvas;
    this.status = status;
    this.fetchJSON = options.request;
    this.frameInterval = options.frameInterval || 1000 / 30;
    this.pollAfterResponse = options.pollAfterResponse === true;
    this.replayHistory = options.replayHistory === true;
    this.lastSequence = 0;
    this.disposed = false;
    this.controller = new AbortController();
    this.viewOffset = options.viewOffset || [1.35, -1.6, 0.95];
    this.messageText = 'Loading 3D scene…';
    this.active = true;
    this.followRobot = true;
    this.followPosition = new THREE.Vector3();
    this.followDelta = new THREE.Vector3();
    this.hasFollowPosition = false;
    this.samples = [];
    this.playbackTarget = undefined;
    this.bodies = [];
    this.scene = new THREE.Scene();
    this.scene.background = new THREE.Color('#171e22');
    this.scene.fog = new THREE.Fog('#171e22', 18, 65);
    this.camera = new THREE.PerspectiveCamera(48, 16 / 9, 0.015, 100);
    this.camera.up.set(0, 0, 1);
    this.camera.layers.enable(1);
    // Retry without multisampling if the browser cannot allocate it. Use the
    // actual canvas, rather than allocating a second context as a support probe.
    const context = canvas.getContext('webgl2', { antialias: true }) ||
      canvas.getContext('webgl2', { antialias: false });
    if (!context) {
      const error = Error('The browser could not create a WebGL 2 graphics context.');
      error.name = 'WebGLUnavailableError';
      throw error;
    }
    this.renderer = new THREE.WebGLRenderer({ canvas, context });
    this.renderer.setPixelRatio(Math.min(devicePixelRatio || 1, 2));
    this.renderer.outputColorSpace = THREE.SRGBColorSpace;
    this.renderer.shadowMap.enabled = true;
    this.renderer.shadowMap.type = THREE.PCFSoftShadowMap;
    this.renderer.shadowMap.autoUpdate = false;
    this.controls = new OrbitControls(this.camera, canvas);
    this.controls.enableZoom = options.wheelZoom !== false;
    this.controls.addEventListener('change', () => { this.needsRender = true; });
    this.controls.enableDamping = true;
    this.controls.dampingFactor = 0.12;
    this.controls.minDistance = 0.3;
    this.controls.maxDistance = 28;
    this.controls.maxPolarAngle = Math.PI / 2 - 0.015;
    this.controls.screenSpacePanning = false;
    this.controls.target.set(0, 0, 0.3);
    this.camera.position.set(1.35, -1.6, 1.25);
    this.controls.update();

    const sky = new THREE.HemisphereLight(0xe6f1ff, 0x4b5042, 2.2);
    sky.position.set(0, 0, 10);
    this.scene.add(sky);
    const sun = new THREE.DirectionalLight(0xfff1df, 3);
    sun.position.set(2, -3, 8);
    sun.castShadow = true;
    sun.shadow.mapSize.set(1024, 1024);
    Object.assign(sun.shadow.camera, { left: -7, right: 7, top: 7, bottom: -7, near: 0.5, far: 22 });
    sun.shadow.normalBias = 0.015;
    this.scene.add(sun);
    this.world = new THREE.Group();
    this.scene.add(this.world);
    const grid = new THREE.GridHelper(12, 24, 0x7a8c96, 0x53646e);
    grid.rotation.x = Math.PI / 2;
    grid.position.z = 0.002;
    this.scene.add(grid);
    this.lidar = new LidarView(this.scene, options.request);
    this.lidarAvailable = options.lidar !== false;
    this.lidar.setEnabled(this.lidarAvailable);
    this.nextPosition = new THREE.Vector3();
    this.nextQuaternion = new THREE.Quaternion();

    this.resize = new ResizeObserver(() => {
      const { width, height } = canvas.getBoundingClientRect();
      if (!width || !height) return;
      this.renderer.setSize(width, height, false);
      this.camera.aspect = width / height;
      this.camera.updateProjectionMatrix();
      this.needsRender = true;
    });
    this.resize.observe(canvas);
    this.contextLostListener = event => {
      event.preventDefault();
      this.contextLost = true;
      this.samples = [];
      this.message('3D graphics interrupted · waiting for recovery…');
    };
    this.contextRestoredListener = () => {
      this.contextLost = false;
      this.samples = [];
      this.replayTime = this.playbackTarget = this.lastDrawTime = undefined;
      this.replayDebt = 0;
      this.needsRender = true;
      this.renderer.shadowMap.needsUpdate = true;
      this.message('Reconnecting to 3D scene…');
    };
    canvas.addEventListener('webglcontextlost', this.contextLostListener);
    canvas.addEventListener('webglcontextrestored', this.contextRestoredListener);
    this.renderer.setAnimationLoop(now => this.draw(now));
    this.poll();
  }

  setActive(active) {
    if (!active && this.replayHistory) {
      this.samples = [];
      this.replayTime = this.playbackTarget = this.lastDrawTime = undefined;
      this.replayDebt = 0;
    }
    this.active = active;
    this.controls.enabled = active;
    this.lidar.setActive(active);
    this.needsRender = true;
    if (active) this.status.textContent = this.messageText;
  }

  message(text) {
    this.messageText = text;
    if (this.active) this.status.textContent = text;
  }

  setFollowRobot(enabled) {
    this.followRobot = enabled;
    const robot = this.bodies[this.robotIndex];
    if (enabled && robot) {
      // Recenter without changing the orbit angle or zoom. Further manual pan
      // is kept as an offset while the camera translates with the robot.
      this.followDelta.copy(robot.position).sub(this.controls.target);
      this.camera.position.add(this.followDelta);
      this.controls.target.copy(robot.position);
      this.followPosition.copy(robot.position);
      this.hasFollowPosition = true;
      this.controls.update();
      this.needsRender = true;
    }
  }

  setLidarEnabled(enabled) {
    this.lidar.setEnabled(this.lidarAvailable && enabled);
    this.needsRender = true;
  }

  resetView() {
    const robot = this.bodies[this.robotIndex];
    const target = robot ? robot.position.clone() : new THREE.Vector3(0, 0, 0.3);
    // Reset damping as well as pose, so a previous drag cannot move the reset view.
    this.controls.enableDamping = false;
    this.controls.reset();
    this.controls.target.copy(target);
    this.camera.position.copy(target).add(new THREE.Vector3(...this.viewOffset));
    this.controls.update();
    this.controls.enableDamping = true;
    this.needsRender = true;
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
      if (geom.type === 'plane') material.color.set('#27353e');
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
    this.playbackTarget = undefined;
    this.replayTime = this.lastDrawTime = undefined;
    this.replayDebt = 0;
    this.needsFraming = true;
    this.lastPoseKey = null;
    this.hasFollowPosition = false;
    this.needsRender = true;
  }

  acceptState(state, received = performance.now()) {
    if (this.contextLost) return;
    const count = this.bodies.length;
    if (state.positions.length !== count * 3 || state.quaternions.length !== count * 4 ||
        !Number.isFinite(state.time) || !state.positions.every(Number.isFinite) || !state.quaternions.every(Number.isFinite)) {
      throw Error('Invalid scene pose received');
    }
    const previous = this.samples.at(-1);
    // Reset, obstacle edits and reconnects must never blend between worlds.
    if (previous && (state.epoch !== previous.epoch || state.generation !== previous.generation ||
        state.time < previous.time || received - previous.received > 2000)) {
      this.samples = [];
      this.playbackTarget = undefined;
      this.replayTime = undefined;
      this.replayDebt = 0;
    }
    this.samples.push({ ...state, received });
    this.lastPoseArrival = performance.now();
    this.lidar.setStateIdentity(state);
    if (this.samples.length > (this.replayHistory ? 180 : 5)) {
      this.samples.shift();
      this.replayTime = Math.max(this.replayTime ?? received, this.samples[0].received);
      if (this.replayHistory) this.playbackOverflow = true;
    }
    this.message(state.valid === false ? 'Physics fault · last valid pose' :
      state.mode === 'paused' ? 'Paused · camera controls available' : 'Live 3D');
    this.canvas.dataset.epoch = String(state.epoch);
  }

  acceptBatch(packet) {
    if (!Array.isArray(packet.frames) || packet.frames.length === 0 || packet.frames.length > 90) throw Error('Invalid pose history received');
    this.playbackOverflow = false;
    if (packet.dropped) {
      this.samples = [];
      this.replayTime = this.playbackTarget = undefined;
      this.replayDebt = 0;
    }
    for (const frame of packet.frames) {
      if (!Number.isSafeInteger(frame.sequence) || frame.sequence < 1 || !Number.isFinite(frame.captured_ms) || !frame.state) throw Error('Invalid captured pose received');
      if (frame.sequence <= this.lastSequence) continue;
      if (frame.state.scene_id !== this.sceneId) {
        this.sceneId = null;
        this.samples = [];
        return;
      }
      const previous = this.samples.at(-1);
      if (previous && frame.captured_ms <= previous.received) throw Error('Pose capture timestamps are not ordered');
      this.acceptState(frame.state, frame.captured_ms);
      this.lastSequence = frame.sequence;
      this.canvas.dataset.poseSequence = String(frame.sequence);
    }
    if (packet.dropped || this.playbackOverflow) this.message('Live 3D · some movement history unavailable');
  }

  async json(path) {
    if (this.fetchJSON) return this.fetchJSON(path);
    const response = await fetch(path, { cache: 'no-store', signal: AbortSignal.any([this.controller.signal, AbortSignal.timeout(15000)]) });
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
          this.message('Loading 3D scene…');
          const model = await this.json('/api/scene');
          if (this.disposed) return;
          this.loadScene(model);
          if (!this.active || document.hidden) return;
        }
        const state = await this.json('/api/scene/state');
        if (this.disposed || !this.active || document.hidden || this.contextLost) return;
        if (this.replayHistory && Array.isArray(state.frames)) {
          this.acceptBatch(state);
        } else if (state.scene_id !== this.sceneId) {
          this.sceneId = null;
          this.samples = [];
        } else {
          this.acceptState(state);
        }
        // Host tool calls may wait in a background queue. Space reads after
        // completion so variable queue delays cannot produce a burst of poses.
        delay = this.pollAfterResponse ? this.frameInterval : Math.max(0, this.frameInterval - (performance.now() - started));
      }
    } catch (error) {
      if (this.disposed) return;
      if (!this.contextLost) this.message('Connection interrupted · retrying…');
      if (this.active) this.status.title = error.message;
      delay = 1000;
    } finally {
      if (!this.disposed) this.pollTimer = setTimeout(() => this.poll(), delay);
    }
  }

  draw(now) {
    if (this.disposed || !this.active || document.hidden || this.contextLost) return;
    const samples = this.samples;
    if (samples.length) {
      let clock = now;
      if (this.replayHistory) {
        // Advance through every captured movement at its original cadence.
        // Waiting for a batch freezes this clock, so late poses aren't skipped.
        const previousTime = this.replayTime ?? samples[0].received;
        const next = samples.find(sample => sample.received > previousTime);
        this.replayDebt = Math.min(1000, (this.replayDebt ?? 0) + (this.lastDrawTime === undefined ? 0 : Math.max(0, now - this.lastDrawTime)));
        if (this.replayTime === undefined) {
          this.replayTime = samples[0].received;
          this.replayDebt = 0;
        } else if (next) {
          // Show each capture at least once even if a render frame is slow.
          this.replayTime = Math.min(previousTime + this.replayDebt, next.received);
          this.replayDebt -= this.replayTime - previousTime;
        } else {
          this.replayTime = samples.at(-1).received;
          this.replayDebt = 0;
        }
        clock = this.replayTime + 65;
      }
      const { before, after, alpha, target } = posePlayback(samples, clock, this.playbackTarget);
      this.playbackTarget = target;
      while (this.replayHistory && samples.length > 2 && samples[1].received <= target) samples.shift();
      const poseKey = `${after.epoch}:${after.generation}:${before.time + (after.time - before.time) * alpha}`;
      if (poseKey !== this.lastPoseKey) {
        this.needsRender = true;
        this.renderer.shadowMap.needsUpdate = true;
        this.lastPoseKey = poseKey;
      }
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
      const robot = this.bodies[this.robotIndex];
      if (robot) {
        if (this.followRobot && this.hasFollowPosition) {
          this.followDelta.copy(robot.position).sub(this.followPosition);
          this.camera.position.add(this.followDelta);
          this.controls.target.add(this.followDelta);
        }
        this.followPosition.copy(robot.position);
        this.hasFollowPosition = true;
      }
      if (now - this.lastPoseArrival > 2000) this.message('Connection interrupted · last received pose');
    }
    this.lastDrawTime = now;
    if (this.lidar.update(now)) this.needsRender = true;
    // A paused world needs new frames only while navigating. Camera movement
    // doesn't change the shadow map; recompute it only when physics poses move.
    const cameraMoved = this.controls.update();
    if (cameraMoved || this.needsRender) {
      this.renderer.render(this.scene, this.camera);
      this.needsRender = false;
    }
  }

  zoom(scale) {
    const offset = this.camera.position.clone().sub(this.controls.target);
    const distance = THREE.MathUtils.clamp(offset.length() * scale, this.controls.minDistance, this.controls.maxDistance);
    this.camera.position.copy(this.controls.target).add(offset.setLength(distance));
    this.controls.update();
    this.needsRender = true;
  }

  dispose() {
    this.disposed = true;
    this.active = false;
    clearTimeout(this.pollTimer);
    this.controller.abort();
    this.renderer.setAnimationLoop(null);
    this.resize.disconnect();
    this.canvas.removeEventListener('webglcontextlost', this.contextLostListener);
    this.canvas.removeEventListener('webglcontextrestored', this.contextRestoredListener);
    this.controls.dispose();
    this.lidar.dispose();
    const geometries = new Set(), materials = new Set();
    this.scene.traverse(object => {
      if (object.geometry) geometries.add(object.geometry);
      if (object.material) for (const material of [object.material].flat()) materials.add(material);
      object.shadow?.map?.dispose();
    });
    geometries.forEach(geometry => geometry.dispose());
    materials.forEach(material => material.dispose());
    this.renderer.dispose();
  }
}
