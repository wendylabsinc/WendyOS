import * as THREE from 'three';
import { OrbitControls } from 'three/addons/controls/OrbitControls.js';
import { toCreasedNormals } from 'three/addons/utils/BufferGeometryUtils.js';

// MuJoCo is the authority. The browser draws the bodies it streams (the G1's links and
// the boxes), the racks and cart from the scene description, and what the robot's
// turbopuffer memory just answered. Nothing here feeds back into the physics.

const params = new URLSearchParams(location.search);
const DELAY = 0.08; // seconds of buffering behind the newest state
const FONT = 'Geist, Inter, -apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif';
const CREAM = '#f1eee7';
const MATCH_STATES = new Set(['Asking turbopuffer', 'Shelving', 'Question', 'Fetching']);

const smoothstep = (a, b, x) => { const t = Math.min(Math.max((x - a) / (b - a), 0), 1); return t * t * (3 - 2 * t); };
const text = (selector, value) => { const el = document.querySelector(selector); if (el.textContent !== value) el.textContent = value; };

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

function canvasTexture(width, height, draw) {
  const canvas = document.createElement('canvas');
  canvas.width = width; canvas.height = height;
  draw(canvas.getContext('2d'), width, height);
  const texture = new THREE.CanvasTexture(canvas);
  texture.colorSpace = THREE.SRGBColorSpace;
  texture.anisotropy = 8;
  return texture;
}

function wrap(g, words, width) {
  const lines = [];
  let line = '';
  for (const word of words.split(' ')) {
    const next = line ? `${line} ${word}` : word;
    if (g.measureText(next).width > width && line) { lines.push(line); line = word; } else line = next;
  }
  if (line) lines.push(line);
  return lines;
}

function kraft(g, w, h, color) {
  g.fillStyle = color;
  g.fillRect(0, 0, w, h);
  // faint corrugation and speckle so the cardboard does not read as flat plastic
  for (let x = 0; x < w; x += 6) { g.fillStyle = `rgba(0,0,0,${0.025 + 0.02 * Math.sin(x * 0.7)})`; g.fillRect(x, 0, 3, h); }
  for (let i = 0; i < 500; i++) { g.fillStyle = `rgba(60,40,20,${Math.random() * 0.06})`; g.fillRect(Math.random() * w, Math.random() * h, 2, 2); }
}

// The end of a box: kraft cardboard with a shipping label naming what is inside.
function labelTexture(label, color) {
  return canvasTexture(512, 366, (g, w, h) => {
    kraft(g, w, h, color);
    g.fillStyle = '#f4f1ea';
    g.fillRect(34, 58, w - 68, h - 116);
    g.fillStyle = '#1c2128';
    g.font = `500 46px ${FONT}`;
    const lines = wrap(g, label, w - 120).slice(0, 3);
    lines.forEach((line, i) => g.fillText(line, 60, 118 + i * 54));
    for (let x = 60, k = 0; x < 300; k++) { const bar = 2 + ((k * 7) % 5); g.fillRect(x, h - 104, bar, 34); x += bar + 3 + ((k * 3) % 4); }
  });
}

function topTexture(color) {
  return canvasTexture(256, 256, (g, w, h) => {
    kraft(g, w, h, color);
    g.fillStyle = 'rgba(214, 196, 160, 0.9)'; // packing tape along the seam
    g.fillRect(0, h / 2 - 22, w, 44);
  });
}

function signTexture(name) {
  return canvasTexture(1024, 150, (g, w, h) => {
    g.fillStyle = '#1e242d';
    g.fillRect(0, 0, w, h);
    g.fillStyle = CREAM;
    g.font = `500 76px ${FONT}`;
    g.textBaseline = 'middle';
    g.fillText(name, 44, h / 2 + 4);
  });
}

function bayTexture(bay) {
  return canvasTexture(128, 64, (g, w, h) => {
    g.fillStyle = '#f1eee7';
    g.fillRect(0, 0, w, h);
    g.fillStyle = '#1c2128';
    g.font = `500 38px ${FONT}`;
    g.textBaseline = 'middle';
    g.textAlign = 'center';
    g.fillText(`Bay ${bay}`, w / 2, h / 2 + 2);
  });
}

