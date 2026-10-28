import * as THREE from 'three';
import {GLTFLoader} from './vendor/loaders/GLTFLoader.js';
import {DRACOLoader} from './vendor/loaders/DRACOLoader.js';
import {OrbitControls} from './vendor/controls/OrbitControls.js';

const params = new URLSearchParams(location.search);
const board = params.get('board') || 'jetson-orin-nano';
const mode = params.get('mode') || 'recovery';
const boot = params.get('boot') === 'true';
const stage = document.querySelector('#stage');
const status = document.querySelector('#status');
const label = document.querySelector('#label');
const instruction = document.querySelector('#instruction');
const loadTimeout=setTimeout(()=>fail('Model load timed out'),30000);
let root, selected = 0, steps = [], highlights = [], animation = 0;
window.addEventListener('error',event=>fail(event.message));
const scene = new THREE.Scene();
const camera = new THREE.PerspectiveCamera(36, 1, .01, 100);
const renderer = new THREE.WebGLRenderer({antialias:true, alpha:true});
renderer.setPixelRatio(Math.min(devicePixelRatio, 2));
renderer.outputColorSpace = THREE.SRGBColorSpace;
renderer.toneMapping = THREE.ACESFilmicToneMapping;
renderer.toneMappingExposure = 1.45;
stage.prepend(renderer.domElement);
const controls = new OrbitControls(camera, renderer.domElement);
controls.minDistance = .2; controls.maxDistance = 14;
controls.enablePan = true;
scene.add(new THREE.HemisphereLight(0xffffff, 0x738398, 2.8));
for(const [position, intensity] of [[[3,6,5],3.5],[[-4,3,-3],2.5],[[0,-4,2],1.3]]) {
  const light = new THREE.DirectionalLight(0xffffff,intensity);light.position.set(...position);scene.add(light);
}
function render(){renderer.render(scene,camera)}
controls.addEventListener('change',render);
new ResizeObserver(()=>{const w=stage.clientWidth,h=stage.clientHeight;renderer.setSize(w,h);camera.aspect=w/h;camera.updateProjectionMatrix();render()}).observe(stage);
const decoder = new DRACOLoader().setDecoderPath('./draco/').setWorkerLimit(1);
const loader = new GLTFLoader().setDRACOLoader(decoder);
function fail(error){clearTimeout(loadTimeout);status.textContent='The 3D guide could not load. Use the illustrated instructions below.';window.guideError=String(error);window.webkit?.messageHandlers?.install3D?.postMessage({error:String(error)});}

