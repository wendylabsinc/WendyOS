"""Display-only companion UI and narrow proxy for the existing G1 wrapper."""
from __future__ import annotations

import json
import os
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


PORT = int(os.environ.get("CONTROL_UI_PORT", "8122"))
WRAPPER = os.environ.get("PHYSICAL_WRAPPER_URL", "http://127.0.0.1:8121").rstrip("/")
INFERENCE = os.environ.get("INFERENCE_URL", "http://127.0.0.1:8117").rstrip("/")
GET_PATHS = {
    "/state",
    "/health",
}
POST_PATHS = {
    "/api/reference-residual/run-policy",
    "/api/reference-residual/stop",
}

HTML = r"""<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>G1 responsive Coke policy</title><style>
:root{color-scheme:dark;font-family:ui-sans-serif,system-ui,-apple-system,sans-serif}body{margin:0;background:#0b0f14;color:#e8edf2}main{max-width:820px;margin:auto;padding:28px 20px 50px}h1{font-size:27px;margin:0 0 6px}.sub{color:#96a3b3;margin:0 0 20px}.card{background:#131a22;border:1px solid #263241;border-radius:14px;padding:18px;margin:14px 0}.status{display:flex;gap:10px;align-items:center;font-weight:750}.dot{width:11px;height:11px;border-radius:50%;background:#f0b429}.dot.ready{background:#3ddc97}.dot.active{background:#4da3ff}.dot.fault{background:#ff5c6c}dl{display:grid;grid-template-columns:1fr auto;gap:9px 18px}dt{color:#96a3b3}dd{margin:0;font-variant-numeric:tabular-nums}.actions{display:grid;grid-template-columns:1fr 1fr;gap:12px}button{border:0;border-radius:11px;padding:15px;font-weight:800;font-size:16px}button:disabled{opacity:.4}#run{background:#3ddc97;color:#06130d}#stop{background:#ffcf5c;color:#1b1300}select{background:#0b0f14;color:#e8edf2;border:1px solid #3a4a5d;border-radius:8px;padding:6px 10px;font-weight:750}.vision{position:relative;width:100%;aspect-ratio:4/3;background:#080b0f;border-radius:10px;overflow:hidden;margin-top:12px}.vision img,.vision svg{position:absolute;inset:0;width:100%;height:100%;object-fit:contain}.ideal{fill:none;stroke:#ff4fd8;stroke-width:3}.detected{fill:none;stroke:#41f0a8;stroke-width:3}.delta{stroke:white;stroke-width:2;stroke-dasharray:5 4}.legend{display:flex;gap:18px;color:#96a3b3;font-size:13px;margin-top:10px}.swatch{display:inline-block;width:10px;height:10px;border-radius:50%;margin-right:6px}.pink{background:#ff4fd8}.green{background:#41f0a8}.distance{font-size:22px;font-weight:800;margin:14px 0 4px}.warning{color:#ffcf5c;line-height:1.45}a{color:#78b7ff}pre{white-space:pre-wrap;overflow-wrap:anywhere;color:#b8c4d1;font-size:12px;max-height:220px;overflow:auto}.traces{display:grid;gap:7px;font-size:13px}
</style></head><body><main><h1>G1 responsive Coke policy</h1><p class="sub">Stage 2 wide update 16 · camera-relative can retarget · 40 Hz</p>
<section class="card"><div class="status"><span id="dot" class="dot"></span><span id="headline">Connecting…</span></div><dl><dt>Active policy</dt><dd id="policy">—</dd><dt>Replay rate</dt><dd><select id="rate"><option value="15">15 Hz · 80 s</option><option value="25" selected>25 Hz · 48 s</option><option value="30">30 Hz · 40 s</option><option value="40">40 Hz · 30 s</option></select></dd><dt>Policy steps</dt><dd id="steps">—</dd><dt>Effective rate</dt><dd id="hz">—</dd><dt>DDS writes</dt><dd id="writes">—</dd><dt>State skew</dt><dd id="skew">—</dd></dl><div class="actions"><button id="run" disabled>Run policy</button><button id="stop" disabled>Controlled stop / release</button></div></section>
<section class="card"><strong>Can alignment</strong><div class="vision"><img id="camera" alt="Live segmentation"><svg viewBox="0 0 320 240" preserveAspectRatio="xMidYMid meet"><line id="delta" class="delta" visibility="hidden"/><circle id="ideal" class="ideal" r="10" visibility="hidden"/><path id="cross" class="ideal" d="M-15 0H15M0-15V15" visibility="hidden"/><circle id="detected" class="detected" r="8" visibility="hidden"/></svg></div><div class="legend"><span><i class="swatch pink"></i>Ideal nominal can</span><span><i class="swatch green"></i>Detected can</span></div><div id="distance" class="distance">No fresh Coke mask</div><p id="alignment" class="sub">The ideal marker appears after the waist turns far enough for the nominal can to enter frame.</p><div id="traces" class="traces"></div></section>
<section class="card warning">Run only while the G1 is harnessed, the area is clear, and a safety observer is ready. A policy fault holds the measured pose at SDK weight 1 until Controlled stop / release.</section><section class="card"><strong>Last event</strong><pre id="event">Waiting for state…</pre></section>
</main><script>
const el=id=>document.getElementById(id);let state=null,activeCommand=null,busy=false,activePolicy=null;const commandId=()=>`operator-ui-${Date.now()}`,host=location.hostname,inference=`http://${location.hostname}:8117`;
function mark(id,p){const n=el(id);if(!p){n.setAttribute('visibility','hidden');if(id==='ideal')el('cross').setAttribute('visibility','hidden');return}n.setAttribute('visibility','visible');n.setAttribute('cx',p[0]);n.setAttribute('cy',p[1]);if(id==='ideal'){el('cross').setAttribute('transform',`translate(${p[0]} ${p[1]})`);el('cross').setAttribute('visibility','visible')}}
async function alignment(){const q=state?.live?.q_43||[];if(q.length!==43)return;el('camera').src=`http://${host}:8003/preview.jpg?t=${Date.now()}`;try{const u=`${inference}/retarget/project?yaw=${encodeURIComponent(q[12])}&roll=${encodeURIComponent(q[13])}&pitch=${encodeURIComponent(q[14])}`,v=await(await fetch(u,{cache:'no-store'})).json(),a=v.ideal_pixel_xy,b=v.detected_pixel_xy;mark('ideal',a);mark('detected',b);const line=el('delta');if(a&&b){for(const [k,x] of [['x1',a[0]],['y1',a[1]],['x2',b[0]],['y2',b[1]]])line.setAttribute(k,x);line.setAttribute('visibility','visible')}else line.setAttribute('visibility','hidden');if(b&&v.distance_to_optimal_cm!=null){el('distance').textContent=`${Number(v.distance_to_optimal_cm).toFixed(1)} cm from optimal`;el('alignment').textContent=`${Number(v.pixel_displacement_px||0).toFixed(0)} px in camera · ${Number(v.detection_age_ms||0).toFixed(0)} ms old · raw planar distance`}else{el('distance').textContent='No fresh Coke mask';el('alignment').textContent=a?'Ideal location is visible; waiting for a detected mask.':'Nominal can is outside the current camera view.'}}catch(e){el('alignment').textContent=`Overlay unavailable: ${e}`}}
async function traces(){try{const v=await(await fetch(`${inference}/retarget/runs`,{cache:'no-store'})).json();el('traces').innerHTML=(v.runs||[]).slice(0,5).map(r=>`<a href="${inference}${r.download_path}">${r.session_id} · saved JSONL</a>`).join('')}catch(_){}}
async function contract(){try{activePolicy=(await(await fetch(`${inference}/contract`,{cache:'no-store'})).json()).active_policy||null}catch(_){}}
async function refresh(){try{state=await(await fetch('/state',{cache:'no-store'})).json();const p=state.policy_runtime||{},io=state.io||{},policy=activePolicy||p.active_policy||p.camera?.active_policy||{},running=!!p.running,holding=!!p.holding_after_fault,ready=state.healthy&&!running&&!holding&&!io.publishers_armed;activeCommand=(running&&p.command_id)||activeCommand;el('dot').className=`dot ${holding?'fault':running?'active':ready?'ready':''}`;el('headline').textContent=holding?'Fault hold active — release required':running?`Policy running at ${Number(p.selected_execution_hz||el('rate').value)} Hz`:ready?'Ready to run':'Not ready';el('policy').textContent=policy.candidate_id?`${policy.candidate_id} · ${String(policy.checkpoint_sha256||'').slice(0,12)}`:'—';el('steps').textContent=`${p.policy_steps_completed||0} / ${p.policy_steps_total||1200}`;el('hz').textContent=`${Number(p.effective_policy_hz||0).toFixed(2)} Hz`;el('writes').textContent=io.dds_writes_accepted??0;el('skew').textContent=`${(Number(state.live?.state_skew_s||0)*1000).toFixed(2)} ms`;el('rate').disabled=busy||running||holding||io.publishers_armed;el('run').disabled=busy||!ready;el('stop').disabled=busy||!(running||holding||io.publishers_armed);if(holding&&p.last_error)el('event').textContent=p.last_error;else if(!busy)el('event').textContent=running?`Running ${activeCommand}`:'No active fault';await alignment()}catch(e){el('headline').textContent='Wrapper unreachable';el('dot').className='dot fault';el('run').disabled=true;el('event').textContent=String(e)}}
el('run').onclick=async()=>{busy=true;try{const r=await fetch('/api/reference-residual/preflight',{cache:'no-store'}),c=await r.json();if(!r.ok||c.accepted!==true)throw Error(c.error||JSON.stringify(c));const checkpoint=c.active_policy?.checkpoint_sha256,label=c.active_policy?.candidate_id||'active policy',executionHz=Number(el('rate').value);if(!checkpoint)throw Error('Preflight did not pin a checkpoint');if(!confirm(`Preflight passed for ${label}. Replay at ${executionHz} Hz. Confirm harness, clear area, and safety observer. Start one bounded run?`))return;activeCommand=commandId();const response=await fetch('/api/reference-residual/run-policy',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({schema:'wendy.g1.reference-residual-physical-run-request.v1',command_id:activeCommand,operator_confirmed:true,maximum_policy_steps:Math.min(1200,Number(c.reference_frames||1200)),expected_checkpoint_sha256:checkpoint,execution_hz:executionHz})}),value=await response.json();el('event').textContent=JSON.stringify(value,null,2);if(!response.ok)throw Error(value.error||`Run rejected ${response.status}`)}catch(e){el('event').textContent=String(e)}finally{busy=false;refresh();traces()}};
el('stop').onclick=async()=>{if(!activeCommand)return;busy=true;try{const r=await fetch('/api/reference-residual/stop',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({schema:'wendy.g1.reference-residual-physical-stop-request.v1',command_id:activeCommand,operator_confirmed:true})});el('event').textContent=JSON.stringify(await r.json(),null,2)}catch(e){el('event').textContent=String(e)}finally{busy=false;refresh();traces()}};contract();refresh();traces();setInterval(refresh,1000);setInterval(traces,5000);setInterval(contract,10000);
</script></body></html>"""


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        return

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        if path == "/":
            return self.reply(200, HTML.encode(), "text/html; charset=utf-8")
        if path == "/api/reference-residual/preflight":
            return self.preflight()
        if path not in GET_PATHS:
            return self.reply_json(404, {"error": "not found"})
        self.proxy("GET", path, None)

    def do_POST(self):
        path = self.path.split("?", 1)[0]
        if path not in POST_PATHS:
            return self.reply_json(404, {"error": "not found"})
        length = int(self.headers.get("Content-Length", "0"))
        if length <= 0 or length > 32768:
            return self.reply_json(400, {"error": "invalid request length"})
        body = self.rfile.read(length)
        if path == "/api/reference-residual/run-policy":
            try:
                value = json.loads(body)
                expected = value.pop("expected_checkpoint_sha256")
                contract = self.fetch_json(INFERENCE + "/contract")
                actual = (contract.get("active_policy") or {}).get("checkpoint_sha256")
                if expected != actual:
                    return self.reply_json(
                        409,
                        {"accepted": False, "error": "active checkpoint changed after preflight"},
                    )
                body = json.dumps(value, separators=(",", ":")).encode()
            except (KeyError, TypeError, ValueError, urllib.error.URLError) as exc:
                return self.reply_json(409, {"accepted": False, "error": str(exc)})
        self.proxy("POST", path, body)

    @staticmethod
    def fetch_json(url: str, *, body: bytes | None = None) -> dict:
        request = urllib.request.Request(
            url,
            data=body,
            method="POST" if body is not None else "GET",
            headers={"Content-Type": "application/json"} if body is not None else {},
        )
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.load(response)

    def preflight(self):
        try:
            state = self.fetch_json(WRAPPER + "/state")
            policy = state.get("policy_runtime") or {}
            io = state.get("io") or {}
            if (
                state.get("healthy") is not True
                or policy.get("running")
                or policy.get("holding_after_fault")
                or io.get("publishers_armed")
            ):
                return self.reply_json(
                    409,
                    {"accepted": False, "error": "physical wrapper is not disarmed and ready"},
                )
            contract = self.fetch_json(INFERENCE + "/contract")
            inference_check = self.fetch_json(INFERENCE + "/preflight", body=b"{}")
            active = contract.get("active_policy") or {}
            if not active.get("checkpoint_sha256"):
                raise ValueError("inference contract omitted checkpoint identity")
            return self.reply_json(
                200,
                {
                    "accepted": True,
                    "active_policy": active,
                    "reference_frames": contract.get("reference_frames"),
                    "camera": inference_check.get("camera"),
                    "motion_command_sent": False,
                },
            )
        except Exception as exc:
            return self.reply_json(
                503,
                {"accepted": False, "error": f"{type(exc).__name__}: {exc}"},
            )

    def proxy(self, method: str, path: str, body: bytes | None):
        request = urllib.request.Request(
            WRAPPER + path,
            data=body,
            method=method,
            headers={"Content-Type": "application/json"} if body else {},
        )
        try:
            with urllib.request.urlopen(request, timeout=120) as response:
                self.reply(response.status, response.read(), "application/json")
        except urllib.error.HTTPError as exc:
            self.reply(exc.code, exc.read(), "application/json")
        except Exception as exc:
            self.reply_json(503, {"error": f"{type(exc).__name__}: {exc}"})

    def reply_json(self, status: int, value: dict):
        self.reply(status, json.dumps(value, separators=(",", ":")).encode(), "application/json")

    def reply(self, status: int, body: bytes, content_type: str):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
