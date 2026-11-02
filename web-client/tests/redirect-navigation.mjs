// Optional browser test after build:wasm. No live authentication is performed.
import {createRequire} from 'node:module';
import {fileURLToPath} from 'node:url';
const require=createRequire(import.meta.url);
const {chromium,webkit,devices}=require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const crypto=require('node:crypto');
const zlib=require('node:zlib');
const pub=fileURLToPath(new URL('../public/',import.meta.url));
const pin=crypto.createHash('sha256').update(zlib.gunzipSync(fs.readFileSync(pub+'wendy.wasm.gz'))).digest('hex');
(async()=>{
 const engine=process.argv[2]||'webkit',mobile=process.argv.includes('--mobile');
 const browser=await(engine==='webkit'?webkit.launch({headless:true,...(process.env.WEBKIT_EXECUTABLE?{executablePath:process.env.WEBKIT_EXECUTABLE}:{})}):chromium.launch({headless:true,channel:'chrome'}));
 try{for(const origin of ['https://cloud.dev.wendy.sh','https://cloud.wendy.dev']){
  const auth=origin.includes('.dev.')?'https://auth.dev.wendy.sh':'https://auth.wendy.dev';
  const settings={auth_base:auth,client_id:'cloud-login',identity_resource:'https://pki.wendy.sh/identity',identity_endpoint:'https://identity.dev.pki.wendy.sh/v1/identity/certificate',cloud_resource:origin+'/api',cloud_grpc:'api.dev.wendy.sh:443',relay_issuer:'https://api.dev.wendy.sh'};
  const context=await browser.newContext(mobile?devices['iPhone 13']:{});
  const page=await context.newPage();page.setDefaultTimeout(30000);let closed=false;let network=[];
  const html='<!doctype html><title>Redirect implementation probe</title><script src="/probe.js"></script>';
  const bootstrap=`
  window.boot=async()=>{const worker=new Worker('/wendy-web/wendy-worker.js?sha256=${pin}');let seq=0;const pending=new Map();window.worker=worker;window.call=(method,params)=>new Promise((resolve,reject)=>{const id=++seq;pending.set(id,{resolve,reject});worker.postMessage({id,method,params})});await new Promise((resolve,reject)=>{worker.onmessage=({data})=>{if(data.ready)resolve();else if(data.fatal)reject(Error(data.fatal));else if(pending.has(data.id)){const p=pending.get(data.id);pending.delete(data.id);p.resolve(data)}}});return call('init',${JSON.stringify(settings)})};window.documentID=crypto.randomUUID();`;
  // Do not intercept blob: URLs: WebKit uses them internally for Blob.stream.
  await context.route('https://**/*',async route=>{
    const u=new URL(route.request().url());network.push(u.origin+u.pathname);
    if(u.origin===origin&&u.pathname.startsWith('/wendy-web/')){const name=u.pathname.split('/').pop();return route.fulfill({status:200,headers:{'content-type':name.endsWith('.js')?'text/javascript':'application/gzip','cross-origin-resource-policy':'same-origin',},body:fs.readFileSync(pub+name)})}
    if(u.origin===origin&&u.pathname==='/probe.js')return route.fulfill({contentType:'text/javascript',body:bootstrap});
    if(u.origin===origin)return route.fulfill({status:200,headers:{'content-type':'text/html','cross-origin-opener-policy':'same-origin-allow-popups','content-security-policy':"default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",'x-content-type-options':'nosniff','referrer-policy':'no-referrer','cache-control':'no-store'},body:html});
    if(u.origin===auth&&u.pathname==='/authorize')return route.fulfill({status:200,headers:{'content-type':'text/html','cache-control':'no-store'},body:'<!doctype html><title>Synthetic OAuth cancellation</title>'});
    throw Error('Unexpected network request '+u.origin+u.pathname);
  });
  try{
   await page.goto(origin+'/probe');
   assert.equal((await page.evaluate(()=>Promise.race([boot(),new Promise((_,reject)=>setTimeout(()=>reject(Error('Worker boot timed out')),20000))]))).result,true);
   const oldDocument=await page.evaluate(()=>documentID);
   const oldWorkers=page.workers();assert.equal(oldWorkers.length,1);oldWorkers[0].on('close',()=>closed=true);
   const begin=await page.evaluate(async()=>{const r=await call('auth-redirect-begin',{redirect:location.origin+'/auth/callback'});sessionStorage.setItem('pending',r.result.pending);return r});
   assert.deepEqual(Object.keys(begin.result).sort(),['pending','url']);
   const wrapped=await page.evaluate(async handle=>{
    const db=await new Promise((resolve,reject)=>{const r=indexedDB.open('wendy-auth-pending',1);r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});
    try {const record=await new Promise((resolve,reject)=>{const r=db.transaction('pending').objectStore('pending').get(handle);r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});
     let exportRefused=false;try{await crypto.subtle.exportKey('raw',record.wrappingKey)}catch{exportRefused=true}
     return {extractable:record.wrappingKey.extractable,exportRefused,fields:Object.keys(record).sort()};
    } finally {db.close()}
   },begin.result.pending);
   assert.equal(wrapped.extractable,false);assert.equal(wrapped.exportRefused,true);assert.deepEqual(wrapped.fields,['created','iv','wrapped','wrappingKey']);
   const state=new URL(begin.result.url).searchParams.get('state');
   await Promise.all([page.waitForURL(auth+'/**'),page.evaluate(url=>location.assign(url),begin.result.url)]);
   const callback=origin+'/auth/callback?error=access_denied&state='+encodeURIComponent(state);
   await Promise.all([page.waitForURL(origin+'/auth/callback**'),page.evaluate(url=>location.assign(url),callback)]);
   assert.equal((await page.evaluate(()=>Promise.race([boot(),new Promise((_,reject)=>setTimeout(()=>reject(Error('Worker boot timed out')),20000))]))).result,true);
   assert.notEqual(await page.evaluate(()=>documentID),oldDocument);
   const result=await page.evaluate(async()=>{
    const query=new URL(location.href).searchParams;const pending=sessionStorage.getItem('pending');
    const cb={pending,redirect:location.origin+location.pathname,state:query.get('state'),code:'',issuer:'',error:query.get('error')};
    const first=await call('auth-redirect-complete',cb);const replay=await call('auth-redirect-complete',cb);sessionStorage.removeItem('pending');
    return {pending,first,replay};
   });
   assert.equal(result.pending,begin.result.pending);assert.equal(result.first.error,'Authentication failed: access_denied');assert.match(result.replay.error,/missing or already used/);assert.equal(closed,true);
   assert.equal(network.some(x=>x.includes('/oauth2/token')||x.includes('/identity/certificate')),false);
   console.log(JSON.stringify({engine,mobile,origin,browser:browser.version(),passed:true,wasmSHA256:pin,oldWorkerClosed:closed,oldDocumentReplaced:true,wrappingKeyNonExtractable:true,newWorkerCount:page.workers().length,handlePreserved:true,errorCausePreserved:true,replayRefused:true,tokenOrCertificateRequests:0,networkMode:'browser-routed synthetic OAuth pages; raw gzip; actual Cloud COOP header; real built WASM and full top-level navigation'}));
  }catch(e){console.log(JSON.stringify({engine,mobile,origin,passed:false,error:e.stack}));process.exitCode=1}
  finally{await context.close()}
 }}finally{await browser.close()}
})().catch(e=>{console.error(e);process.exitCode=1});