// A thin outline box that marks a slot or a matched box.
function outline(size, opacity = 0.9) {
  const edges = new THREE.EdgesGeometry(new THREE.BoxGeometry(size[0], size[1], size[2]));
  const line = new THREE.LineSegments(edges, new THREE.LineBasicMaterial({ color: CREAM, transparent: true, opacity, depthTest: true }));
  line.visible = false;
  line.renderOrder = 4;
  return line;
}

export class WarehouseViewer {
  constructor(canvas, status) {
    this.canvas = canvas;
    this.statusEl = status;
    this.states = new StateBuffer();
    this.showHud = params.get('hud') !== '0';
    this.cinematic = params.get('camera') === 'cinematic';
    this.director = null; // optional function(frameInfo) -> {position, target, fov}
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
    this.scene.fog = new THREE.Fog('#1b2129', 14, 36);
    this.camera = new THREE.PerspectiveCamera(40, 16 / 9, 0.02, 120);
    this.camera.up.set(0, 0, 1);
    this.camera.position.set(3.6, -6.4, 3.9);
    this.controls = new OrbitControls(this.camera, canvas);
    this.controls.target.set(0, 0.2, 0.7);
    this.controls.enableDamping = true;
    this.controls.dampingFactor = 0.1;
    this.controls.maxPolarAngle = Math.PI / 2 - 0.02;
    this.controls.minDistance = 0.4;
    this.controls.maxDistance = 30;
    this.controls.update();

    this.bodies = [];
    this.boxes = new Map(); // label -> {group, outline}
    this.signs = new Map(); // zone key -> {material, base}
    this.clock = new THREE.Clock();
    this.hudTimer = 0;
    this.pulse = 0;
    this.q0 = new THREE.Quaternion(); this.q1 = new THREE.Quaternion();
    this.buildWorld();
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
    scene.environmentIntensity = 0.6;

    const sky = new THREE.Mesh(new THREE.SphereGeometry(80, 32, 16), new THREE.ShaderMaterial({
      side: THREE.BackSide, depthWrite: false, fog: false,
      uniforms: { top: { value: new THREE.Color('#0f1318') }, horizon: { value: new THREE.Color('#1b2129') } },
      vertexShader: 'varying vec3 vDir; void main(){ vDir = normalize(position); gl_Position = projectionMatrix * modelViewMatrix * vec4(position,1.0); }',
      fragmentShader: `
        uniform vec3 top; uniform vec3 horizon; varying vec3 vDir;
        void main() {
          vec3 c = vDir.z > 0.0 ? mix(horizon, top, pow(vDir.z, 0.55)) : horizon;
          gl_FragColor = vec4(c, 1.0);
          #include <colorspace_fragment>
        }`,
    }));
    scene.add(sky);

    // A pool of light over the work area that falls off into the fog.
    const floorGeometry = new THREE.CircleGeometry(40, 128);
    const colors = [];
    const centre = new THREE.Color('#353d48'), edge = new THREE.Color('#1b2129'), c = new THREE.Color();
    const floorPositions = floorGeometry.getAttribute('position');
    for (let i = 0; i < floorPositions.count; i++) {
      c.copy(centre).lerp(edge, smoothstep(3.5, 15, Math.hypot(floorPositions.getX(i), floorPositions.getY(i) - 0.4)));
      colors.push(c.r, c.g, c.b);
    }
    floorGeometry.setAttribute('color', new THREE.Float32BufferAttribute(colors, 3));
    const floor = new THREE.Mesh(floorGeometry, new THREE.MeshStandardMaterial({ vertexColors: true, roughness: 0.9, metalness: 0 }));
    floor.receiveShadow = true;
    scene.add(floor);
    const lines = [];
    for (let i = -10; i <= 10; i++) { lines.push(i, -10, 0.001, i, 10, 0.001, -10, i, 0.001, 10, i, 0.001); }
    scene.add(new THREE.LineSegments(new THREE.BufferGeometry().setAttribute('position', new THREE.Float32BufferAttribute(lines, 3)),
      new THREE.LineBasicMaterial({ color: CREAM, transparent: true, opacity: 0.045, depthWrite: false })));

    scene.add(new THREE.HemisphereLight('#dde6f0', '#1c2128', 0.8));
    const key = new THREE.DirectionalLight('#fff4e8', 2.6);
    key.position.set(3, -5, 9);
    key.castShadow = true;
    key.shadow.mapSize.set(4096, 4096);
    Object.assign(key.shadow.camera, { left: -6, right: 6, top: 6, bottom: -6, near: 1, far: 25 });
    key.shadow.bias = -0.0002;
    key.shadow.normalBias = 0.015;
    scene.add(key);
    const rim = new THREE.DirectionalLight('#bcd4f0', 1.2);
    rim.position.set(-4, 6, 4);
    scene.add(rim);
    const fill = new THREE.DirectionalLight('#e8eef5', 0.9); // lifts the faces that look away from the key
    fill.position.set(-6, -3, 4);
    scene.add(fill);
  }

