// Validate the app's actual WASM bridge in Node's worker runtime against the
// existing local fixture. This exercises RPCs, not browser UI automation.
// node worker.mjs http://127.0.0.1:8787 /absolute/path/to/web-client/public
import assert from 'node:assert/strict';
import { Worker } from 'node:worker_threads';
import { resolve } from 'node:path';
const origin=process.argv[2]||'http://127.0.0.1:8787';
const assets=resolve(process.argv[3]||'web-client/public');
const fixture=await (await fetch(origin+'/fixture')).json();
const worker=new Worker(`
const {parentPort,workerData}=require('node:worker_threads');
const fs=require('node:fs');const vm=require('node:vm');const path=require('node:path');
globalThis.WebSocket=require(path.join(workerData,'../node_modules/ws'));
globalThis.postMessage=value=>parentPort.postMessage(value);
vm.runInThisContext(fs.readFileSync(path.join(workerData,'wasm_exec.js'),'utf8'));
parentPort.on('message',request=>globalThis.wendyRequest(JSON.stringify(request)));
(async()=>{
 const go=new Go();const wasm=require('node:zlib').gunzipSync(fs.readFileSync(path.join(workerData,'wendy.wasm.gz')));
 const {instance}=await WebAssembly.instantiate(wasm,go.importObject);await go.run(instance);
})().catch(e=>parentPort.postMessage({fatal:String(e)}));
`,{eval:true,workerData:assets});
let next=0;
const pending=new Map();const events=[];
const ready=new Promise((resolve,reject)=>{
 worker.on('message',data=>{
  if(data.ready){resolve();return;}
  if(data.fatal){reject(new Error(data.fatal));return;}
  if(data.event){events.push(data);return;}
  const p=pending.get(data.id);if(!p)return;pending.delete(data.id);clearTimeout(p.timer);
  data.error?p.reject(new Error(data.error)):p.resolve(data.result);
 });
 worker.on('error',reject);
});
function call(method,params={}){const id=++next;return new Promise((resolve,reject)=>{
 const timer=setTimeout(()=>{pending.delete(id);reject(new Error(method+' timed out'));},20000);
 pending.set(id,{resolve,reject,timer});worker.postMessage({id,method,params});
});}
try{
 await ready;
 const base={relay:origin.replace(/^http/,'ws')+'/tunnel',certificate:fixture.certificate,assetId:42};
 await assert.rejects(call('connect',{...base,assetId:43}),/identity|asset|43/i);
 const version=await call('connect',base);assert.equal(version.version,'wasm-mtls-fixture');
 const connectionStats=async()=>await (await fetch(origin+'/connection-stats')).json();
 const connected=await connectionStats();
 assert.equal(connected.active,1,'Expected one live device connection');
 const snap=await call('snapshot');assert.equal(snap.version.version,'wasm-mtls-fixture');
 assert.equal(snap.warnings.length,2,'Unavailable fixture RPCs must produce honest warnings');
 assert.deepEqual(snap.apps,[]);
 await assert.rejects(call('shell-open',{rows:24,cols:80}),/Shell access is disabled/);
 for (const kind of ['logs','metrics','traces']) {
   await call(kind);
   const deadline=Date.now()+3000;
   while(!events.some(e=>e.event===kind)&&Date.now()<deadline)await new Promise(r=>setTimeout(r,20));
   assert(events.some(e=>e.event===kind),'Expected OTLP '+kind+' stream');
   // Poll while each telemetry stream is open, matching the dashboard cadence.
   await new Promise(r=>setTimeout(r,2000));
   await call('snapshot');
   assert.deepEqual(await connectionStats(),connected,'Polling and switching telemetry must reuse the same TLS connection');
   await call('telemetry-stop');
 }
 await call('disconnect');
 await assert.rejects(call('snapshot'),/Connect to a device/);
 console.log('PASS: device identity rejection, authenticated connect, snapshot, RPC warnings, OTLP logs/metrics/traces sharing one TLS connection, shell disabled, disconnect');
}finally{await worker.terminate();}
