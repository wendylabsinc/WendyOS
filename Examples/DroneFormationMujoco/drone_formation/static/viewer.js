import * as THREE from 'three';
import { OrbitControls } from 'three/addons/controls/OrbitControls.js';

// MuJoCo is the authority. The browser interpolates streamed poses, spins the
// propellers from each rotor's streamed thrust, and draws the simulator's wind
// field as moving streaks. Nothing here feeds back into the physics.

const params = new URLSearchParams(location.search);
const DELAY = 0.08; // seconds of buffering behind the newest state
const CREAM = new THREE.Color('#f1eee7');
const STREAK_SECONDS = 0.16;
const BOUNDS = { x: [-4.2, 4.2], y: [-4.2, 4.2], z: [0.2, 3.1] };
const WINDSOCKS = [[-4.3, 3.6], [4.4, 3.2], [3.9, -4.3], [-4.1, -3.9]];

const smoothstep = (a, b, x) => { const t = Math.min(Math.max((x - a) / (b - a), 0), 1); return t * t * (3 - 2 * t); };

class StateBuffer {
  constructor() { this.samples = []; this.clock = null; }
  push(sample) {
    this.samples.push(sample);
    if (this.samples.length > 90) this.samples.shift();
  }
  latest() { return this.samples[this.samples.length - 1]; }
  // A render clock that trails the newest sample by DELAY and absorbs jitter.
  advance(dt) {
    const latest = this.latest();
    if (!latest) return null;
    const goal = latest.elapsed - DELAY;
    if (this.clock === null || Math.abs(goal - this.clock) > 0.5) this.clock = goal;
    const rate = Math.min(Math.max(1 + 0.8 * (goal - this.clock), 0.85), 1.15);
    this.clock += dt * rate;
    return this.clock;
  }
  bracket(t) {
    const s = this.samples;
    if (!s.length) return null;
    if (t <= s[0].elapsed) return [s[0], s[0], 0];
    for (let i = s.length - 1; i > 0; i--) {
      if (s[i - 1].elapsed <= t) {
        const a = s[i - 1], b = s[i];
        if (a.epoch !== b.epoch) return [b, b, 0];
        const span = b.elapsed - a.elapsed;
        return [a, b, span > 0 ? Math.min((t - a.elapsed) / span, 1) : 1];
      }
    }
    return [s[s.length - 1], s[s.length - 1], 0];
  }
}

class WindGrid {
  constructor() { this.grids = []; this.field = null; }
  push(message) {
    const raw = atob(message.data);
    const values = new Float32Array(raw.length);
    for (let i = 0; i < raw.length; i++) {
      const byte = raw.charCodeAt(i);
      values[i] = (byte > 127 ? byte - 256 : byte) * message.scale;
    }
    this.grids.push({ elapsed: message.elapsed, epoch: message.epoch, values, x: message.x, y: message.y, z: message.z });
    if (this.grids.length > 4) this.grids.shift();
  }
  // Blend the two grids around time t into one field for this frame.
  update(t) {
    const g = this.grids;
    if (!g.length) return false;
    let a = g[0], b = g[0], k = 0;
    for (let i = g.length - 1; i > 0; i--) {
      if (g[i - 1].elapsed <= t) { a = g[i - 1]; b = g[i]; k = Math.min(Math.max((t - a.elapsed) / (b.elapsed - a.elapsed || 1), 0), 1); break; }
      a = b = g[i - 1];
    }
    if (a.epoch !== b.epoch) { a = b; k = 0; }
    if (!this.field || this.field.length !== a.values.length) this.field = new Float32Array(a.values.length);
    for (let i = 0; i < this.field.length; i++) this.field[i] = a.values[i] + (b.values[i] - a.values[i]) * k;
    this.shape = { x: a.x, y: a.y, z: a.z };
    return true;
  }
  sample(x, y, z, out) {
    const f = this.field, s = this.shape;
    if (!f) { out[0] = out[1] = out[2] = 0; return out; }
    const nx = s.x[2], ny = s.y[2], nz = s.z.length;
    const fx = Math.min(Math.max((x - s.x[0]) / (s.x[1] - s.x[0]) * (nx - 1), 0), nx - 1.0001);
    const fy = Math.min(Math.max((y - s.y[0]) / (s.y[1] - s.y[0]) * (ny - 1), 0), ny - 1.0001);
    let iz = 0;
    while (iz < nz - 2 && z > s.z[iz + 1]) iz++;
    const fz = Math.min(Math.max((z - s.z[iz]) / (s.z[iz + 1] - s.z[iz]), 0), 1);
    const ix = Math.floor(fx), iy = Math.floor(fy), tx = fx - ix, ty = fy - iy;
    for (let c = 0; c < 3; c++) {
      let v = 0;
      for (let dz = 0; dz < 2; dz++) {
        const wz = dz ? fz : 1 - fz;
        for (let dy = 0; dy < 2; dy++) {
          const wy = dy ? ty : 1 - ty;
          const row = ((iz + dz) * ny + iy + dy) * nx + ix;
          v += wz * wy * ((1 - tx) * f[row * 3 + c] + tx * f[(row + 1) * 3 + c]);
        }
      }
      out[c] = v;
    }
    return out;
  }
}