function marker(position, radius=.09) {
  const group = new THREE.Group();group.position.copy(position);
  const ring = new THREE.Mesh(new THREE.SphereGeometry(radius,24,16), new THREE.MeshBasicMaterial({color:0xff9b21,transparent:true,opacity:.65,depthTest:false}));
  ring.renderOrder=100;group.add(ring);scene.add(group);highlights.push(group);
}
function matching(pattern) {let result=[];root.traverse(n=>{if(n.isMesh && pattern.test(n.name))result.push(n)});return result;}
function boundsOf(nodes) {const box=new THREE.Box3();nodes.forEach(n=>box.union(new THREE.Box3().setFromObject(n)));return box;}
function point(pattern) {const nodes=matching(pattern);if(!nodes.length)throw new Error('Missing model part: '+pattern);return boundsOf(nodes).getCenter(new THREE.Vector3());}
function focus(target, offset, instant=false) {
  cancelAnimationFrame(animation);
  const from=camera.position.clone(), fromTarget=controls.target.clone();const to=target.clone().add(new THREE.Vector3(...offset));
  if(instant || matchMedia('(prefers-reduced-motion: reduce)').matches){camera.position.copy(to);controls.target.copy(target);controls.update();render();return;}
  const start=performance.now();
  const tick=now=>{const t=Math.min((now-start)/650,1),e=t*t*(3-2*t);camera.position.lerpVectors(from,to,e);controls.target.lerpVectors(fromTarget,target,e);controls.update();render();if(t<1)animation=requestAnimationFrame(tick)};
  animation=requestAnimationFrame(tick);
}
function choose(index, instant=false) {
  selected=index;highlights.forEach(n=>{scene.remove(n);n.traverse(child=>{child.geometry?.dispose();child.material?.dispose()})});highlights=[];
  root.traverse(n=>{if(n.isMesh && n.userData.originalMaterial && n.material!==n.userData.originalMaterial){n.material.dispose();n.material=n.userData.originalMaterial;}if(n.userData.originalPosition)n.position.copy(n.userData.originalPosition);});
  const step=steps[index];label.textContent=step.title;instruction.textContent=step.text;
  document.querySelectorAll('#steps button').forEach((b,i)=>b.setAttribute('aria-pressed',String(i===index)));
  if(step.parts){for(const part of matching(step.parts)){part.userData.originalMaterial ||= part.material;part.material=new THREE.MeshStandardMaterial({color:0xff9b21,emissive:0xff7800,emissiveIntensity:.5,roughness:.5});}}
  const target=step.target || new THREE.Vector3();if(step.marker)marker(target,step.radius);
  step.draw?.();focus(target,step.offset || [4,3,5],instant);
}
function configure() {
  if(board==='dragonwing-iq-9075'){
    const usb=point(/^USB0.*reversible_contact_tongue/), dip=point(/^SW2.*white_rear_actuator_3/), power=point(/^JPWR.*central_power_pin/);
    steps=[
      {title:'USB0 data cable',text:'Connect your Mac to USB0. The highlighted USB-C connector carries EDL flashing data.',target:usb,parts:/^USB0/,offset:[1.5,1.2,2.8]},
      {title:boot?'SW2-3 → OFF':'SW2-3 → ON',text:boot?'Power off. Move only switch 3 of the SW2 mode bank away from ON. Then power-cycle to boot WendyOS.':'Power off. Move only switch 3 of the SW2 mode bank toward its printed ON label, then power on for EDL. Leave the other poles unchanged.',target:dip,parts:/^SW2.*white_rear_actuator_3/,offset:[-.45,.35,-1.4],draw:()=>{const actuator=matching(/^SW2.*white_rear_actuator_3/)[0];actuator.userData.originalPosition ||= actuator.position.clone();const world=actuator.getWorldPosition(new THREE.Vector3()).add(new THREE.Vector3(0,boot?-.035:.035,0));actuator.position.copy(actuator.parent.worldToLocal(world));}},
      {title:boot?'Power-cycle':'Connect power',text:boot?'With SW2-3 OFF, reconnect power. Leaving it ON returns the board to EDL.':'With SW2-3 ON and USB0 attached, reconnect power. A blank display in EDL is expected.',target:power,parts:/^JPWR/,offset:[-1.5,1.2,-2.6]}
    ];
  } else if(board==='jetson-orin-nano') {
    // Anchors are in the normalized display copy's coordinates. J14 is on the
    // rear edge under the module, separate from the vertical 40-pin GPIO bank.
    // Pin order reverses when viewed from the rear: 1 at left, 12 at right.
    const usb=new THREE.Vector3(1.50,-.265,1.47), header=new THREE.Vector3(-.48,-.33,-1.14);
    const storage={title:'NVMe underside',text:mode==='nvme'?'With power disconnected, remove the NVMe for imaging in a USB enclosure. After imaging, return it to the M.2 Key M slot and secure it.':'With power disconnected, install the NVMe in the M.2 Key M slot. Keep the SSD in the Jetson throughout USB recovery.',target:new THREE.Vector3(0,-.4,0),offset:[.1,-5,.1]};
    if(mode!=='recovery') {
      steps=[{title:'Identify the board',text:mode==='sd'?'Use a card reader attached to your Mac to write the microSD image. Return the card to the Jetson after a successful write.':'Write the NVMe through a USB enclosure attached to your Mac. This rootfs-only method leaves the Jetson boot firmware unchanged.',offset:[4,3,5]}, ...(mode==='nvme'?[storage]:[])];
    } else {
      steps=[
        storage,
        {title:'USB-C to your Mac',text:'Connect the highlighted USB-C data port to your Mac. Keep the DC power supply unplugged while fitting the recovery jumper.',target:usb,marker:true,radius:.1,offset:[2,1.4,3]},
        {title:boot?'Remove the J14 jumper':'Bridge J14 pins 9–10',text:boot?'Remove the jumper before normal boot. Follow the installer’s power-cycle instructions.':'With DC power unplugged, bridge only pin 9 (GND) and pin 10 (FC REC). The orange bridge shows the pair from the rear. Match the board labels before fitting a jumper.',target:header,offset:[0,.22,-2.6],draw:()=>{
          const p9=new THREE.Vector3(-.713,-.33,-1.14),p10=new THREE.Vector3(-.802,-.33,-1.14);
          marker(p9,.025);marker(p10,.025);
          if(!boot){const curve=new THREE.CatmullRomCurve3([p9,p9.clone().add(new THREE.Vector3(0,.10,-.08)),p10.clone().add(new THREE.Vector3(0,.10,-.08)),p10]);const wire=new THREE.Mesh(new THREE.TubeGeometry(curve,24,.018,8,false),new THREE.MeshBasicMaterial({color:0xff8b00,depthTest:false}));wire.renderOrder=100;scene.add(wire);highlights.push(wire);}
        }},
        {title:boot?'Boot WendyOS':'Apply DC power',text:boot?'With the recovery jumper removed, power-cycle the developer kit to boot WendyOS.':'Keep pins 9–10 bridged while connecting the DC supply. Remove the jumper once the installer detects recovery mode. Keep USB-C connected through final success.',target:new THREE.Vector3(-1.45,-.1,1.38),marker:true,radius:.1,offset:[-2,1.3,3]}
      ];
    }
  } else {
    const sd=new THREE.Vector3(-1.46,.055,-.385);
    steps=mode==='nvme'?[
      {title:'Pi 5 with NVMe',text:'Image the SSD in a USB NVMe enclosure attached to your Mac. The Pi remains powered off.',offset:[3,2,5]},
      {title:'Fit the M.2 HAT',text:'After imaging, install the SSD in your Pi 5 M.2 HAT and follow the HAT’s PCIe ribbon instructions. The base-board model does not include the HAT.',offset:[0,0,5]}
    ]:[
      {title:'microSD underside',text:boot?'With the Pi powered off, insert the flashed microSD card into the highlighted underside slot.':'Remove the microSD card from the powered-off Pi. Put it in a reader attached to your Mac for imaging.',target:sd,marker:true,radius:.16,offset:[-.8,.4,-3.2],draw:()=>{
        const arrow=new THREE.ArrowHelper(new THREE.Vector3(boot?1:-1,0,0),new THREE.Vector3(boot?-2.3:-1.65,.055,-.43),.6,0x007aff,.13,.09);scene.add(arrow);highlights.push(arrow);
      }},
      {title:'Board overview',text:'Return the flashed card to the Pi, connect networking, then power on. The supplied model depicts Raspberry Pi 5.',offset:[3,2,5]}
    ];
  }
  document.querySelector('#steps').replaceChildren(...steps.map((s,i)=>{const b=document.createElement('button');b.textContent=s.title;b.onclick=()=>choose(i);return b}));
  choose(boot && board==='dragonwing-iq-9075'?1:0,true);
  clearTimeout(loadTimeout);status.hidden=true;window.guideReady=true;
  window.webkit?.messageHandlers?.install3D?.postMessage({ready:true});
}
loader.load(`./models/${board}.glb`, gltf=>{
  root=gltf.scene;root.updateMatrixWorld(true);
  const box=new THREE.Box3().setFromObject(root),center=box.getCenter(new THREE.Vector3()),size=box.getSize(new THREE.Vector3());
  const normalization=new THREE.Group();scene.add(normalization);normalization.add(root);root.position.sub(center);normalization.scale.setScalar(3.6/Math.max(size.x,size.y,size.z));scene.updateMatrixWorld(true);
  root.traverse(n=>{if(n.isMesh){for(const m of Array.isArray(n.material)?n.material:[n.material]){m.roughness=Math.max(.35,m.roughness ?? .5);m.metalness=Math.min(.6,m.metalness ?? 0)}}});
  try {configure()} catch(error){fail(error)}
},undefined,fail);
document.querySelector('#reset').onclick=()=>choose(selected);
// Read-only diagnostics for asset validation; no app or device commands.
window.installGuide={pick:(x,y)=>{const r=new THREE.Raycaster();r.setFromCamera(new THREE.Vector2(x/stage.clientWidth*2-1,1-y/stage.clientHeight*2),camera);return r.intersectObject(root,true).slice(0,4).map(h=>({name:h.object.name,point:h.point.toArray()}))},select:index=>choose(index,true),inspect:()=>{const nodes=[];root?.traverse(n=>{if(n.isMesh){const b=new THREE.Box3().setFromObject(n);nodes.push({name:n.name,center:b.getCenter(new THREE.Vector3()).toArray(),size:b.getSize(new THREE.Vector3()).toArray()})}});return nodes},camera:(position,target)=>{camera.position.set(...position);controls.target.set(...target);controls.update();render()}};