  // Floor tape in front of each rack and around the cart, where the robot works.
  buildTape(description) {
    const material = new THREE.MeshBasicMaterial({ color: CREAM, transparent: true, opacity: 0.22, depthWrite: false });
    const strip = (x, y, length, angle) => {
      const mesh = new THREE.Mesh(new THREE.PlaneGeometry(length, 0.05), material);
      mesh.position.set(x, y, 0.002);
      mesh.rotation.z = angle;
      this.scene.add(mesh);
    };
    const area = (cx, cy, facing, width, depth) => {
      const d = [Math.cos(facing), Math.sin(facing)], l = [-d[1], d[0]];
      // a U of tape: two sides running out from the fixture and a line across the aisle
      for (const s of [-1, 1]) {
        strip(cx + l[0] * s * width / 2 - d[0] * depth / 2, cy + l[1] * s * width / 2 - d[1] * depth / 2, depth, facing);
      }
      strip(cx - d[0] * depth, cy - d[1] * depth, width, facing + Math.PI / 2);
    };
    for (const zone of description.zones) area(zone.x, zone.y, zone.facing, description.rack.width, 0.95);
    const cart = description.cart, slots = description.slots.cart;
    const mid = slots.reduce((m, s) => [m[0] + s[0] / slots.length, m[1] + s[1] / slots.length], [0, 0]);
    area(mid[0] - 0.13 * Math.cos(cart.facing), mid[1] - 0.13 * Math.sin(cart.facing), cart.facing, 2.6, 0.95);
  }

  buildFixtures(description) {
    const materials = {
      post: new THREE.MeshStandardMaterial({ color: '#c8642e', roughness: 0.55, metalness: 0.25 }),
      board: new THREE.MeshStandardMaterial({ color: '#9aa1a9', roughness: 0.38, metalness: 0.55 }),
      cartDeck: new THREE.MeshStandardMaterial({ color: '#2e333a', roughness: 0.85, metalness: 0.1 }),
      cartFrame: new THREE.MeshStandardMaterial({ color: '#4f79a8', roughness: 0.45, metalness: 0.45 }),
      grip: new THREE.MeshStandardMaterial({ color: '#141619', roughness: 0.6 }),
      leg: new THREE.MeshStandardMaterial({ color: '#30363f', roughness: 0.5, metalness: 0.5 }),
      wheel: new THREE.MeshStandardMaterial({ color: '#15181c', roughness: 0.8 }),
    };
    const pick = (name) => name.startsWith('cart_') ? (
        name === 'cart_top' || name === 'cart_shelf' ? materials.cartDeck
        : name.startsWith('cart_post') || name.startsWith('cart_handle_') && name !== 'cart_handle_grip' ? materials.cartFrame
        : name === 'cart_handle_grip' ? materials.grip : name.startsWith('cart_wheel') ? materials.wheel : materials.leg)
      : name.includes('_post_') ? materials.post : name.endsWith('_board') ? materials.board : materials.leg;
    for (const fixture of description.fixtures) {
      const [a, b, c] = fixture.size;
      let geometry;
      if (fixture.shape === 'box') geometry = new THREE.BoxGeometry(2 * a, 2 * b, 2 * c);
      else { geometry = new THREE.CylinderGeometry(a, a, 2 * b, 24); geometry.rotateX(Math.PI / 2); }
      const mesh = new THREE.Mesh(geometry, pick(fixture.name));
      mesh.position.fromArray(fixture.pos);
      mesh.quaternion.fromArray(fixture.quat);
      mesh.receiveShadow = true;
      mesh.castShadow = !fixture.name.endsWith('_top_board'); // like overhead lighting: keep the shelf below lit
      this.scene.add(mesh);
    }
    // Zone signs on top of each rack, facing the aisle, and bay plates under the shelf.
    const rack = description.rack;
    for (const zone of description.zones) {
      const d = new THREE.Vector3(Math.cos(zone.facing), Math.sin(zone.facing), 0);
      const texture = signTexture(zone.name);
      const material = new THREE.MeshBasicMaterial({ map: texture, color: '#ffffff' });
      const face = new THREE.PlaneGeometry(rack.width - 0.12, (rack.width - 0.12) * 150 / 1024);
      const sign = new THREE.Group();
      const front = new THREE.Mesh(face, material), back = new THREE.Mesh(face, material);
      back.rotation.y = Math.PI; // readable from behind the rack too
      sign.add(front, back);
      sign.up.set(0, 0, 1);
      sign.position.set(zone.x, zone.y, 1.8).addScaledVector(d, 0.03);
      sign.lookAt(sign.position.clone().sub(d));
      this.scene.add(sign);
      const frame = outline([rack.width - 0.08, 0.02, (rack.width - 0.08) * 150 / 1024 + 0.04]);
      frame.position.copy(sign.position);
      frame.quaternion.copy(sign.quaternion).multiply(new THREE.Quaternion().setFromAxisAngle(new THREE.Vector3(1, 0, 0), Math.PI / 2));
      this.scene.add(frame);
      this.signs.set(zone.key, { frame });
      description.slots[zone.key].forEach((slot, bay) => {
        const plate = new THREE.Mesh(new THREE.PlaneGeometry(0.12, 0.06), new THREE.MeshBasicMaterial({ map: bayTexture(bay + 1) }));
        plate.up.set(0, 0, 1);
        plate.position.set(slot[0], slot[1], rack.shelf - 0.06).addScaledVector(d, -0.13 - 0.004);
        plate.lookAt(plate.position.clone().sub(d));
        this.scene.add(plate);
      });
    }
    this.slots = description.slots;
    this.layout = description; // zones, rack size and slots, for camera scripts using the director hook
    this.targetMarker = outline([0.3, 0.36, 0.26]);
    this.scene.add(this.targetMarker);
  }

