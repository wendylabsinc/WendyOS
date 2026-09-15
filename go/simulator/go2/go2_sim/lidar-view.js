import * as THREE from './vendor/three.module.js';

// An observer-only diagnostic layer. Every point is an actual captured return
// in world coordinates; no maximum-range rays or decorative sweeps are drawn.
export class LidarView {
  constructor(scene) {
    this.enabled = true;
    this.active = true;
    this.identity = null;
    this.identityReceived = 0;
    this.nextPoll = 0;
    this.expires = 0;
    this.request = null;
    this.dirty = false;
    this.disposed = false;
    this.group = new THREE.Group();
    this.group.name = 'lidar-overlay';
    this.group.visible = false;
    this.group.userData = { pointCount: 0, fresh: false, status: 'waiting' };
    this.positions = new Float32Array(1800 * 3);
    this.geometry = new THREE.BufferGeometry();
    this.geometry.setAttribute('position', new THREE.BufferAttribute(this.positions, 3).setUsage(THREE.DynamicDrawUsage));
    this.geometry.setDrawRange(0, 0);
    this.material = new THREE.PointsMaterial({ color: 0x37e4ed, size: 0.032, transparent: true,
      opacity: 0.85, depthWrite: false, sizeAttenuation: true });
    this.points = new THREE.Points(this.geometry, this.material);
    this.points.frustumCulled = false;
    this.markerGeometry = new THREE.SphereGeometry(0.025, 12, 8);
    this.markerMaterial = new THREE.MeshBasicMaterial({ color: 0x37e4ed, depthTest: false });
    this.marker = new THREE.Mesh(this.markerGeometry, this.markerMaterial);
    this.group.add(this.points, this.marker);
    this.group.traverse(object => object.layers.set(1));
    scene.add(this.group);
    this.visibilityChanged = () => {
      if (document.hidden) this.cancel('hidden');
      else this.nextPoll = 0;
    };
    document.addEventListener('visibilitychange', this.visibilityChanged);
  }

  setEnabled(enabled) {
    this.enabled = Boolean(enabled);
    if (!this.enabled) this.cancel('hidden');
    else this.nextPoll = 0;
  }

  setActive(active) {
    this.active = Boolean(active);
    if (!this.active) this.cancel('inactive');
    else this.nextPoll = 0;
  }

  setStateIdentity(state) {
    const previous = this.identity;
    this.identity = state;
    this.identityReceived = performance.now();
    if (!previous || previous.scene_id !== state.scene_id || previous.epoch !== state.epoch ||
        previous.generation !== state.generation) {
      this.cancel('waiting');
      this.nextPoll = 0;
    }
    if (state.valid === false || ['paused', 'fault'].includes(state.mode)) this.cancel(state.mode);
  }

  eligible(now) {
    return !this.disposed && this.enabled && this.active && !document.hidden && this.identity &&
      this.identity.valid !== false && !['paused', 'fault'].includes(this.identity.mode) &&
      now - this.identityReceived < 750;
  }

  clear(status) {
    if (this.group.visible || this.group.userData.status !== status) this.dirty = true;
    this.group.visible = false;
    this.geometry.setDrawRange(0, 0);
    Object.assign(this.group.userData, { pointCount: 0, fresh: false, status });
  }

  cancel(status) {
    if (this.request) this.request.controller.abort();
    this.request = null;
    this.clear(status);
  }

  async poll(now) {
    const request = { controller: new AbortController() };
    this.request = request;
    this.nextPoll = now + 100;
    const timeout = setTimeout(() => request.controller.abort(), 1500);
    try {
      const response = await fetch('/api/scene/lidar', { cache: 'no-store', signal: request.controller.signal });
      if (!response.ok) throw Error('Lidar unavailable');
      const sample = await response.json();
      const received = performance.now();
      if (this.request !== request || !this.eligible(received)) return;
      const state = this.identity;
      if (sample.scene_id !== state.scene_id || sample.epoch !== state.epoch ||
          sample.generation !== state.generation) {
        this.clear('waiting');
        return;
      }
      if (!sample.enabled || !sample.available || !sample.fresh || ['paused', 'fault'].includes(sample.mode)) {
        this.clear(!sample.enabled ? 'disabled' : sample.mode === 'paused' ? 'paused' : 'waiting');
        return;
      }
      const finiteArray = (value, size) => Array.isArray(value) && value.length === size && value.every(Number.isFinite);
      if (!Array.isArray(sample.points) || sample.points.length > this.positions.length ||
          sample.points.length % 3 || !sample.points.every(Number.isFinite) ||
          !finiteArray(sample.origin, 3) || !finiteArray(sample.quaternion, 4) ||
          !Number.isFinite(sample.age_ms) || sample.age_ms < 0) throw Error('Invalid lidar observation');
      this.expires = received + 500 - sample.age_ms - (received - now);
      if (this.expires <= received) {
        this.clear('stale');
        return;
      }
      this.positions.set(sample.points);
      this.geometry.attributes.position.needsUpdate = true;
      const pointCount = sample.points.length / 3;
      this.geometry.setDrawRange(0, pointCount);
      this.marker.position.fromArray(sample.origin);
      this.marker.quaternion.fromArray(sample.quaternion).normalize();
      this.group.visible = true;
      Object.assign(this.group.userData, { pointCount, fresh: true, status: 'live',
        epoch: sample.epoch, generation: sample.generation, time: sample.time });
      this.dirty = true;
    } catch (error) {
      if (this.request === request) {
        this.clear('unavailable');
        this.nextPoll = performance.now() + 1000;
      }
    } finally {
      clearTimeout(timeout);
      if (this.request === request) this.request = null;
    }
  }

  update(now) {
    if (!this.eligible(now)) {
      if (this.request || this.group.visible) this.cancel('inactive');
    }
    else {
      if (this.group.visible && now >= this.expires) this.clear('stale');
      if (!this.request && now >= this.nextPoll) void this.poll(now);
    }
    const dirty = this.dirty;
    this.dirty = false;
    return dirty;
  }

  dispose() {
    this.disposed = true;
    this.cancel('disposed');
    document.removeEventListener('visibilitychange', this.visibilityChanged);
    this.group.removeFromParent();
    this.geometry.dispose();
    this.material.dispose();
    this.markerGeometry.dispose();
    this.markerMaterial.dispose();
  }
}
