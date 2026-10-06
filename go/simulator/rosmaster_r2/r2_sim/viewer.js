import * as THREE from 'three';
import { OrbitControls } from '/vendor/OrbitControls.js';
import { createRobot } from './robot-model.js';
import { createEnvironment } from './environment.js';

const $ = id => document.getElementById(id);
const canvas = $('world');
const renderer = new THREE.WebGLRenderer({canvas, antialias: true});
renderer.setPixelRatio(Math.min(devicePixelRatio, 2));
renderer.shadowMap.enabled = true;
renderer.shadowMap.type = THREE.PCFSoftShadowMap;
renderer.toneMapping = THREE.ACESFilmicToneMapping;
renderer.toneMappingExposure = 1.12;
renderer.outputColorSpace = THREE.SRGBColorSpace;
const scene = new THREE.Scene();
const camera = new THREE.PerspectiveCamera(45, 1, .01, 80);
camera.up.set(0, 0, 1);
const controls = new OrbitControls(camera, canvas);
controls.enableDamping = true;
controls.maxPolarAngle = Math.PI / 2 - .04;
controls.minDistance = .45;
controls.maxDistance = 22;
let robot, wheelGroups, wheels, environment, geometry;
const lidarGeometry = new THREE.BufferGeometry();
const lidarPoints = new THREE.Points(lidarGeometry, new THREE.PointsMaterial({color: 0xd76525, size: .018, sizeAttenuation: true}));
scene.add(lidarPoints);
const trailGeometry = new THREE.BufferGeometry();
const trailLine = new THREE.Line(trailGeometry, new THREE.LineBasicMaterial({color: 0x377d88}));
scene.add(trailLine);
const trail = [];
let token = null, sequence = 0, keys = new Set(), state = null, epoch = null, view = 'orbit';
let lastPose = new THREE.Vector3(), lastScan = 0, lastStatus = 0;
let commandPending = false, paused = false;
async function api(path, body) {
  const response = await fetch(path, body === undefined ? {signal: AbortSignal.timeout(2000)} : {
    method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify(body), signal: AbortSignal.timeout(2000)
  });
  const result = await response.json();
  if (!response.ok) throw new Error(result.error || 'Simulator request failed');
  return result;
}
function notice(message) { $('notice').textContent = message; }
function forgetControls() { token = null; keys.clear(); $('enable').textContent = 'Enable keyboard controls'; }
async function action(path, body = {}) {
  try { return await api(path, body); }
  catch (error) { notice(error.message); return null; }
}
function center() {
  const p = robot.localToWorld(new THREE.Vector3(geometry.wheelbase / 2, 0, .13));
  controls.target.copy(p);
  camera.position.copy(p).add(view === 'top' ? new THREE.Vector3(0,-.001,9) : new THREE.Vector3(.85,-1.1,.72));
  controls.update();
}
function setView(mode) {
  view = mode; controls.enabled = mode !== 'driver';
  for (const name of ['orbit','top','driver']) $(name).classList.toggle('active', name === mode);
  center();
}
for (const mode of ['orbit','top','driver']) $(mode).onclick = () => setView(mode);
$('recenter').onclick = center;
$('enable').onclick = async () => {
  forgetControls();
  const result = await action('/api/arm', {mode:'browser'});
  if (result) { token = result.token; sequence = 0; $('enable').textContent = 'Keyboard controls enabled'; notice('Hold W to drive. A and D steer the front wheels.'); }
};
$('app').onclick = async () => { forgetControls(); if(await action('/api/arm',{mode:'app'})) notice('App control enabled. Drive from the henk page.'); };
$('ros').onclick = async () => { forgetControls(); if (await action('/api/arm',{mode:'ros'})) notice('Listening on /cmd_vel. The first fresh publisher receives control.'); };
async function stop() { forgetControls(); await action('/api/stop'); notice('Stopped. Enable controls before driving again.'); }
$('stop').onclick = stop;
$('reset').onclick = async () => { forgetControls(); if(await action('/api/reset')) { trail.length=0; notice('World reset. Enable controls to drive.'); } };
$('pause').onclick = async () => { forgetControls(); await action('/api/pause',{paused:!paused}); };
$('limit').oninput = () => { $('limit-value').textContent = `${Number($('limit').value).toFixed(2)} m/s`; };
const keyMap = {ArrowUp:'w',ArrowDown:'s',ArrowLeft:'a',ArrowRight:'d',w:'w',s:'s',a:'a',d:'d'};
window.addEventListener('keydown', event => {
  if (event.code === 'Space' && !['INPUT','TEXTAREA','SELECT'].includes(event.target.tagName)) { event.preventDefault(); if(!event.repeat) stop(); return; }
  if (['INPUT','TEXTAREA','SELECT'].includes(event.target.tagName) || event.ctrlKey || event.metaKey || event.altKey) return;
  const key=keyMap[event.key] || keyMap[event.key.toLowerCase()];
  if (key && token) { event.preventDefault(); keys.add(key); }
});
window.addEventListener('keyup', event => { const key=keyMap[event.key] || keyMap[event.key.toLowerCase()]; if(key) keys.delete(key); });
function releaseOnBlur() {
  if (token) { forgetControls(); navigator.sendBeacon('/api/stop',new Blob(['{}'],{type:'application/json'})); }
}
window.addEventListener('blur',releaseOnBlur);
document.addEventListener('visibilitychange', () => { if(document.hidden) releaseOnBlur(); });
for (const button of document.querySelectorAll('[data-key]')) {
  button.addEventListener('pointerdown', event => { if(token) { event.preventDefault(); button.setPointerCapture(event.pointerId); keys.add(button.dataset.key); } });
  for (const event of ['pointerup','pointercancel','lostpointercapture']) button.addEventListener(event,() => keys.delete(button.dataset.key));
}
setInterval(async () => {
  for (const button of document.querySelectorAll('[data-key]')) button.classList.toggle('active',keys.has(button.dataset.key));
  if (!token || commandPending) return;
  commandPending = true;
  const body = {token, sequence:++sequence, speed:(Number(keys.has('w'))-Number(keys.has('s')))*Number($('limit').value),
    steering:(Number(keys.has('a'))-Number(keys.has('d')))*(geometry?.max_steering || Math.PI/6)};
  try { await api('/api/command',body); }
  catch(error) { forgetControls(); notice(error.message); }
  finally { commandPending=false; }
},50);
async function poll() {
  try {
    const status = await api('/api/status'); lastStatus=performance.now();
    state=status.state; paused=status.mode==='paused';
    if (epoch !== status.epoch) { epoch=status.epoch; trail.length=0; forgetControls(); }
    if (status.control_mode!=='browser') forgetControls();
    $('connection').textContent=status.healthy ? (paused?'Simulation paused':'Simulator connected') : 'Simulator fault';
    $('connection').style.color=status.healthy?'#d4d4d8':'#fca5a5';
    $('speed').textContent=state.speed.toFixed(2); $('steering').textContent=(state.steering*180/Math.PI).toFixed(1);
    $('position').textContent=`${state.x.toFixed(2)}, ${state.y.toFixed(2)} m`;
    $('heading').textContent=`${(state.yaw*180/Math.PI).toFixed(1)}°`; $('distance').textContent=`${state.distance.toFixed(2)} m`;
    $('owner').textContent=status.owner?(status.control_mode==='browser'?'Keyboard / touch':status.control_mode==='app'?'App':'ROS 2 publisher'):'None';
    $('pause').textContent=paused?'Resume':'Pause'; $('ros').disabled=!status.ros_enabled || paused || !status.healthy;
    $('enable').disabled=paused || !status.healthy; $('app').disabled=paused || !status.healthy; $('collision').hidden=!state.collision;
    if (status.error) notice(status.error);
    wheelGroups.forEach((pivot,i) => { pivot.rotation.z=i<2?state.steering_angles[i]:0; wheels[i].rotation.y=state.wheel_positions[i]; });
    if(!trail.length || Math.hypot(state.x-trail[trail.length-1].x,state.y-trail[trail.length-1].y)>.025) {
      trail.push(new THREE.Vector3(state.x,state.y,.012)); if(trail.length>3000) trail.shift();
      trailGeometry.setFromPoints(trail);
    }
  } catch(error) {
    forgetControls(); $('connection').textContent='Connection lost'; $('connection').style.color='#fca5a5'; notice('Reconnecting. Enable controls again after the connection returns.');
  }
  setTimeout(poll,50);
}
async function pollScan() {
  try {
    const scan=await api('/api/scan'), positions=[];
    if(!scan.paused && scan.epoch===epoch) {
      scan.ranges.forEach((range,i) => { if(range!==null) { const angle=scan.yaw+scan.angle_min+i*scan.angle_increment; positions.push(scan.origin[0]+Math.cos(angle)*range,scan.origin[1]+Math.sin(angle)*range,scan.origin[2]); } });
    }
    lidarGeometry.setAttribute('position',new THREE.Float32BufferAttribute(positions,3));
    lidarGeometry.computeBoundingSphere(); lastScan=performance.now();
  } catch { lidarPoints.visible=false; }
  setTimeout(pollScan,100);
}
let lastRender=performance.now();
function render() {
  const now=performance.now(), blend=1-Math.exp(-Math.min((now-lastRender)/1000,.1)*22); lastRender=now;
  if(state) {
    const previous=robot.position.clone();
    robot.position.lerp(new THREE.Vector3(state.x,state.y,0),blend);
    const delta=robot.position.clone().sub(previous);
    if($('follow').checked && view!=='driver') { camera.position.add(delta); controls.target.add(delta); }
    robot.rotation.z+=Math.atan2(Math.sin(state.yaw-robot.rotation.z),Math.cos(state.yaw-robot.rotation.z))*blend;
  }
  const width=canvas.clientWidth,height=canvas.clientHeight;
  if(canvas.width!==Math.round(width*renderer.getPixelRatio()) || canvas.height!==Math.round(height*renderer.getPixelRatio())) {
    renderer.setSize(width,height,false); camera.aspect=width/height; camera.updateProjectionMatrix();
  }
  if(view==='driver' && state) {
    const {x,y}=robot.position, yaw=robot.rotation.z;
    camera.position.set(x+Math.cos(yaw)*.285,y+Math.sin(yaw)*.285,.235);
    camera.lookAt(x+Math.cos(yaw)*3,y+Math.sin(yaw)*3,.235);
  } else controls.update();
  lidarPoints.visible=$('lidar').checked && !paused && performance.now()-lastScan<500 && performance.now()-lastStatus<500;
  trailLine.visible=$('trail').checked;
  environment.update(robot.position);
  renderer.render(scene,camera); requestAnimationFrame(render);
}
try {
  const world=await api('/api/scene'); geometry=world.geometry;
  ({robot, wheelGroups, wheels} = createRobot(geometry, world.lidar_offset));
  scene.add(robot);
  environment = createEnvironment(scene, renderer, world);
  center(); poll(); pollScan(); render();
} catch(error) { $('connection').textContent='Unable to load simulator'; notice(error.message); }