function propDiscTexture() {
  const canvas = document.createElement('canvas');
  canvas.width = canvas.height = 128;
  const g = canvas.getContext('2d');
  const gradient = g.createRadialGradient(64, 64, 6, 64, 64, 63);
  gradient.addColorStop(0, 'rgba(255,255,255,0.05)');
  gradient.addColorStop(0.55, 'rgba(255,255,255,0.35)');
  gradient.addColorStop(0.9, 'rgba(255,255,255,0.8)');
  gradient.addColorStop(1, 'rgba(255,255,255,0)');
  g.fillStyle = gradient;
  g.fillRect(0, 0, 128, 128);
  const texture = new THREE.CanvasTexture(canvas);
  texture.colorSpace = THREE.SRGBColorSpace;
  return texture;
}

// Screen-space ribbons between two points: wind streaks (tapered) and formation links.
function ribbonMaterial(color, taper) {
  return new THREE.ShaderMaterial({
    uniforms: { resolution: { value: new THREE.Vector2(1, 1) }, width: { value: 2.0 }, color: { value: new THREE.Color(color) }, taper: { value: taper } },
    vertexShader: `
      uniform vec2 resolution; uniform float width;
      attribute vec2 corner; attribute vec3 head; attribute vec3 tail; attribute float alpha;
      varying float vAlpha; varying float vS;
      void main() {
        vec4 h = projectionMatrix * modelViewMatrix * vec4(head, 1.0);
        vec4 t = projectionMatrix * modelViewMatrix * vec4(tail, 1.0);
        // Fade streaks that pass right by the lens or sink into the distance.
        vAlpha = alpha * smoothstep(0.5, 1.8, h.w) * (1.0 - smoothstep(9.0, 17.0, h.w));
        vS = corner.x;
        if (h.w < 0.05 || t.w < 0.05 || vAlpha < 0.002) { gl_Position = vec4(2.0, 2.0, 2.0, 1.0); return; }
        vec2 d = h.xy / h.w * resolution - t.xy / t.w * resolution;
        float len = length(d);
        vec2 dir = len > 1e-4 ? d / len : vec2(1.0, 0.0);
        vec4 p = mix(t, h, corner.x);
        p.xy += vec2(-dir.y, dir.x) * corner.y * width / resolution * p.w;
        gl_Position = p;
      }`,
    fragmentShader: `
      uniform vec3 color; uniform float taper; varying float vAlpha; varying float vS;
      void main() {
        gl_FragColor = vec4(color, vAlpha * mix(1.0, vS, taper));
        #include <colorspace_fragment>
      }`,
    transparent: true,
    depthWrite: false,
    side: THREE.DoubleSide, // the ribbon's winding flips with its screen direction
    blending: THREE.AdditiveBlending,
  });
}

function ribbonGeometry(count) {
  const geometry = new THREE.InstancedBufferGeometry();
  geometry.setAttribute('position', new THREE.Float32BufferAttribute(new Float32Array(12), 3));
  geometry.setAttribute('corner', new THREE.Float32BufferAttribute([0, -1, 0, 1, 1, -1, 1, 1], 2));
  geometry.setIndex([0, 1, 2, 2, 1, 3]);
  for (const [name, size] of [['head', 3], ['tail', 3], ['alpha', 1]]) {
    geometry.setAttribute(name, new THREE.InstancedBufferAttribute(new Float32Array(count * size), size).setUsage(THREE.DynamicDrawUsage));
  }
  geometry.instanceCount = count;
  return geometry;
}

// Thin lines between neighbouring drones that outline the formation. They are
// drawn between the drones' actual positions, so gusts visibly bend them.
class Links {
  constructor(scene, max) {
    this.max = max;
    this.geometry = ribbonGeometry(max);
    this.material = ribbonMaterial('#f1eee7', 0.0);
    this.mesh = new THREE.Mesh(this.geometry, this.material);
    this.mesh.frustumCulled = false;
    this.mesh.renderOrder = 1;
    scene.add(this.mesh);
  }
  update(links, positions, visible) {
    this.mesh.visible = visible && !!links;
    if (!this.mesh.visible) return;
    const head = this.geometry.getAttribute('head'), tail = this.geometry.getAttribute('tail'), alpha = this.geometry.getAttribute('alpha');
    const n = Math.min(links.length, this.max);
    for (let k = 0; k < n; k++) {
      const [a, b, w] = links[k];
      head.array.set(positions[a], k * 3);
      tail.array.set(positions[b], k * 3);
      alpha.array[k] = 0.4 * w;
    }
    this.geometry.instanceCount = n;
    head.needsUpdate = tail.needsUpdate = alpha.needsUpdate = true;
  }
}