  buildBodies(description, buffer) {
    const geometries = description.meshes.map((mesh) => {
      const geometry = new THREE.BufferGeometry();
      geometry.setAttribute('position', new THREE.BufferAttribute(new Float32Array(buffer, mesh.vertexOffset, mesh.vertexCount * 3), 3));
      geometry.setIndex(new THREE.BufferAttribute(new Uint32Array(buffer, mesh.indexOffset, mesh.indexCount), 1));
      return toCreasedNormals(geometry, THREE.MathUtils.degToRad(40));
    });
    const shell = new THREE.MeshStandardMaterial({ color: '#b9bec5', roughness: 0.42, metalness: 0.35 });
    const dark = new THREE.MeshStandardMaterial({ color: '#2a2f36', roughness: 0.55, metalness: 0.25 });
    this.bodies = description.bodies.map(() => { const group = new THREE.Group(); this.scene.add(group); return group; });
    for (const part of description.parts) {
      const mesh = new THREE.Mesh(geometries[part.mesh], part.rgba[0] < 0.4 ? dark : shell);
      mesh.position.fromArray(part.pos);
      mesh.quaternion.fromArray(part.quat);
      mesh.castShadow = mesh.receiveShadow = true;
      this.bodies[part.body].add(mesh);
    }
    for (const box of description.boxes) {
      const [x, y, z] = box.size;
      const geometry = new THREE.BoxGeometry(x, z, y); // built y-up, then turned so the label stands upright
      geometry.rotateX(Math.PI / 2);
      const label = new THREE.MeshStandardMaterial({ map: labelTexture(box.label, box.color), roughness: 0.85 });
      const top = new THREE.MeshStandardMaterial({ map: topTexture(box.color), roughness: 0.85 });
      const plain = new THREE.MeshStandardMaterial({ color: box.color, roughness: 0.9 });
      const mesh = new THREE.Mesh(geometry, [label, label, top, plain, plain, plain]);
      mesh.castShadow = mesh.receiveShadow = true;
      const group = this.bodies[box.body];
      group.add(mesh);
      const ring = outline([x + 0.05, y + 0.05, z + 0.05]);
      group.add(ring);
      this.boxes.set(box.label, { group, outline: ring, name: box.name });
    }
    this.zoneNames = Object.fromEntries(description.zones.map((z) => [z.key, z.name]));
    this.zoneNames.cart = 'Cart';
    this.pelvis = description.bodies.indexOf('pelvis');
  }

