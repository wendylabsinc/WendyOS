// Exercise the shipped WASM worker and the local auth proxy, without signing
// in a user. Run while the web client serves http://localhost:5173.
import assert from 'node:assert/strict';
import { Worker } from 'node:worker_threads';
import { resolve } from 'node:path';
const worker=new Worker(`
const {parentPort,workerData}=require('node:worker_threads');
const fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
globalThis.WebSocket=require(path.join(workerData.assets,'../node_modules/ws'));
globalThis.location={origin:'http://localhost:5173'};
globalThis.self=globalThis;
globalThis.postMessage=value=>parentPort.postMessage(value);
globalThis.importScripts=(...names)=>{for(const name of names)vm.runInThisContext(fs.readFileSync(path.join(workerData.assets,name),'utf8'));};
globalThis.indexedDB=require(path.join(workerData.assets,'../node_modules/fake-indexeddb')).indexedDB;
const nativeFetch=globalThis.fetch;
globalThis.fetch=(input,options)=> {
 const url=new URL(input,location.origin);
 // Exercise hosts that return raw gzip as well as real Fetch auto-decoding.
 if(workerData.raw && url.pathname==='/wendy.wasm.gz')
   return Promise.resolve(new Response(fs.readFileSync(path.join(workerData.assets,'wendy.wasm.gz'))));
 return nativeFetch(url,options);
};
parentPort.on('message',data=>globalThis.onmessage({data}));
// Go detects Node and disables Fetch; this test needs the browser transport.
globalThis.process=Object.create(globalThis.process);
Object.defineProperty(globalThis.process,"argv0",{value:"browser"});
vm.runInThisContext(fs.readFileSync(path.join(workerData.assets,'wendy-worker.js'),'utf8'));
`,{eval:true,workerData:{assets:resolve('web-client/public'),raw:process.argv.includes('--raw-gzip')}});
let next=0;const pending=new Map();
const ready=new Promise((resolve,reject)=>{
 worker.on('message',d=>{if(d.ready)return resolve();if(d.fatal)return reject(new Error(d.fatal));const p=pending.get(d.id);if(!p)return;pending.delete(d.id);clearTimeout(p.timer);d.error?p.reject(new Error(d.error)):p.resolve(d.result);});worker.on('error',reject);
});
function call(method,params={}){return new Promise((resolve,reject)=>{const id=++next;const timer=setTimeout(()=>reject(new Error('Timed out: '+method)),45000);pending.set(id,{resolve,reject,timer});worker.postMessage({id,method,params});});}
try {
 await ready;
 const authorize=new URL(await call('auth-begin',{email:process.env.WENDY_TEST_EMAIL || '',redirect:'http://localhost:5173/auth/callback'}));
 assert.equal(authorize.origin,'https://auth.dev.wendy.sh');
 assert.equal(authorize.searchParams.get('client_id'),'cloud-login');
 assert.equal(authorize.searchParams.get('code_challenge_method'),'S256');
 assert.equal(authorize.searchParams.get('resource'),'https://pki.wendy.sh/identity');
 assert(authorize.searchParams.get('state').length>=43);
 const login=await fetch(authorize);
 assert.equal(login.status,200);assert.match(login.url,/auth\.dev\.wendy\.sh\/realms\/[^/]+\/login/);
 await assert.rejects(call('auth-complete',{state:'wrong',code:'none',issuer:'https://auth.dev.wendy.sh/realms/system'}),/state mismatch/);
 await assert.rejects(call('cloud-discover'),/Sign in with Wendy first/);
 console.log('PASS: shipped worker loader ('+(process.argv.includes('--raw-gzip')?'raw gzip':'HTTP auto-decoded gzip')+'), live auth discovery, PKCE, accepted callback, rejected invalid state and unsigned discovery');
} finally {await worker.terminate();}
