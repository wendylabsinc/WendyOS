import * as THREE from './vendor/three.module.js';
import { OrbitControls } from './vendor/OrbitControls.js';
import { LidarView } from './lidar-view.js';

// MuJoCo remains the authority: the browser only interpolates world poses.
// Geometry is fetched once per model; polling is bounded to one request at a time.
export class SandboxViewer {
  constructor(canvas, status) {
    this.canvas = canvas;
    this.status = status;
    this.messageText = 'Loading 3D scene…';
    this.active = true;
    this.followRobot = true;
    this.followPosition = new THREE.Vector3();
    this.followDelta = new THREE.Vector3();
    this.hasFollowPosition = false;
    this.samples = [];
    this.bodies = [];
    this.scene = new THREE.Scene();
    this.scene.background = new THREE.Color('#171e22');
    this.scene.fog = new THREE.Fog('#171e22', 18, 65);
    this.camera = new THREE.PerspectiveCamera(48, 16 / 9, 0.015, 100);
    this.camera.up.set(0, 0, 1);
    this.camera.layers.enable(1);
    this.renderer = new THREE.WebGLRenderer({ canvas, antialias: true });
    this.renderer.setPixelRatio(Math.min(devicePixelRatio || 1, 2));
    this.renderer.outputColorSpace = THREE.SRGBColorSpace;
    this.renderer.shadowMap.enabled = true;
    this.renderer.shadowMap.type = THREE.PCFSoftShadowMap;
    this.renderer.shadowMap.autoUpdate = false;
    this.controls = new OrbitControls(this.camera, canvas);
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
    this.lidar = new LidarView(this.scene);
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
    canvas.addEventListener('webglcontextlost', () => {
      this.contextLost = true;
      this.message('3D graphics interrupted. Reload this page to restore the view.');
    });
    this.renderer.setAnimationLoop(now => this.draw(now));
    this.poll();
  }

  setActive(active) {
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
    this.lidar.setEnabled(enabled);
    this.needsRender = true;
  }

  resetView() {
    const robot = this.bodies[this.robotIndex];
    const target = robot ? robot.position.clone() : new THREE.Vector3(0, 0, 0.3);
    // Reset damping as well as pose, so a previous drag cannot move the reset view.
    this.controls.enableDamping = false;
    this.controls.reset();
    this.controls.target.copy(target);
    this.camera.position.copy(target).add(new THREE.Vector3(1.35, -1.6, 0.95));
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
    this.needsFraming = true;
    this.lastPoseKey = null;
    this.hasFollowPosition = false;
    this.needsRender = true;
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
    this.lidar.setStateIdentity(state);
    if (this.samples.length > 5) this.samples.shift();
    this.message(state.valid === false ? 'Physics fault · last valid pose' :
      state.mode === 'paused' ? 'Paused · camera controls available' : 'Live 3D');
    this.canvas.dataset.epoch = String(state.epoch);
  }

  async json(path) {
    const response = await fetch(path, { cache: 'no-store', signal: AbortSignal.timeout(15000) });
    if (!response.ok) throw Error(`Scene unavailable (${response.status})`);
    return response.json();
  }

  async poll() {
    let delay = 100;
    const started = performance.now();
    try {
      if (this.active && !document.hidden && !this.contextLost) {
        if (!this.sceneId) {
          this.message('Loading 3D scene…');
          const model = await this.json('/api/scene');
          this.loadScene(model);
          if (!this.active || document.hidden) return;
        }
        const state = await this.json('/api/scene/state');
        if (!this.active || document.hidden) return;
        if (state.scene_id !== this.sceneId) {
          this.sceneId = null;
          this.samples = [];
        } else {
          this.acceptState(state);
        }
        delay = Math.max(0, 1000 / 30 - (performance.now() - started));
      }
    } catch (error) {
      this.message('Connection interrupted · retrying…');
      if (this.active) this.status.title = error.message;
      delay = 1000;
    } finally {
      this.pollTimer = setTimeout(() => this.poll(), delay);
    }
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
      if (now - samples.at(-1).received > 2000) this.message('Connection interrupted · last received pose');
    }
    if (this.lidar.update(now)) this.needsRender = true;
    // A paused world needs new frames only while navigating. Camera movement
    // doesn't change the shadow map; recompute it only when physics poses move.
    const cameraMoved = this.controls.update();
    if (cameraMoved || this.needsRender) {
      this.renderer.render(this.scene, this.camera);
      this.needsRender = false;
    }
  }
}