class Streaks {
  constructor(scene, count) {
    this.count = count;
    const geometry = ribbonGeometry(count);
    this.head = geometry.getAttribute('head');
    this.tail = geometry.getAttribute('tail');
    this.alpha = geometry.getAttribute('alpha');
    this.material = ribbonMaterial('#dcd9d2', 1.0);
    this.mesh = new THREE.Mesh(geometry, this.material);
    this.mesh.frustumCulled = false;
    this.mesh.renderOrder = 2;
    scene.add(this.mesh);
    this.position = new Float32Array(count * 3);
    this.age = new Float32Array(count);
    this.life = new Float32Array(count);
    for (let i = 0; i < count; i++) this.respawn(i, Math.random());
    this.v = [0, 0, 0];
  }
  respawn(i, ageFraction = 0) {
    const p = this.position;
    p[i * 3] = BOUNDS.x[0] + Math.random() * (BOUNDS.x[1] - BOUNDS.x[0]);
    p[i * 3 + 1] = BOUNDS.y[0] + Math.random() * (BOUNDS.y[1] - BOUNDS.y[0]);
    p[i * 3 + 2] = BOUNDS.z[0] + Math.pow(Math.random(), 1.3) * (BOUNDS.z[1] - BOUNDS.z[0]);
    this.life[i] = 1.4 + Math.random() * 1.8;
    this.age[i] = ageFraction * this.life[i];
  }
  update(wind, dt, visible) {
    this.mesh.visible = visible;
    if (!visible) return;
    const p = this.position, v = this.v, head = this.head.array, tail = this.tail.array, alpha = this.alpha.array;
    for (let i = 0; i < this.count; i++) {
      const o = i * 3;
      wind.sample(p[o], p[o + 1], p[o + 2], v);
      p[o] += v[0] * dt; p[o + 1] += v[1] * dt; p[o + 2] += v[2] * dt;
      this.age[i] += dt;
      if (this.age[i] > this.life[i] || p[o] < BOUNDS.x[0] - 0.3 || p[o] > BOUNDS.x[1] + 0.3 ||
          p[o + 1] < BOUNDS.y[0] - 0.3 || p[o + 1] > BOUNDS.y[1] + 0.3 || p[o + 2] < 0.05 || p[o + 2] > 3.4) {
        this.respawn(i);
      }
      const speed = Math.hypot(v[0], v[1], v[2]);
      const u = this.age[i] / this.life[i];
      const fade = smoothstep(0, 0.2, u) * (1 - smoothstep(0.7, 1, u));
      alpha[i] = 0.7 * fade * smoothstep(0.6, 6.0, speed);
      head[o] = p[o]; head[o + 1] = p[o + 1]; head[o + 2] = p[o + 2];
      tail[o] = p[o] - v[0] * STREAK_SECONDS; tail[o + 1] = p[o + 1] - v[1] * STREAK_SECONDS; tail[o + 2] = p[o + 2] - v[2] * STREAK_SECONDS;
    }
    this.head.needsUpdate = this.tail.needsUpdate = this.alpha.needsUpdate = true;
  }
}

// A soft light under each drone so the formation reads from far away,
// like the LED decks swarms fly with. Purely visual.
class Markers {
  constructor(scene, count) {
    this.geometry = new THREE.BufferGeometry();
    this.geometry.setAttribute('position', new THREE.Float32BufferAttribute(new Float32Array(count * 3), 3));
    this.material = new THREE.ShaderMaterial({
      uniforms: { scale: { value: 500 }, size: { value: 0.03 }, minPx: { value: 14 }, color: { value: new THREE.Color('#fff4e2') } },
      vertexShader: `
        uniform float scale; uniform float size; uniform float minPx; varying float vNear;
        void main() {
          vec4 mv = modelViewMatrix * vec4(position, 1.0);
          gl_Position = projectionMatrix * mv;
          float px = size * scale / max(-mv.z, 0.01);
          gl_PointSize = max(px, minPx);
          vNear = smoothstep(1.2, 4.5, -mv.z);
        }`,
      fragmentShader: `
        uniform vec3 color; varying float vNear;
        void main() {
          float d = length(gl_PointCoord - 0.5) * 2.0;
          float core = exp(-d * d * 10.0);
          float halo = exp(-d * d * 3.0) * 0.45;
          gl_FragColor = vec4(color, (core + halo) * mix(0.35, 1.0, vNear));
          #include <colorspace_fragment>
        }`,
      transparent: true, depthWrite: false, blending: THREE.AdditiveBlending,
    });
    this.points = new THREE.Points(this.geometry, this.material);
    this.points.frustumCulled = false;
    this.points.renderOrder = 3;
    scene.add(this.points);
  }
  update(positions, camera, height, ratio, visible) {
    this.points.visible = visible;
    this.material.uniforms.minPx.value = 14 * ratio;
    const array = this.geometry.getAttribute('position').array;
    positions.forEach((p, i) => { array[i * 3] = p[0]; array[i * 3 + 1] = p[1]; array[i * 3 + 2] = p[2] - 0.014; });
    this.geometry.getAttribute('position').needsUpdate = true;
    this.material.uniforms.scale.value = height / (2 * Math.tan(THREE.MathUtils.degToRad(camera.fov) / 2));
  }
}