  async start() {
    try {
      this.statusEl.textContent = 'Loading the G1…';
      const [scene, meshes] = await Promise.all([fetch('/api/scene'), fetch('/api/meshes.bin')]);
      if (!scene.ok || !meshes.ok) throw new Error(`scene ${scene.status}, meshes ${meshes.status}`);
      const description = await scene.json();
      const buffer = await meshes.arrayBuffer();
      this.buildTape(description);
      this.buildFixtures(description);
      this.buildBodies(description, buffer);
      this.statusEl.textContent = 'Connecting to the simulator…';
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
      this.states.push(JSON.parse(event.data));
      if (!this.statusEl.hidden) this.statusEl.hidden = true;
    });
    source.onerror = () => { this.statusEl.hidden = false; this.statusEl.textContent = 'Reconnecting to the simulator…'; };
  }

  async pollStatus() {
    try {
      const status = await (await fetch('/api/status')).json();
      text('#link-mujoco', `MuJoCo ${status.mujocoVersion} · ${status.physicsHz} Hz`);
      text('#link-rtf', `${status.realtimeFactor.toFixed(2)}× real time`);
      this.status = status;
    } catch { /* the stream reports connection problems */ }
    setTimeout(() => this.pollStatus(), 2000);
  }

  key(event) {
    if (event.target !== document.body) return;
    const k = event.key.toLowerCase();
    if (k === 'h') { this.showHud = !this.showHud; this.applyHud(); }
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
  }

  frame() {
    const dt = Math.min(this.clock.getDelta(), 0.05);
    const t = this.states.advance(dt);
    if (t === null || !this.bodies.length) { this.renderer.render(this.scene, this.camera); return; }
    const [a, b, k] = this.states.bracket(t);
    for (let i = 0; i < this.bodies.length; i++) {
      const i3 = i * 3, i4 = i * 4, body = this.bodies[i];
      body.position.set(
        a.positions[i3] + (b.positions[i3] - a.positions[i3]) * k,
        a.positions[i3 + 1] + (b.positions[i3 + 1] - a.positions[i3 + 1]) * k,
        a.positions[i3 + 2] + (b.positions[i3 + 2] - a.positions[i3 + 2]) * k);
      this.q0.fromArray(a.quaternions, i4);
      this.q1.fromArray(b.quaternions, i4);
      body.quaternion.slerpQuaternions(this.q0, this.q1, k);
    }
    this.pulse += dt;
    this.updateHighlights(b);
    const pelvis = this.bodies[this.pelvis];
    const held = b.held ? this.bodies[this.boxIndex(b.held)] : null;
    this.frameInfo = {
      elapsed: t, dt, state: b,
      time: a.time + (a.epoch === b.epoch ? (b.time - a.time) * k : 0), // since this loop of the show began
      robot: pelvis.position.toArray(),
      heading: new THREE.Euler().setFromQuaternion(pelvis.quaternion, 'ZYX').z,
      held: held ? held.position.toArray() : null,
    };
    this.updateCamera(dt);
    this.hudTimer -= dt;
    if (this.hudTimer <= 0) { this.hudTimer = 0.12; this.updateHud(b); }
    this.renderer.render(this.scene, this.camera);
  }

  boxIndex(name) {
    for (const box of this.boxes.values()) if (box.name === name) return this.bodies.indexOf(box.group);
    return -1;
  }

  // Outline what the memory answered (matched boxes and zones) and the bay the robot is heading for.
  updateHighlights(state) {
    const glow = 0.55 + 0.35 * Math.sin(this.pulse * 5);
    const last = state.memory.last;
    const showMatches = last && MATCH_STATES.has(state.activity.title);
    for (const box of this.boxes.values()) box.outline.visible = false;
    for (const sign of this.signs.values()) sign.frame.visible = false;
    if (showMatches) {
      last.matches.forEach((match, rank) => {
        const opacity = rank === 0 ? glow : 0.3;
        if (match.kind === 'item' && this.boxes.has(match.label)) {
          const ring = this.boxes.get(match.label).outline;
          ring.visible = true; ring.material.opacity = opacity;
        } else if (match.kind === 'zone' && this.signs.has(match.zone)) {
          const frame = this.signs.get(match.zone).frame;
          frame.visible = true; frame.material.opacity = opacity;
        }
      });
    }
    const target = state.target;
    const marker = this.targetMarker;
    marker.visible = !!target && state.activity.title !== 'Asking turbopuffer';
    if (marker.visible) {
      const slot = this.slots[target.zone][target.bay];
      marker.position.set(slot[0], slot[1], slot[2] + 0.13);
      marker.rotation.set(0, 0, slot[3]);
      marker.material.opacity = 0.35 + 0.5 * glow;
    }
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
      this.orbit = (this.orbit ?? -Math.PI / 3) + dt * 0.05;
      const r = this.frameInfo.robot;
      this.controls.target.lerp(new THREE.Vector3(r[0], r[1], 0.8), 1 - Math.exp(-dt * 1.2));
      this.camera.position.set(this.controls.target.x + 5.2 * Math.cos(this.orbit), this.controls.target.y + 5.2 * Math.sin(this.orbit), 2.6);
      this.camera.lookAt(this.controls.target);
      return;
    }
    this.controls.update();
  }

  updateHud(state) {
    if (!this.showHud) return;
    text('#task-title', state.activity.title);
    text('#task-detail', state.activity.detail);
    const memory = state.memory;
    const stand = memory.backend !== 'turbopuffer';
    text('#memory-backend', memory.backend);
    text('#memory-detail', memory.detail);
    const last = memory.last;
    text('#memory-latency', !last ? '' : stand ? 'Local' : `${Math.round(last.roundTripMs)} ms`);
    const query = document.querySelector('#memory-query');
    if (last) {
      text('#memory-query', last.purpose === 'find' ? `“${last.text}”` : `Where do things like “${last.text}” go?`);
      query.classList.remove('muted');
    } else {   // a new loop: nothing asked yet
      text('#memory-query', 'Waiting for the first box');
      query.classList.add('muted');
    }
    const list = document.querySelector('#memory-matches');
    const key = last ? JSON.stringify(last.matches) + last.text : '';
    if (key !== this.matchKey) {
      this.matchKey = key;
      list.replaceChildren(...(last ? last.matches : []).map((match, rank) => {
        const li = document.createElement('li');
        if (rank === 0) li.className = 'best';
        const where = match.kind === 'zone' ? 'Zone description'
          : `${this.zoneNames[match.zone]} · ${match.shelf === 'top' ? 'top shelf' : `bay ${match.bay + 1}`}`;
        const similarity = Math.max(0, Math.min(1, 1 - match.distance));
        li.innerHTML = '<span class="rank"></span><span class="what"></span><span class="dist num"></span><span class="where"></span>';
        li.querySelector('.rank').textContent = String(rank + 1);
        li.querySelector('.what').textContent = match.kind === 'zone' ? this.zoneNames[match.zone] : match.label;
        // a zone row is the zone's description text, embedded like any item
        li.querySelector('.where').textContent = where;
        const dist = li.querySelector('.dist');
        dist.textContent = match.distance.toFixed(3);
        const bar = document.createElement('span');
        bar.className = 'bar';
        bar.style.width = `${Math.round(similarity * 100)}%`;
        li.querySelector('.where').appendChild(bar);
        return li;
      }));
    }
    const rows = last?.rows ?? memory.log.find((a) => a.rows)?.rows;
    const method = stand ? 'Word overlap, no embeddings' : 'Vector search over native embeddings';
    text('#memory-foot', [method, rows ? `${rows} rows` : null, last?.serverMs != null ? `${Math.round(last.serverMs)} ms server` : null]
      .filter(Boolean).join(' · '));
    const log = document.querySelector('#memory-log');
    const logKey = memory.log.map((a) => a.op + a.text + a.roundTripMs).join('|');
    if (logKey !== this.logKey) {
      this.logKey = logKey;
      log.replaceChildren(...memory.log.slice(0, 4).map((entry) => {
        const li = document.createElement('li');
        const what = document.createElement('span');
        what.textContent = entry.op === 'write' ? `Write · ${entry.text}${entry.note ? ` → ${entry.note}` : ''}` : `Query · ${entry.text}`;
        const ms = document.createElement('span');
        ms.textContent = entry.roundTripMs >= 1 ? `${Math.round(entry.roundTripMs)} ms` : '';
        li.append(what, ms);
        return li;
      }));
    }
    const error = document.querySelector('#memory-error');
    error.hidden = !memory.error;
    if (memory.error) text('#memory-error', memory.error);
  }
}
