import { mountViewer } from './viewer.js';
const byId = id => document.getElementById(id);
byId('viewer').replaceChildren();
let viewer = { resetView() {}, setActive() {} };
try { viewer = mountViewer(byId('viewer')); }
catch (error) { showError(error.message); }
byId('reset-view').addEventListener('click', () => viewer.resetView());
let latest, busy = false, imageKey, controllerChosen = false;
async function command(action, body = {}) {
  if (busy) return;
  busy = true;
  try {
    // Fetch a fresh token so a failed load or restarted server cannot leave
    // the controls permanently tied to a rejected promise or stale session.
    const session = await fetch('/api/session', { cache: 'no-store', signal: AbortSignal.timeout(5000) });
    if (!session.ok) throw Error(`Session HTTP ${session.status}`);
    const { token } = await session.json();
    if (!token) throw Error('Session token missing. Try again.');
    const response = await fetch(`/api/${action}`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Coke-Token': token }, body: JSON.stringify(body), signal: AbortSignal.timeout(5000) });
    const data = await response.json();
    if (!response.ok) throw Error(data.error || 'Command failed');
    byId('error').hidden = true;
    await refresh();
  } catch (error) { showError(error.message); }
  finally { busy = false; }
}
function showError(message) { byId('error').textContent = message; byId('error').hidden = false; }
byId('run').onclick = () => command('run', { mode: byId('mode').value, ...(byId('steps').value === 'full' ? {} : { steps: Number(byId('steps').value) }) });
byId('mode').onchange = () => { controllerChosen = true; byId('steps').options[0].textContent = ['policy', 'hil'].includes(byId('mode').value) ? 'Full reference · 188 seconds' : 'Full scene · 60 seconds'; };
byId('pause').onclick = () => command(latest?.phase === 'paused' ? 'resume' : 'pause');
byId('reset').onclick = () => command('reset');
function pill(id, label, connected) { const node = byId(id); node.textContent = label; node.classList.toggle('connected', connected); }
async function refresh() {
  try {
    const response = await fetch('/api/status', { cache: 'no-store', signal: AbortSignal.timeout(5000) });
    if (!response.ok) throw Error(`Status HTTP ${response.status}`);
    latest = await response.json();
    if (!controllerChosen && latest.phase !== 'loading') {
      if (latest.hil_enabled) byId('mode').value = 'hil';
      controllerChosen = true;
    }
    const running = ['running', 'starting', 'resetting'].includes(latest.phase);
    const loaded = !['loading', 'error'].includes(latest.phase);
    pill('connection', latest.phase === 'loading' ? 'Loading scene' : 'VM scene connected', loaded);
    byId('phase').textContent = { idle: 'Ready to run', loading: 'Loading scene', running: latest.mode === 'ros' ? 'ROS control' : latest.mode === 'expert' ? 'Expert replay' : 'Policy running', completed: 'Run complete', paused: 'Paused', error: 'Run stopped', starting: 'Starting', resetting: 'Resetting scene' }[latest.phase] || latest.phase;
    byId('time').textContent = `${latest.simulation_seconds.toFixed(1)} / ${(latest.total_steps / 40).toFixed(1)} s`;
    byId('progress').max = latest.total_steps; byId('progress').value = latest.step;
    byId('run').disabled = running || !loaded;
    byId('pause').disabled = !['running', 'paused'].includes(latest.phase);
    byId('pause').textContent = latest.phase === 'paused' ? 'Resume' : 'Pause';
    byId('reset').disabled = ['loading', 'resetting', 'starting'].includes(latest.phase);
    byId('mode').disabled = running; byId('steps').disabled = running;
    byId('mode').querySelector('[value="ros"]').disabled = !latest.ros_enabled;
    byId('mode').querySelector('[value="hil"]').disabled = !latest.hil_enabled;
    byId('lift').textContent = `${((latest.metrics.maximum_lift_m || 0) * 100).toFixed(1)} cm`;
    byId('distance').textContent = latest.metrics.final_placement_distance_m == null ? '—' : `${(latest.metrics.final_placement_distance_m * 100).toFixed(1)} cm`;
    byId('inference').textContent = latest.inference_ms == null ? '—' : `${latest.inference_ms.toFixed(1)} ms`;
    byId('hil-detail').textContent = latest.mode === 'hil' && latest.hil
      ? `Jetson compute ${latest.hil.compute_ms.toFixed(1)} ms; round trip ${latest.hil.round_trip_ms.toFixed(1)} ms. Vision: ${latest.hil.devices.vision}, control: ${latest.hil.devices.control}.`
      : latest.hil_enabled ? 'Jetson policy is configured. Select it above to run with the VM simulation.' : '';
    pill('visible', latest.can_visible ? 'Can visible' : 'Can out of view', latest.can_visible);
    pill('ros', latest.ros_enabled ? latest.ros?.enabled ? 'Connected' : 'Starting' : 'Local preview', !!latest.ros?.enabled);
    byId('ros-detail').textContent = latest.ros_enabled ? 'Trajectory targets pass through ROS 2 before reaching MuJoCo.' : 'ROS 2 is available when deployed to the Wendy VM.';
    const nextKey = `${latest.epoch}:${latest.metrics.camera_frame}`;
    if (nextKey !== imageKey && latest.metrics.camera_frame != null) { byId('camera').src = `/camera.jpg?v=${encodeURIComponent(nextKey)}`; byId('mask').src = `/mask.jpg?v=${encodeURIComponent(nextKey)}`; imageKey = nextKey; }
    if (latest.last_error) showError(latest.last_error); else if (!busy) byId('error').hidden = true;
    if (latest.joint_names && !byId('joints').children.length) latest.joint_names.forEach(name => { const div = document.createElement('div'); const label = document.createElement('label'); label.textContent = name; const value = document.createElement('span'); div.append(label, value); byId('joints').append(div); });
    if (latest.joints) [...byId('joints').children].forEach((div, i) => { div.lastChild.textContent = `${latest.joints[i].toFixed(3)} rad`; });
  } catch (error) { pill('connection', 'Disconnected', false); byId('run').disabled = true; byId('pause').disabled = true; byId('reset').disabled = true; showError(error.message); }
}
async function poll() { await refresh(); setTimeout(poll, 350); }
poll();
document.addEventListener('visibilitychange', () => viewer.setActive(!document.hidden));
if (document.modelContext?.registerTool) {
  const lifecycle = new AbortController();
  Promise.resolve(document.modelContext.registerTool({
    name: 'get_coke_demo_status',
    description: 'Read the current virtual G1 run, measured can lift, placement distance, and ROS connection status.',
    inputSchema: { type: 'object', properties: {}, additionalProperties: false },
    annotations: { readOnlyHint: true },
    async execute(input) {
      if (!input || typeof input !== 'object' || Object.keys(input).length) throw Error('No arguments are accepted');
      const response = await fetch('/api/status', { cache: 'no-store', signal: AbortSignal.timeout(5000) });
      if (!response.ok) throw Error('The Coke demo is unavailable');
      const value = await response.json();
      return { phase: value.phase, mode: value.mode, step: value.step, metrics: value.metrics, ros: value.ros, error: value.last_error };
    }
  }, { signal: lifecycle.signal })).catch(() => {});
  window.addEventListener('pagehide', () => lifecycle.abort(), { once: true });
}