class Windsock {
  constructor(scene, x, y) {
    this.x = x; this.y = y; this.wind = new THREE.Vector3(); this.phase = Math.random() * 10;
    const pole = new THREE.Mesh(new THREE.CylinderGeometry(0.022, 0.03, 2.4, 16),
      new THREE.MeshStandardMaterial({ color: '#c9c6bf', roughness: 0.45, metalness: 0.6 }));
    pole.rotation.x = Math.PI / 2;
    pole.position.set(x, y, 1.2);
    pole.castShadow = true;
    scene.add(pole);
    this.pivot = new THREE.Group();
    this.pivot.position.set(x, y, 2.36);
    scene.add(this.pivot);
    const orange = new THREE.MeshStandardMaterial({ color: '#d8663a', roughness: 0.85, side: THREE.DoubleSide });
    const white = new THREE.MeshStandardMaterial({ color: '#efece6', roughness: 0.85, side: THREE.DoubleSide });
    const ring = new THREE.Mesh(new THREE.TorusGeometry(0.15, 0.008, 8, 32),
      new THREE.MeshStandardMaterial({ color: '#c9c6bf', roughness: 0.4, metalness: 0.7 }));
    ring.rotation.y = Math.PI / 2;
    this.pivot.add(ring);
    this.segments = [];
    let parent = this.pivot;
    const count = 5, length = 0.19;
    for (let i = 0; i < count; i++) {
      const r0 = 0.15 - i * 0.017, r1 = 0.15 - (i + 1) * 0.017;
      const geometry = new THREE.CylinderGeometry(r1, r0, length, 24, 1, true);
      geometry.rotateZ(-Math.PI / 2);
      geometry.translate(length / 2, 0, 0);
      const segment = new THREE.Group();
      const mesh = new THREE.Mesh(geometry, i % 2 ? white : orange);
      mesh.castShadow = true;
      segment.add(mesh);
      if (i > 0) segment.position.x = length;
      parent.add(segment);
      this.segments.push(segment);
      parent = segment;
    }
  }
  update(wind, dt, time) {
    const v = wind.sample(this.x, this.y, 2.3, [0, 0, 0]);
    const k = 1 - Math.exp(-dt / 0.35);
    this.wind.x += (v[0] - this.wind.x) * k;
    this.wind.y += (v[1] - this.wind.y) * k;
    const speed = Math.hypot(this.wind.x, this.wind.y);
    const lift = Math.pow(Math.min(speed / 5.5, 1), 0.8);
    if (speed > 0.05) this.yaw = Math.atan2(this.wind.y, this.wind.x);
    const yaw = this.yaw ?? 0;
    this.pivot.rotation.set(0, (1 - lift) * 1.25, yaw, 'ZYX');
    for (let i = 1; i < this.segments.length; i++) {
      const flutter = Math.sin(time * (7 + 5 * lift) + i * 1.3 + this.phase) * 0.07 * lift;
      this.segments[i].rotation.set(0, (1 - lift) * 0.1 + flutter, flutter * 0.8, 'ZYX');
    }
  }
}

