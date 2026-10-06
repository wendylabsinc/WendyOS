import * as THREE from "three";
import { createRobot } from "../../../go/simulator/rosmaster_r2/r2_sim/robot-model.js";

// Reuse the R2 runtime's detailed procedural model. Only geometry is constructed
// here; every moving world pose comes from the authorized simulator observer.
export function rosmasterScene(world) {
  const { robot, wheelGroups } = createRobot(world.geometry, world.lidar_offset);
  robot.updateMatrixWorld(true);
  const bodies = [{ id: 0, name: "world" }, { id: 1, name: "rosmaster-r2" },
    ...wheelGroups.map((_, i) => ({ id: i + 2, name: `wheel-${i}` }))];
  const meshes = [], geoms = [], geometryIDs = new Map();
  const matrix = new THREE.Matrix4(), instance = new THREE.Matrix4();
  const position = new THREE.Vector3(), quaternion = new THREE.Quaternion(), scale = new THREE.Vector3();
  function add(part, transform, body) {
    let id = geometryIDs.get(part.geometry);
    if (id === undefined) {
      id = meshes.length;
      geometryIDs.set(part.geometry, id);
      const attribute = part.geometry.getAttribute("position");
      meshes.push({ id, positions: Array.from(attribute.array),
        indices: part.geometry.index ? Array.from(part.geometry.index.array) : Array.from({ length: attribute.count }, (_, i) => i) });
    }
    transform.decompose(position, quaternion, scale);
    // Procedural R2 parts use unit scale; keep any geometry-authored scale in
    // vertices rather than adding a transform unsupported by the canvas model.
    let mesh = id;
    if (scale.distanceToSquared(new THREE.Vector3(1, 1, 1)) > 1e-10) {
      const source = meshes[id];
      mesh = meshes.length;
      meshes.push({ id: mesh, indices: source.indices,
        positions: source.positions.map((value, i) => value * [scale.x, scale.y, scale.z][i % 3]) });
    }
    const material = Array.isArray(part.material) ? part.material[0] : part.material;
    geoms.push({ type: "mesh", name: part.name || "R2 part", body, mesh,
      size: [0, 0, 0], position: position.toArray(), quaternion: quaternion.toArray(),
      rgba: [...material.color.toArray(), material.opacity] });
  }
  const baseInverse = new THREE.Matrix4();
  robot.traverse(part => {
    if (!part.isMesh) return;
    let body = 1;
    for (let ancestor = part.parent; ancestor; ancestor = ancestor.parent) {
      const wheel = wheelGroups.indexOf(ancestor);
      if (wheel >= 0) { body = wheel + 2; break; }
    }
    baseInverse.copy(body === 1 ? robot.matrixWorld : wheelGroups[body - 2].matrixWorld).invert();
    matrix.multiplyMatrices(baseInverse, part.matrixWorld);
    if (part.isInstancedMesh) {
      for (let i = 0; i < part.count; i++) {
        part.getMatrixAt(i, instance);
        add(part, new THREE.Matrix4().multiplyMatrices(matrix, instance), body);
      }
    } else add(part, matrix, body);
  });
  const geometries = new Set(), materials = new Set(), textures = new Set();
  robot.traverse(part => {
    if (part.geometry) geometries.add(part.geometry);
    for (const material of Array.isArray(part.material) ? part.material : [part.material]) {
      if (!material) continue;
      materials.add(material);
      for (const value of Object.values(material)) if (value?.isTexture) textures.add(value);
    }
  });
  geometries.forEach(item => item.dispose()); materials.forEach(item => item.dispose()); textures.forEach(item => item.dispose());
  const box = (name, size, position, rgba) => geoms.push({ type: "box", name, body: 0,
    size: size.map(value => value / 2), position, quaternion: [0, 0, 0, 1], rgba });
  const half = world.room_half_size;
  box("floor", [half * 2, half * 2, .04], [0, 0, -.02], [.36, .42, .45, 1]);
  for (const [x, y, width, depth] of [[-half, 0, .1, half * 2], [half, 0, .1, half * 2], [0, -half, half * 2, .1], [0, half, half * 2, .1]]) {
    box("room wall", [width, depth, .6], [x, y, .3], [.64, .68, .67, 1]);
  }
  for (const obstacle of world.obstacles) {
    box("obstacle", [obstacle.width, obstacle.depth, obstacle.height],
      [obstacle.x, obstacle.y, obstacle.height / 2], [.76, .72, .59, 1]);
  }
  return { version: 1, id: world.id, robot_body: 1, bodies, meshes, geoms };
}
