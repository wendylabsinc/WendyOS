import * as THREE from 'three';
import {material, mesh, box, cylinder, plate, rod, instances, canvasTexture} from './visuals.js';

// A visual approximation of the Nano R2, authored from product photographs.
// The collision footprint and sensor origins remain owned by simulation.py.
export function createRobot(g, lidarOffset) {
  const robot = new THREE.Group();
  robot.name = 'rosmaster-r2';
  const wheelGroups = [], wheels = [];
  const black = material(0x22292e, .36, .5);
  const rubber = material(0x202124, .93);
  const tread = material(0x292b2e, .88);
  const alloy = material(0xa6afb2, .27, .85);
  const brass = material(0xa8924e, .32, .75);
  const red = material(0xc52d36, .32, .55);
  const pcb = material(0x175b3f, .62, .15);
  const plastic = material(0x161b20, .58);
  const blue = material(0x067faa, .42, .3);
  const glass = new THREE.MeshPhysicalMaterial({color:0x123746, roughness:.12, metalness:.45, clearcoat:1});
  const led = new THREE.MeshStandardMaterial({color:0x72d5a2, emissive:0x35d87e, emissiveIntensity:1.8});
  const center = g.wheelbase / 2;
  const chassisWidth = g.track - .055;
  const chassisLength = g.length - .052;
  const front = center + chassisLength / 2;
  const back = center - chassisLength / 2;

  function screw(parent, x, y, z) {
    cylinder(parent, .0034, .0023, [x, y, z], alloy, true, 6);
    cylinder(parent, .0013, .0025, [x, y, z + .0002], black, true, 6);
  }

  // Thin machined decks, open mounting holes and exposed brass standoffs.
  const holes = [];
  for (const x of [-.105, -.045, .045, .105]) for (const y of [-.063, .063]) holes.push([x, y, .003]);
  plate(robot, chassisLength, chassisWidth, .005, .013, [center, 0, .063], pcb);
  plate(robot, chassisLength, chassisWidth, .005, .014, [center, 0, .135], black, holes);
  for (const x of [back + .019, front - .02]) for (const y of [-1, 1]) {
    cylinder(robot, .0037, .069, [x, y * (chassisWidth / 2 - .012), .099], brass, true, 6);
    screw(robot, x, y * (chassisWidth / 2 - .012), .139);
  }
  // Battery, rear geared motors and front steering servo below the deck.
  plate(robot, .096, .098, .055, .009, [center - .018, 0, .097], blue);
  for (const y of [-.041, .041]) box(robot, [.015, .105, .057], [center - .018 + y, 0, .097], rubber);
  for (const side of [-1, 1]) {
    cylinder(robot, .019, .055, [0, side * .059, g.wheel_radius], alloy, false);
    cylinder(robot, .022, .018, [0, side * .097, g.wheel_radius], black, false);
    rod(robot, [0, side * .09, g.wheel_radius], [0, side * g.track / 2, g.wheel_radius], .004, alloy);
    box(robot, [.048, .005, .055], [0, side * .089, .065], pcb);
    rod(robot, [g.wheelbase - .034, side * .048, .068], [g.wheelbase, side * (g.track / 2 - .02), .05], .004, alloy);
  }
  box(robot, [.04, .039, .03], [g.wheelbase - .033, 0, .088], plastic);
  cylinder(robot, .01, .008, [g.wheelbase - .022, 0, .108], alloy);
  rod(robot, [g.wheelbase - .025, -.08, .074], [g.wheelbase - .025, .08, .074], .0025, alloy);
  plate(robot, .022, g.width - .044, .028, .008, [center + g.length / 2 - .015, 0, .057], rubber);

  // Nano carrier board with connectors, heat sink, fan and GPIO header.
  plate(robot, .092, .105, .003, .003, [.025, 0, .153], pcb);
  for (const x of [-.012, .062]) for (const y of [-.043, .043]) {
    cylinder(robot, .0025, .014, [x, y, .145], brass);
    screw(robot, x, y, .156);
  }
  box(robot, [.061, .062, .009], [.018, 0, .162], black);
  const fins = [];
  for (let i = 0; i < 13; i++) fins.push({position:[-.01 + i * .0045, 0, .174]});
  instances(robot, new THREE.BoxGeometry(.0015, .058, .025), black, fins);
  cylinder(robot, .021, .007, [.018, 0, .19], plastic);
  cylinder(robot, .007, .009, [.018, 0, .191], alloy);
  const blades = [];
  for (let i = 0; i < 7; i++) {
    const a = i * Math.PI * 2 / 7;
    blades.push({position:[.018 + Math.cos(a) * .012, Math.sin(a) * .012, .195], rotation:[0, 0, a + .5]});
  }
  instances(robot, new THREE.BoxGeometry(.014, .006, .001), black, blades);
  for (const y of [-.034, -.008, .027]) {
    box(robot, [.014, .018, .016], [-.024, y, .163], alloy);
    box(robot, [.001, .013, .01], [-.0315, y, .163], plastic);
  }
  box(robot, [.056, .006, .006], [.025, .045, .159], plastic);
  const pins = [];
  for (let i = 0; i < 16; i++) pins.push({position:[.002 + i * .003, .045, .165]});
  instances(robot, new THREE.BoxGeometry(.001, .001, .006), brass, pins);
  cylinder(robot, .002, .002, [.067, -.036, .157], led);

  // Raised lidar platform and scanner at the actual simulated scan origin.
  const [lx, ly, lz] = lidarOffset;
  for (const x of [lx - .045, lx + .045]) for (const y of [-.046, .046]) {
    cylinder(robot, .003, .078, [x, y, lz - .092], black);
    screw(robot, x, y, lz - .048);
  }
  plate(robot, .112, .119, .003, .009, [lx, ly, lz - .049], pcb);
  cylinder(robot, .043, .007, [lx, ly, lz - .043], black);
  cylinder(robot, .038, .027, [lx, ly, lz - .0265], plastic);
  cylinder(robot, .042, .017, [lx, ly, lz - .004], glass);
  cylinder(robot, .043, .012, [lx, ly, lz + .0105], black);
  cylinder(robot, .038, .002, [lx, ly, lz + .0175], plastic);
  cylinder(robot, .006, .003, [lx, ly, lz + .019], black);
  cylinder(robot, .0015, .002, [lx + .026, ly, lz + .019], led);

  // Front depth camera: rounded shell, inset face and three separate lenses.
  const cameraMount = new THREE.Group(); cameraMount.position.set(front - .015, 0, .2); robot.add(cameraMount);
  box(robot, [.023, .042, .037], [front - .025, 0, .155], black);
  cylinder(robot, .012, .057, [front - .025, 0, .176], black, false);
  const cameraBody = plate(cameraMount, .17, .042, .027, .018, [0, 0, 0], plastic);
  cameraBody.rotation.set(Math.PI / 2, Math.PI / 2, 0);
  const face = plate(cameraMount, .155, .032, .002, .014, [.015, 0, 0], black);
  face.rotation.copy(cameraBody.rotation);
  for (const [y, radius] of [[-.052, .008], [0, .006], [.052, .008]]) {
    const bezel = cylinder(cameraMount, radius + .002, .003, [.017, y, 0], plastic, false);
    bezel.rotation.z = Math.PI / 2;
    const lens = cylinder(cameraMount, radius, .0035, [.019, y, 0], glass, false);
    lens.rotation.z = Math.PI / 2;
    const reflection = cylinder(cameraMount, radius * .24, .0037, [.0195, y - radius * .25, radius * .28], alloy, false);
    reflection.rotation.z = Math.PI / 2;
  }
  // Rear antenna and routed power/data cables are visible between the decks.
  cylinder(robot, .004, .019, [back + .02, .059, .155], black);
  rod(robot, [back + .02, .059, .159], [back - .018, .071, .249], .0032, plastic);
  for (const [color, offset] of [[0xa92525, 0], [0x161a1e, .004]]) {
    const curve = new THREE.CatmullRomCurve3([
      new THREE.Vector3(center, -.038 + offset, .106), new THREE.Vector3(.04, -.064 + offset, .12),
      new THREE.Vector3(-.013, -.061 + offset, .143), new THREE.Vector3(.008, -.031 + offset, .155),
    ]);
    mesh(robot, new THREE.TubeGeometry(curve, 20, .0016, 6, false), material(color, .72));
  }

  const badge = canvasTexture(512, 128, ctx => {
    ctx.fillStyle = '#151a1d'; ctx.fillRect(0, 0, 512, 128);
    ctx.fillStyle = '#d4dade'; ctx.font = '600 45px sans-serif';
    ctx.fillText('ROSMASTER', 26, 79);
    ctx.fillStyle = '#e34c50'; ctx.fillText('R2', 385, 79);
  });
  const label = mesh(robot, new THREE.PlaneGeometry(.126, .031), new THREE.MeshStandardMaterial({map:badge, roughness:.6}), [center + .03, 0, .138]);
  label.castShadow = false;

  // Lathed tire shoulders and separate raised tread blocks catch grazing light.
  // Instancing keeps the repeated tread, rim bolts and spokes inexpensive.
  const r = g.wheel_radius, width = .044;
  const profile = [[.64, -.5], [.83, -.51], [.94, -.43], [1, -.29], [1, .29], [.94, .43], [.83, .51], [.64, .5]]
    .map(([radius, y]) => new THREE.Vector2(radius * r, y * width));
  const tireGeometry = new THREE.LatheGeometry(profile, 64);
  const treadTransforms = [];
  for (let i = 0; i < 48; i++) for (const side of [-1, 1]) {
    const a = i * Math.PI * 2 / 48;
    treadTransforms.push({position:[Math.sin(a) * (r - .0007), side * .0105, Math.cos(a) * (r - .0007)], rotation:[0, a, side * .35]});
  }
  const wheelLocations = [[g.wheelbase, g.track / 2], [g.wheelbase, -g.track / 2], [0, g.track / 2], [0, -g.track / 2]];
  wheelLocations.forEach(([x, y], index) => {
    const pivot = new THREE.Group(); pivot.position.set(x, y, r); pivot.name = `wheel-pivot-${index}`;
    robot.add(pivot); wheelGroups.push(pivot);
    const wheel = new THREE.Group(); wheel.name = `wheel-${index}`; pivot.add(wheel); wheels.push(wheel);
    mesh(wheel, tireGeometry, rubber);
    instances(wheel, new THREE.BoxGeometry(.0023, .020, .0018), tread, treadTransforms);
    cylinder(wheel, r * .65, width * .86, [0, 0, 0], black, false, 48);
    for (const side of [-1, 1]) {
      const lip = mesh(wheel, new THREE.TorusGeometry(r * .69, .0024, 8, 64), red, [0, side * width * .5, 0]);
      lip.rotation.x = Math.PI / 2;
      const spokes = [], bolts = [];
      for (let i = 0; i < 10; i++) {
        const a = i * Math.PI * 2 / 10;
        spokes.push({position:[Math.sin(a) * r * .38, side * width * .48, Math.cos(a) * r * .38], rotation:[0, a + .18, 0]});
        bolts.push({position:[Math.sin(a) * r * .7, side * width * .53, Math.cos(a) * r * .7], rotation:[0, 0, 0]});
      }
      instances(wheel, new THREE.BoxGeometry(.0035, .0025, r * .52), black, spokes);
      instances(wheel, new THREE.CylinderGeometry(.0011, .0011, .0015, 6), alloy, bolts);
      cylinder(wheel, .009, .006, [0, side * width * .48, 0], black, false);
      cylinder(wheel, .004, .007, [0, side * width * .53, 0], alloy, false, 6);
    }
  });
  return {robot, wheelGroups, wheels};
}