export class DroneViewer {
  constructor(canvas, status) {
    this.canvas = canvas;
    this.statusEl = status;
    this.states = new StateBuffer();
    this.wind = new WindGrid();
    this.showHud = params.get('hud') !== '0';
    this.showWind = params.get('wind') !== '0';
    this.showTrails = params.get('trails') === '1';
    this.showTargets = params.get('targets') === '1';
    this.cinematic = params.get('camera') === 'cinematic';
    this.director = null; // optional function(frameInfo) -> {position, target, fov}
    this.renderTime = null;
    this.frameInfo = null;

    this.renderer = new THREE.WebGLRenderer({ canvas, antialias: true, powerPreference: 'high-performance' });
    this.renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
    this.renderer.outputColorSpace = THREE.SRGBColorSpace;
    this.renderer.toneMapping = THREE.ACESFilmicToneMapping;
    this.renderer.toneMappingExposure = 1.05;
    this.renderer.shadowMap.enabled = true;
    this.renderer.shadowMap.type = THREE.PCFSoftShadowMap;

    this.scene = new THREE.Scene();
    this.scene.background = new THREE.Color('#1b2129');
    this.scene.fog = new THREE.Fog('#1b2129', 12, 34);
    this.camera = new THREE.PerspectiveCamera(40, 16 / 9, 0.02, 120);
    this.camera.up.set(0, 0, 1);
    this.camera.position.set(2.4, -4.9, 2.3);
    this.controls = new OrbitControls(this.camera, canvas);
    this.controls.target.set(0, 0, 1.15);
    this.controls.enableDamping = true;
    this.controls.dampingFactor = 0.1;
    this.controls.maxPolarAngle = Math.PI / 2 - 0.02;
    this.controls.minDistance = 0.25;
    this.controls.maxDistance = 30;
    this.controls.update();

    this.buildWorld();
    this.streaks = new Streaks(this.scene, Number(params.get('particles') || 1200));
    this.markers = new Markers(this.scene, 12);
    this.showMarkers = params.get('markers') !== '0';
    this.links = new Links(this.scene, 48);
    this.showLinks = params.get('links') !== '0';
    this.windsocks = WINDSOCKS.map(([x, y]) => new Windsock(this.scene, x, y));
    this.drones = [];
    this.clock = new THREE.Clock();
    this.hudTimer = 0;
    this.q0 = new THREE.Quaternion(); this.q1 = new THREE.Quaternion();
    addEventListener('resize', () => this.resize());
    addEventListener('keydown', (event) => this.key(event));
    this.resize();
    this.applyHud();
  }

  buildWorld() {
    const scene = this.scene;
    const pmrem = new THREE.PMREMGenerator(this.renderer);
    const env = new THREE.Scene();
    env.add(new THREE.Mesh(new THREE.BoxGeometry(20, 20, 20), new THREE.MeshBasicMaterial({ color: '#2b3340', side: THREE.BackSide })));
    for (const [x, y, z, w, h, c] of [[0, 0, 9.5, 10, 10, '#ffffff'], [9.5, -3, 3, 0.1, 6, '#dfe6ee'], [-9.5, 4, 2, 0.1, 5, '#8fa3b8']]) {
      const panel = new THREE.Mesh(new THREE.BoxGeometry(w || 0.1, h, z > 9 ? 0.1 : 4), new THREE.MeshBasicMaterial({ color: c }));
      panel.position.set(x, y, z);
      env.add(panel);
    }
    scene.environment = pmrem.fromScene(env, 0.04).texture;
    scene.environmentIntensity = 0.55;

    const sky = new THREE.Mesh(new THREE.SphereGeometry(80, 32, 16), new THREE.ShaderMaterial({
      side: THREE.BackSide, depthWrite: false, fog: false,
      uniforms: { top: { value: new THREE.Color('#0f1318') }, horizon: { value: new THREE.Color('#1b2129') }, bottom: { value: new THREE.Color('#1b2129') } },
      vertexShader: 'varying vec3 vDir; void main(){ vDir = normalize(position); gl_Position = projectionMatrix * modelViewMatrix * vec4(position,1.0); }',
      fragmentShader: `
        uniform vec3 top; uniform vec3 horizon; uniform vec3 bottom; varying vec3 vDir;
        void main() {
          float z = vDir.z;
          vec3 c = z > 0.0 ? mix(horizon, top, pow(z, 0.55)) : mix(horizon, bottom, pow(-z, 0.4));
          gl_FragColor = vec4(c, 1.0);
          #include <colorspace_fragment>
        }`,
    }));
    sky.rotation.x = 0;
    scene.add(sky);

    // A pool of light under the flight area that falls off into the fog.
    const floorGeometry = new THREE.CircleGeometry(40, 128, 0, Math.PI * 2);
    const floorColors = [];
    const centre = new THREE.Color('#333b46'), edge = new THREE.Color('#1b2129'), c = new THREE.Color();
    const floorPositions = floorGeometry.getAttribute('position');
    for (let i = 0; i < floorPositions.count; i++) {
      const r = Math.hypot(floorPositions.getX(i), floorPositions.getY(i));
      c.copy(centre).lerp(edge, smoothstep(2.5, 14, r));
      floorColors.push(c.r, c.g, c.b);
    }
    floorGeometry.setAttribute('color', new THREE.Float32BufferAttribute(floorColors, 3));
    const floor = new THREE.Mesh(floorGeometry, new THREE.MeshStandardMaterial({ vertexColors: true, roughness: 0.96, metalness: 0 }));
    floor.receiveShadow = true;
    scene.add(floor);

    const lines = [];
    for (let i = -10; i <= 10; i++) { lines.push(i, -10, 0.001, i, 10, 0.001, -10, i, 0.001, 10, i, 0.001); }
    const grid = new THREE.LineSegments(new THREE.BufferGeometry().setAttribute('position', new THREE.Float32BufferAttribute(lines, 3)),
      new THREE.LineBasicMaterial({ color: '#f1eee7', transparent: true, opacity: 0.055, depthWrite: false }));
    scene.add(grid);

    scene.add(new THREE.HemisphereLight('#dde6f0', '#1c2128', 0.85));
    const key = new THREE.DirectionalLight('#fff4e8', 2.7);
    key.position.set(3.5, -5, 8);
    key.castShadow = true;
    key.shadow.mapSize.set(2048, 2048);
    Object.assign(key.shadow.camera, { left: -6, right: 6, top: 6, bottom: -6, near: 1, far: 25 });
    key.shadow.bias = -0.0002;
    key.shadow.normalBias = 0.01;
    scene.add(key);
    const rim = new THREE.DirectionalLight('#bcd4f0', 1.3);
    rim.position.set(-4, 6, 3.5);
    scene.add(rim);
  }

