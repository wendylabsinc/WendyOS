import * as THREE from 'three';

// Original, procedural geometry. Dimensions follow the simulator's approximate
// rear-axle frame: +X forward, +Y left, +Z up. No network assets are required.
export const material = (color, roughness = .65, metalness = 0) =>
  new THREE.MeshStandardMaterial({color, roughness, metalness});

export function mesh(parent, geometry, mat, position = [0, 0, 0]) {
  const part = new THREE.Mesh(geometry, mat);
  part.position.set(...position);
  part.castShadow = part.receiveShadow = true;
  parent.add(part);
  return part;
}

export function box(parent, size, position, mat) {
  return mesh(parent, new THREE.BoxGeometry(...size), mat, position);
}

export function cylinder(parent, radius, length, position, mat, vertical = true, segments = 32) {
  const part = mesh(parent, new THREE.CylinderGeometry(radius, radius, length, segments), mat, position);
  if (vertical) part.rotation.x = Math.PI / 2;
  return part;
}

function roundedShape(width, depth, radius) {
  const shape = new THREE.Shape(), x = -width / 2, y = -depth / 2;
  shape.moveTo(x + radius, y);
  shape.lineTo(x + width - radius, y);
  shape.quadraticCurveTo(x + width, y, x + width, y + radius);
  shape.lineTo(x + width, y + depth - radius);
  shape.quadraticCurveTo(x + width, y + depth, x + width - radius, y + depth);
  shape.lineTo(x + radius, y + depth);
  shape.quadraticCurveTo(x, y + depth, x, y + depth - radius);
  shape.lineTo(x, y + radius);
  shape.quadraticCurveTo(x, y, x + radius, y);
  return shape;
}

export function plate(parent, width, depth, thickness, radius, position, mat, holes = []) {
  const shape = roundedShape(width, depth, radius);
  for (const [x, y, r] of holes) {
    const hole = new THREE.Path();
    hole.absarc(x, y, r, 0, Math.PI * 2, true);
    shape.holes.push(hole);
  }
  const bevel = Math.min(thickness / 4, .0012);
  const geometry = new THREE.ExtrudeGeometry(shape, {
    depth: thickness - bevel * 2, bevelEnabled: true, bevelThickness: bevel,
    bevelSize: bevel, bevelSegments: 2, steps: 1, curveSegments: 8,
  });
  geometry.translate(0, 0, -thickness / 2 + bevel);
  return mesh(parent, geometry, mat, position);
}

export function rod(parent, start, end, radius, mat) {
  const a = new THREE.Vector3(...start), b = new THREE.Vector3(...end);
  const part = mesh(parent, new THREE.CylinderGeometry(radius, radius, a.distanceTo(b), 12), mat);
  part.position.copy(a).add(b).multiplyScalar(.5);
  part.quaternion.setFromUnitVectors(new THREE.Vector3(0, 1, 0), b.sub(a).normalize());
  return part;
}

export function instances(parent, geometry, mat, transforms) {
  const parts = new THREE.InstancedMesh(geometry, mat, transforms.length);
  const transform = new THREE.Object3D();
  transforms.forEach(({position, rotation = [0, 0, 0]}, i) => {
    transform.position.set(...position);
    transform.rotation.set(...rotation);
    transform.updateMatrix();
    parts.setMatrixAt(i, transform.matrix);
  });
  parts.castShadow = parts.receiveShadow = true;
  parent.add(parts);
  return parts;
}

export function canvasTexture(width, height, draw) {
  const canvas = document.createElement('canvas');
  canvas.width = width; canvas.height = height;
  draw(canvas.getContext('2d'), width, height);
  const texture = new THREE.CanvasTexture(canvas);
  texture.colorSpace = THREE.SRGBColorSpace;
  texture.anisotropy = 4;
  return texture;
}
