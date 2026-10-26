// Run after `npm run build:wasm`: the built worker, as a page loads it.
import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';
import {readFileSync} from 'node:fs';
import {gunzipSync} from 'node:zlib';
import vm from 'node:vm';
import {indexedDB} from 'fake-indexeddb';
const pub=(name)=>new URL(`../public/${name}`,import.meta.url);
const gz=readFileSync(pub('wendy.wasm.gz'));
const pin=createHash('sha256').update(gunzipSync(gz)).digest('hex');

function startWorker(query){
  const messages=[];let waiting=[];
  const scope={indexedDB,crypto,TextEncoder,TextDecoder,Uint8Array,URL,Blob,Response,DecompressionStream,WebAssembly,performance,console,setTimeout,clearTimeout,
    location:new URL(`https://console.example/wendy-web/1/wendy-worker.js${query}`),
    fetch:async(url)=>{assert.equal(String(url),'https://console.example/wendy-web/1/wendy.wasm.gz','the module loads next to the worker');return new Response(gz);},
    importScripts:(...names)=>{for(const name of names)vm.runInContext(readFileSync(pub(name),'utf8'),scope);},
    postMessage:(data)=>{messages.push(data);for(const check of waiting)check();},
  };
  scope.self=scope;scope.globalThis=scope;
  vm.createContext(scope);
  vm.runInContext(readFileSync(pub('wendy-worker.js'),'utf8'),scope);
  const next=(match)=>new Promise((resolve)=>{const check=()=>{const found=messages.find(match);if(found){waiting=waiting.filter((c)=>c!==check);resolve(found);}};waiting.push(check);check();});
  let id=0;
  const call=(method,params)=>{const request={id:++id,method,params};scope.onmessage({data:request});return next((m)=>m.id===request.id);};
  return {next,call};
}

const tampered=startWorker('?sha256='+'0'.repeat(64));
assert.equal((await tampered.next((m)=>m.fatal||m.ready)).fatal,'sha256 mismatch','a module that is not the pinned one never runs');

const worker=startWorker('?sha256='+pin);
assert.ok((await worker.next((m)=>m.fatal||m.ready)).ready,'the pinned module starts');
const settings={auth_base:'https://auth.example',client_id:'cloud-login',identity_resource:'https://pki.example/identity',identity_endpoint:'https://identity.example/v1/identity/certificate',cloud_resource:'https://cloud.example/api',cloud_grpc:'api.example:443',relay_issuer:'https://api.example'};
assert.match((await worker.call('init',{...settings,auth_base:'http://auth.example'})).error,/HTTPS/,'init refuses settings that fail Validate');
assert.equal((await worker.call('init',settings)).result,true);
assert.match((await worker.call('init',settings)).error,/already initialized/,'the services cannot be changed once set');
assert.equal((await worker.call('credential',{tenant:'tenant-a'})).error,'Sign in with Wendy first');
assert.equal((await worker.call('sign',{tenant:'tenant-a',bytes:new Uint8Array([1,2,3])})).error,'Sign in with Wendy first');
assert.equal((await worker.call('sign',{tenant:'tenant-a'})).error,'Nothing to sign','bytes cross only as a Uint8Array');
assert.equal((await worker.call('sign',{tenant:'tenant-a',bytes:'AQID'})).error,'Nothing to sign');
assert.equal((await worker.call('auth-restore',{tenant:'tenant-a'})).result,null);
assert.equal((await worker.call('auth-signout',{})).result,true);
console.log('PASS: wasm sha256 pin, sibling paths, init validation and once-only, credential/sign refuse without sign-in, Uint8Array-only bytes');
process.exit(0);