  buildPads(pads) {
    const padMaterial = new THREE.MeshStandardMaterial({ color: '#323a45', roughness: 0.8 });
    const edge = new THREE.LineBasicMaterial({ color: '#f1eee7', transparent: true, opacity: 0.28 });
    const s = 0.12;
    const outline = new THREE.BufferGeometry().setAttribute('position', new THREE.Float32BufferAttribute([-s, -s, 0, s, -s, 0, s, s, 0, -s, s, 0], 3));
    for (const [x, y] of pads) {
      const pad = new THREE.Mesh(new THREE.PlaneGeometry(2 * s, 2 * s), padMaterial);
      pad.position.set(x, y, 0.002);
      pad.receiveShadow = true;
      this.scene.add(pad);
      const line = new THREE.LineLoop(outline, edge);
      line.position.set(x, y, 0.003);
      this.scene.add(line);
    }
  }

  buildDrones(description) {
    const geometries = description.parts.map((part) => {
      const geometry = new THREE.BufferGeometry();
      geometry.setAttribute('position', new THREE.Float32BufferAttribute(part.positions, 3));
      geometry.setAttribute('normal', new THREE.Float32BufferAttribute(part.normals, 3));
      return geometry;
    });
    const color = (rgba) => new THREE.Color().setRGB(rgba[0], rgba[1], rgba[2], THREE.SRGBColorSpace);
    const bodyMaterials = description.parts.map((part) => new THREE.MeshStandardMaterial({
      color: color(part.rgba), roughness: part.metal ? 0.28 : 0.5, metalness: part.metal ? 1 : 0.05,
    }));
    const discTexture = propDiscTexture();
    const discGeometry = new THREE.CircleGeometry(description.propRadius, 40);
    this.rotorSpin = description.rotorSpin;
    this.maxThrust = description.maxThrust;
    for (let d = 0; d < description.drones; d++) {
      const group = new THREE.Group();
      const rotors = [];
      description.parts.forEach((part, index) => {
        if (part.rotor === undefined) {
          const mesh = new THREE.Mesh(geometries[index], bodyMaterials[index]);
          mesh.castShadow = true;
          group.add(mesh);
          return;
        }
        const pivot = new THREE.Group();
        pivot.position.fromArray(part.hub);
        const blades = new THREE.Mesh(geometries[index], new THREE.MeshStandardMaterial({
          color: color(part.rgba), roughness: 0.45, transparent: true, opacity: 1,
        }));
        blades.castShadow = true;
        const disc = new THREE.Mesh(discGeometry, new THREE.MeshBasicMaterial({
          color: color(part.rgba), map: discTexture, transparent: true, opacity: 0, depthWrite: false, side: THREE.DoubleSide,
        }));
        disc.position.z = 0.0131;
        pivot.add(blades, disc);
        group.add(pivot);
        rotors[part.rotor] = { pivot, blades, disc, angle: Math.random() * Math.PI };
      });
      this.scene.add(group);
      const trail = new THREE.Line(new THREE.BufferGeometry(), new THREE.LineBasicMaterial({ color: '#f1eee7', transparent: true, opacity: 0.35 }));
      trail.geometry.setAttribute('position', new THREE.Float32BufferAttribute(new Float32Array(120 * 3), 3));
      trail.frustumCulled = false;
      trail.visible = false;
      this.scene.add(trail);
      const target = new THREE.Mesh(new THREE.RingGeometry(0.055, 0.064, 32),
        new THREE.MeshBasicMaterial({ color: '#f1eee7', transparent: true, opacity: 0.55, side: THREE.DoubleSide, depthWrite: false }));
      target.visible = false;
      this.scene.add(target);
      this.drones.push({ group, rotors, trail, trailCount: 0, target });
    }
  }

