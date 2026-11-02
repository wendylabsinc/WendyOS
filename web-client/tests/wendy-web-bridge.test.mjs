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

function startWorker(query, overrides={}){
  const messages=[];let waiting=[];
  const scope={indexedDB,crypto,TextEncoder,TextDecoder,Uint8Array,ArrayBuffer,URL,Blob,Response,DecompressionStream,WebAssembly,performance,console,setTimeout,clearTimeout,
    location:new URL(`https://console.example/wendy-web/1/wendy-worker.js${query}`),
    fetch:async(url)=>{assert.equal(String(url),'https://console.example/wendy-web/1/wendy.wasm.gz','the module loads next to the worker');return new Response(gz);},
    importScripts:(...names)=>{for(const name of names)vm.runInContext(readFileSync(pub(name),'utf8'),scope);},
    postMessage:(data)=>{messages.push(data);for(const check of waiting)check();},
  };
  Object.assign(scope,overrides);
  scope.self=scope;scope.globalThis=scope;
  vm.createContext(scope);
  vm.runInContext(readFileSync(pub('wendy-worker.js'),'utf8'),scope);
  const next=(match)=>new Promise((resolve)=>{const check=()=>{const found=messages.find(match);if(found){waiting=waiting.filter((c)=>c!==check);resolve(found);}};waiting.push(check);check();});
  let id=0;
  const call=(method,params)=>{const request={id:++id,method,params};scope.onmessage({data:request});return next((m)=>m.id===request.id);};
  return {next,call,scope,messages};
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
const begun=await worker.call('auth-redirect-begin',{redirect:'https://console.example/auth/callback'});
assert.deepEqual(Object.keys(begun.result).sort(),['pending','url']);
assert.match(begun.result.pending,/^[A-Za-z0-9_-]{43}$/);
const authorize=new URL(begun.result.url);
assert.equal(authorize.origin,'https://auth.example');
assert.equal(authorize.searchParams.get('code_challenge_method'),'S256');
assert.equal(authorize.searchParams.has('dpop_jkt'),false);
assert.equal(authorize.searchParams.has('code_verifier'),false);
const replacement=startWorker('?sha256='+pin);
assert.ok((await replacement.next(m=>m.fatal||m.ready)).ready);
await replacement.call('init',settings);
const callback={pending:begun.result.pending,redirect:'https://console.example/auth/callback',state:authorize.searchParams.get('state'),code:'',issuer:'',error:'invalid_scope'};
assert.equal((await replacement.call('auth-redirect-complete',callback)).error,'Authentication failed: invalid_scope','fresh WASM resumes and preserves an OAuth error without iss');
assert.match((await worker.call('auth-redirect-complete',callback)).error,/missing or already used/,'original worker cannot replay a consumed transaction');
const canceled=await worker.call('auth-redirect-begin',{redirect:callback.redirect});
assert.equal((await replacement.call('auth-redirect-cancel',{pending:canceled.result.pending})).result,true);
assert.match((await worker.call('auth-redirect-complete',{...callback,pending:canceled.result.pending})).error,/missing or already used/);
const invalid=await worker.call('auth-redirect-begin',{redirect:callback.redirect});
assert.match((await replacement.call('auth-redirect-complete',{...callback,pending:invalid.result.pending,state:'wrong'})).error,/state mismatch/);
assert.match((await replacement.call('auth-redirect-complete',{...callback,pending:invalid.result.pending})).error,/missing or already used/);
const blocked=startWorker('?sha256='+pin,{indexedDB:{open(){const request={};setTimeout(()=>request.onblocked(),0);return request;}}});
assert.ok((await blocked.next(m=>m.fatal||m.ready)).ready);
await blocked.call('init',settings);
const refused=await blocked.call('auth-redirect-begin',{redirect:callback.redirect});
assert.match(refused.error,/storage is blocked/);
assert.equal(refused.result,undefined,'blocked storage cannot return a navigation URL');
assert.equal(typeof (await blocked.call('auth-begin',{redirect:callback.redirect})).result,'string','legacy begin works without pending storage');
assert.match((await blocked.call('auth-restore',{tenant:'tenant-a'})).error,/could not access credential storage: Browser storage is blocked/,'credential failures carry their cause');
// A cached wendy-credentials.js from before redirects has no wendyPending.
const stale=startWorker('?sha256='+pin);
assert.ok((await stale.next(m=>m.fatal||m.ready)).ready);
await stale.call('init',settings);
vm.runInContext('delete globalThis.wendyPending',stale.scope);
assert.equal((await stale.call('auth-redirect-begin',{redirect:callback.redirect})).error,'Could not save pending sign-in: Browser pending sign-in storage is unavailable');
console.log('PASS: WASM pin/loading, init, unsigned credential/sign refusal, redirect begin/resume/error/replay/cancel/state/storage refusal, store-specific storage errors, legacy begin');
process.exit(0);
