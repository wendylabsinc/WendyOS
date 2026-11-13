import * as THREE from 'three';
import {material, box, plate, canvasTexture} from './visuals.js';

function concreteTexture() {
  // Seeded grain makes screenshots repeatable and avoids external texture files.
  let seed = 418;
  const random = () => { seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0; return seed / 4294967296; };
  const texture = canvasTexture(512, 512, (ctx, width, height) => {
    const pixels = ctx.createImageData(width, height);
    for (let i = 0; i < pixels.data.length; i += 4) {
      const grain = 175 + Math.floor(random() * 15);
      pixels.data.set([grain + 4, grain + 3, grain, 255], i);
    }
    ctx.putImageData(pixels, 0, 0);
    ctx.strokeStyle = 'rgba(83,87,86,.22)'; ctx.lineWidth = 1.5;
    ctx.strokeRect(0, 0, width, height);
    for (let i = 0; i < 90; i++) {
      ctx.strokeStyle = 'rgba(110,110,106,.06)'; ctx.lineWidth = random() * 1.5;
      const x = random() * width, y = random() * height;
      ctx.beginPath(); ctx.moveTo(x, y); ctx.lineTo(x + random() * 50, y + random() * 2); ctx.stroke();
    }
  });
  texture.wrapS = texture.wrapT = THREE.RepeatWrapping;
  return texture;
}

function studioEnvironment(renderer) {
  // A small light box supplies soft reflection shapes for metal, glass and paint.
  const studio = new THREE.Scene(); studio.background = new THREE.Color(0xbcc6d0);
  const walls = new THREE.Mesh(new THREE.BoxGeometry(12, 12, 12), new THREE.MeshBasicMaterial({color:0x68737d, side:THREE.BackSide}));
  studio.add(walls);
  for (const [size, position, intensity] of [
    [[5, .1, 3], [0, -4, 3], 4], [[.1, 5, 3], [-4, 0, 2], 2], [[4, 4, .1], [0, 0, 5], 3],
  ]) box(studio, size, position, new THREE.MeshBasicMaterial({color:new THREE.Color(intensity, intensity, intensity)}));
  const generator = new THREE.PMREMGenerator(renderer);
  const target = generator.fromScene(studio, .04);
  generator.dispose();
  studio.traverse(part => { if (part.isMesh) { part.geometry.dispose(); part.material.dispose(); } });
  return target;
}

export function createEnvironment(scene, renderer, world) {
  scene.background = new THREE.Color(0xc5cbd0);
  scene.fog = new THREE.Fog(0xc5cbd0, 13, 30);
  const reflection = studioEnvironment(renderer);
  scene.environment = reflection.texture;
  scene.environmentIntensity = .55;
  const sky = new THREE.HemisphereLight(0xe6efff, 0x7a746a, 1.3);
  sky.position.set(0, 0, 1); scene.add(sky);
  const sun = new THREE.DirectionalLight(0xfff1dd, 3.2);
  sun.castShadow = true; sun.shadow.mapSize.set(2048, 2048);
  Object.assign(sun.shadow.camera, {left:-3, right:3, top:3, bottom:-3, near:.5, far:14});
  sun.shadow.normalBias = .003; sun.shadow.bias = -.00008; sun.shadow.radius = 3;
  scene.add(sun, sun.target);
  const fill = new THREE.DirectionalLight(0xc8dfff, .8); fill.position.set(2, 4, 3); scene.add(fill);
  const floorSize = world.room_half_size * 2 + .2;
  const concrete = concreteTexture(); concrete.repeat.set(floorSize, floorSize);
  const floorMat = new THREE.MeshStandardMaterial({map:concrete, roughness:.87, metalness:.04});
  const floor = box(scene, [floorSize, floorSize, .08], [0, 0, -.04], floorMat); floor.castShadow = false;
  const wall = material(0xb6bbb9, .83), trim = material(0x39484c, .48, .35);
  const half = world.room_half_size;
  for (const [x, y, w, d] of [[-half - .05, 0, .1, floorSize], [half + .05, 0, .1, floorSize], [0, -half - .05, floorSize, .1], [0, half + .05, floorSize, .1]]) {
    box(scene, [w, d, .6], [x, y, .3], wall);
    box(scene, [w + .002, d + .002, .055], [x, y, .045], trim);
    box(scene, [w + .012, d + .012, .018], [x, y, .602], trim);
  }
  const paint = material(0xdddcd4, .63, .12), corners = material(0x718087, .34, .65);
  const marking = canvasTexture(256, 64, ctx => {
    ctx.fillStyle = '#c9a653'; ctx.fillRect(0, 0, 256, 64);
    ctx.fillStyle = '#303a3e';
    for (let x = -64; x < 320; x += 64) {
      ctx.beginPath(); ctx.moveTo(x, 0); ctx.lineTo(x + 32, 0); ctx.lineTo(x + 96, 64); ctx.lineTo(x + 64, 64); ctx.fill();
    }
  });
  marking.wrapS = marking.wrapT = THREE.RepeatWrapping;
  const stripeMaterial = length => {
    const map = marking.clone(); map.repeat.set(length / .112, 1);
    return new THREE.MeshStandardMaterial({map, roughness:.68});
  };
  world.obstacles.forEach((obstacle, i) => {
    const {x, y, width, depth, height} = obstacle;
    // Keep every visible solid inside the simulator's obstacle bounds.
    box(scene, [width - .004, depth - .004, height - .004], [x, y, height / 2], paint);
    const wideStripe = stripeMaterial(width), narrowStripe = stripeMaterial(depth);
    for (const side of [-1, 1]) {
      box(scene, [width - .036, .012, .028], [x, y + side * (depth / 2 - .006), height - .06], wideStripe);
      box(scene, [.012, depth - .036, .028], [x + side * (width / 2 - .006), y, height - .06], narrowStripe);
      for (const end of [-1, 1]) box(scene, [.018, .018, height], [x + side * (width / 2 - .009), y + end * (depth / 2 - .009), height / 2], corners);
    }
    plate(scene, width - .025, depth - .025, .009, .01, [x, y, height - .005], paint);
    const number = canvasTexture(128, 128, ctx => {
      ctx.fillStyle = '#dddcd4'; ctx.fillRect(0, 0, 128, 128);
      ctx.fillStyle = '#59666b'; ctx.font = '500 66px sans-serif'; ctx.textAlign = 'center'; ctx.fillText(`0${i + 1}`, 64, 88);
    });
    const label = new THREE.Mesh(new THREE.PlaneGeometry(.19, .19), new THREE.MeshStandardMaterial({map:number, roughness:.7}));
    label.position.set(x, y - depth / 2 - .0005, height / 2);
    label.rotation.x = Math.PI / 2; scene.add(label);
  });
  const sunOffset = new THREE.Vector3(-3, -4, 7);
  return {
    update(position) {
      sun.position.copy(position).add(sunOffset);
      sun.target.position.copy(position);
    },
  };
}