  async start() {
    try {
      const response = await fetch('/api/scene');
      if (!response.ok) throw new Error(`scene ${response.status}`);
      const description = await response.json();
      this.buildPads(description.pads);
      this.buildDrones(description);
    } catch (error) {
      this.statusEl.textContent = `Could not load the scene: ${error.message}`;
      return;
    }
    this.connect();
    this.pollStatus();
    this.renderer.setAnimationLoop(() => this.frame());
  }

  connect() {
    const source = new EventSource('/api/stream');
    source.addEventListener('state', (event) => {
      const s = JSON.parse(event.data);
      this.states.push(s);
      if (!this.statusEl.hidden) this.statusEl.hidden = true;
    });
    source.addEventListener('wind', (event) => this.wind.push(JSON.parse(event.data)));
    source.onerror = () => { this.statusEl.hidden = false; this.statusEl.textContent = 'Reconnecting to the simulator…'; };
  }

  async pollStatus() {
    try {
      const status = await (await fetch('/api/status')).json();
      document.querySelector('#link-mujoco').textContent = `MuJoCo ${status.mujocoVersion} · ${status.physicsHz} Hz`;
      document.querySelector('#link-rtf').textContent = `${status.realtimeFactor.toFixed(2)}× real time`;
      this.status = status;
    } catch { /* the stream reports connection problems */ }
    setTimeout(() => this.pollStatus(), 2000);
  }

  key(event) {
    if (event.target !== document.body) return;
    const k = event.key.toLowerCase();
    if (k === 'h') { this.showHud = !this.showHud; this.applyHud(); }
    if (k === 't') this.showTrails = !this.showTrails;
    if (k === 'g') this.showTargets = !this.showTargets;
    if (k === 'w') this.showWind = !this.showWind;
    if (k === 'l') this.showMarkers = !this.showMarkers;
    if (k === 'f') this.showLinks = !this.showLinks;
    if (k === 'c') this.cinematic = !this.cinematic;
  }

  applyHud() {
    for (const el of document.querySelectorAll('.hud')) el.hidden = !this.showHud;
  }

  resize() {
    const w = window.innerWidth, h = window.innerHeight;
    this.renderer.setSize(w, h, false);
    this.camera.aspect = w / h;
    this.camera.updateProjectionMatrix();
    const size = this.renderer.getDrawingBufferSize(new THREE.Vector2());
    this.streaks.material.uniforms.resolution.value.copy(size);
    this.streaks.material.uniforms.width.value = Number(params.get('streak') || 2.2) * this.renderer.getPixelRatio();
    this.links.material.uniforms.resolution.value.copy(size);
    this.links.material.uniforms.width.value = 1.6 * this.renderer.getPixelRatio();
  }

  frame() {
    const dt = Math.min(this.clock.getDelta(), 0.05);
    const t = this.states.advance(dt);
    if (t === null || !this.drones.length) { this.renderer.render(this.scene, this.camera); return; }
    const [a, b, k] = this.states.bracket(t);
    this.renderTime = t;
    const showTime = a.time + (b.epoch === a.epoch ? (b.time - a.time) * k : 0);
    const positions = [];
    for (let d = 0; d < this.drones.length; d++) {
      const drone = this.drones[d];
      const i3 = d * 3, i4 = d * 4;
      const x = a.positions[i3] + (b.positions[i3] - a.positions[i3]) * k;
      const y = a.positions[i3 + 1] + (b.positions[i3 + 1] - a.positions[i3 + 1]) * k;
      const z = a.positions[i3 + 2] + (b.positions[i3 + 2] - a.positions[i3 + 2]) * k;
      drone.group.position.set(x, y, z);
      positions.push([x, y, z]);
      this.q0.set(a.quaternions[i4], a.quaternions[i4 + 1], a.quaternions[i4 + 2], a.quaternions[i4 + 3]);
      this.q1.set(b.quaternions[i4], b.quaternions[i4 + 1], b.quaternions[i4 + 2], b.quaternions[i4 + 3]);
      drone.group.quaternion.slerpQuaternions(this.q0, this.q1, k);
      for (let r = 0; r < 4; r++) {
        const thrust = a.rotors[i4 + r] + (b.rotors[i4 + r] - a.rotors[i4 + r]) * k;
        const rotor = drone.rotors[r];
        const omega = thrust > 1e-4 ? 8 + 70 * Math.sqrt(Math.min(thrust / this.maxThrust, 1)) : 0;
        rotor.angle += this.rotorSpin[r] * -omega * dt;
        rotor.pivot.rotation.z = rotor.angle;
        const blur = smoothstep(12, 45, omega);
        rotor.blades.material.opacity = 1 - 0.78 * blur;
        rotor.blades.material.depthWrite = blur < 0.5;
        rotor.disc.material.opacity = 0.5 * blur;
      }
      this.updateTrail(drone, x, y, z);
      if (b.targets) {
        drone.target.visible = this.showTargets;
        drone.target.position.set(b.targets[i3], b.targets[i3 + 1], b.targets[i3 + 2] - 0.02);
      }
    }
    const hasWind = this.wind.update(t);
    this.streaks.update(this.wind, dt, this.showWind && hasWind);
    for (const sock of this.windsocks) sock.update(this.wind, dt, t);

    this.links.update(b.links, positions, this.showLinks);
    this.markers.update(positions, this.camera, this.renderer.getDrawingBufferSize(new THREE.Vector2()).y, this.renderer.getPixelRatio(), this.showMarkers);
    const centroid = positions.reduce((c, p) => [c[0] + p[0] / positions.length, c[1] + p[1] / positions.length, c[2] + p[2] / positions.length], [0, 0, 0]);
    this.frameInfo = { elapsed: t, time: showTime, dt, positions, centroid, state: b };
    this.updateCamera(dt);
    this.hudTimer -= dt;
    if (this.hudTimer <= 0) { this.hudTimer = 0.12; this.updateHud(b); }
    this.renderer.render(this.scene, this.camera);
  }

  updateTrail(drone, x, y, z) {
    drone.trail.visible = this.showTrails;
    if (!this.showTrails) { drone.trailCount = 0; return; }
    const attribute = drone.trail.geometry.getAttribute('position');
    const array = attribute.array;
    array.copyWithin(3, 0, array.length - 3);
    array[0] = x; array[1] = y; array[2] = z;
    drone.trailCount = Math.min(drone.trailCount + 1, array.length / 3);
    drone.trail.geometry.setDrawRange(0, drone.trailCount);
    attribute.needsUpdate = true;
  }

  updateCamera(dt) {
    if (this.director) {
      const shot = this.director(this.frameInfo);
      if (shot) {
        this.camera.position.fromArray(shot.position);
        if (shot.fov && shot.fov !== this.camera.fov) { this.camera.fov = shot.fov; this.camera.updateProjectionMatrix(); }
        this.camera.lookAt(new THREE.Vector3().fromArray(shot.target));
        return;
      }
    }
    if (this.cinematic) {
      this.orbit = (this.orbit ?? -Math.PI / 2) + dt * 0.06;
      const c = this.frameInfo.centroid;
      this.controls.target.lerp(new THREE.Vector3(c[0], c[1], Math.max(c[2], 1.0)), 1 - Math.exp(-dt * 1.5));
      this.camera.position.set(this.controls.target.x + 7.2 * Math.cos(this.orbit), this.controls.target.y + 7.2 * Math.sin(this.orbit), 2.9);
      this.camera.lookAt(this.controls.target);
      return;
    }
    this.controls.update();
  }

  updateHud(state) {
    if (!this.showHud) return;
    const wind = state.wind;
    const at = wind.atDrones;
    let sum = 0;
    for (let i = 0; i < at.length; i += 3) sum += Math.hypot(at[i], at[i + 1], at[i + 2]);
    document.querySelector('#wind-pattern').textContent = wind.pattern;
    document.querySelector('#wind-speed').textContent = `${(sum / (at.length / 3)).toFixed(1)} m/s at the drones`;
    // Point the arrow the way the air moves, as seen from this camera.
    const heading = THREE.MathUtils.degToRad(wind.heading);
    const forward = new THREE.Vector3();
    this.camera.getWorldDirection(forward);
    forward.z = 0; forward.normalize();
    const right = new THREE.Vector3(forward.y, -forward.x, 0);
    const d = new THREE.Vector3(Math.cos(heading), Math.sin(heading), 0);
    const angle = Math.atan2(d.dot(forward), d.dot(right));
    document.querySelector('#wind-arrow').setAttribute('transform', `rotate(${(-angle * 180 / Math.PI).toFixed(1)})`);
    document.querySelector('#wind-arrow').style.opacity = wind.mean > 0.2 ? 1 : 0.25;
    document.querySelector('#formation-name').textContent = state.formation;
    const error = state.stats.errorMax;
    document.querySelector('#formation-error').textContent = error === null ? (state.formation === 'Landing' ? 'Descending to the pads' : 'On the pads')
      : `Max offset ${(error * 100).toFixed(0)} cm`;
  }
}
