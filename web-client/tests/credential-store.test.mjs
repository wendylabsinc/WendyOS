import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';
import {indexedDB} from 'fake-indexeddb';
const source=readFileSync(new URL('../public/wendy-credentials.js',import.meta.url),'utf8');
function workerStore(){const scope={self:{},indexedDB,crypto,TextEncoder,Uint8Array};vm.runInNewContext(source,scope);return scope.self.wendyCredentials;}
const aad='operator-key:tenant:operator';
const seed=()=>new Uint8Array(32).fill(7);
const first=workerStore();
assert.equal(await first.load(),'');
const saved=JSON.stringify({version:2,certificate:'test-public-certificate',refreshToken:'test-only-refresh'});
await first.saveWrapped(saved,seed(),aad);
const reopened=workerStore();
assert.equal(await reopened.load(),saved,'credentials survive a new worker');
assert.deepEqual([...await reopened.loadKey(aad)],[...seed()],'the seed unwraps in a new worker');
await assert.rejects(reopened.loadKey('operator-key:tenant:another'),'a seed does not unwrap for another operator');
await reopened.saveWrapped('rotated-token',seed(),aad);
assert.equal(await first.load(),'rotated-token','existing workers see committed token rotation');
await reopened.clear();
assert.equal(await workerStore().load(),'','sign-out removes the saved session');
console.log('PASS: IndexedDB wrapped save, new-worker restore and unwrap, token replacement, sign-out deletion');

// One saved sign-in per organization, keyed (tenant, subject) like the aad.
const session=(tenant,subject)=>JSON.stringify({version:2,profile:{tenant,subject}});
const a=session('tenant-a','op'),b=session('tenant-b','op');
const orgs=workerStore();
await orgs.saveWrapped(a,seed(),'operator-key:tenant-a:op');
await orgs.saveWrapped(b,new Uint8Array(32).fill(9),'operator-key:tenant-b:op');
assert.equal(await workerStore().load(),b,'a new worker restores the most recent organization');
assert.equal(await orgs.select('tenant-a'),true);
assert.equal(await orgs.load(),a,'selecting an organization loads its slot');
assert.deepEqual([...await orgs.loadKey('operator-key:tenant-b:op')],[...new Uint8Array(32).fill(9)],'each organization keeps its own key');
assert.equal(await workerStore().load(),a,'the selected organization becomes the most recent');
assert.equal(await orgs.select('tenant-c'),false,'an organization never signed in to has no slot');
assert.equal(await orgs.load(),'');
assert.equal(await workerStore().load(),a,'selecting a missing slot keeps the last sign-in');
await orgs.forget('tenant-a');
assert.equal(await orgs.select('tenant-a'),false,'signing out of one organization removes it');
assert.equal(await orgs.select('tenant-b'),true,'and keeps the others');
await orgs.forget();
assert.equal(await orgs.select('tenant-b'),false,'signing out of everything removes every slot');
assert.equal(await workerStore().load(),'');

// The single slot from before moves to its organization's slot.
const legacy=session('tenant-l','op');
await new Promise((resolve,reject)=>{const r=indexedDB.open('wendy-client',1);r.onsuccess=()=>{const tx=r.result.transaction('credentials','readwrite');tx.objectStore('credentials').put({session:legacy},'session');tx.oncomplete=resolve;tx.onerror=reject;};});
const migrated=workerStore();
assert.equal(await migrated.load(),legacy,'the single-slot sign-in is restored');
assert.equal(migrated.slot(),'tenant-l:op','into its organization slot');
assert.equal(await migrated.select('tenant-l','op'),true);
console.log('PASS: per-organization slots, most-recent restore, per-org and full sign-out, single-slot migration');


// Redirect transactions stay separate from credential slots and survive worker
// replacement. These tests exercise actual IndexedDB transactions and WebCrypto.
const {test} = await import('node:test');
const {IDBFactory} = await import('fake-indexeddb');
function pendingWorker(db, overrides={}) {
  const scope={self:{},indexedDB:db,crypto,TextEncoder,Uint8Array,ArrayBuffer,setTimeout,clearTimeout,...overrides};
  vm.runInNewContext(source,scope);
  return scope.self;
}
const handle=(n)=>String(n).padStart(43,'A');
const plaintext=new TextEncoder().encode('{"version":1,"verifier":"private PKCE test data"}');
async function pendingDB(db,operation) {
  const connection=await new Promise((resolve,reject)=>{const r=db.open('wendy-auth-pending',1);r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error);});
  try {return await new Promise((resolve,reject)=>{const tx=connection.transaction('pending','readwrite');const r=operation(tx.objectStore('pending'));tx.oncomplete=()=>resolve(r.result);tx.onerror=()=>reject(tx.error);});}
  finally {connection.close();}
}

test('redirect: recreated worker unwraps once with a non-extractable wrapping key',async()=>{
  const db=new IDBFactory();
  await pendingWorker(db).wendyPending.put(handle(1),plaintext);
  const record=await pendingDB(db,s=>s.get(handle(1)));
  assert.equal(record.wrappingKey.extractable,false);
  await assert.rejects(crypto.subtle.exportKey('raw',record.wrappingKey));
  assert.equal(Buffer.from(record.wrapped).includes(Buffer.from(plaintext)),false);
  assert.deepEqual(Object.keys(record).sort(),['created','iv','wrapped','wrappingKey']);
  const replacement=pendingWorker(db).wendyPending;
  assert.deepEqual(await replacement.take(handle(1)),plaintext);
  assert.equal(await replacement.take(handle(1)),undefined);
});

test('redirect: concurrent workers take exactly once',async()=>{
  const db=new IDBFactory();
  await pendingWorker(db).wendyPending.put(handle(1),plaintext);
  const outcomes=await Promise.all(Array.from({length:12},()=>pendingWorker(db).wendyPending.take(handle(1))));
  assert.equal(outcomes.filter(x=>x!==undefined).length,1);
  assert.deepEqual(outcomes.find(x=>x!==undefined),plaintext);
});

test('redirect: cancel and cleanup do not touch credential slots or other attempts',async()=>{
  const db=new IDBFactory();
  const {wendyPending:pending,wendyCredentials:credentials}=pendingWorker(db);
  await credentials.saveWrapped(session('tenant','op'),seed(),'operator-key:tenant:op');
  const saved=await credentials.load();
  await pending.put(handle(1),plaintext);
  await pending.put(handle(2),plaintext);
  await pending.delete(handle(1));
  assert.equal(await pending.take(handle(1)),undefined);
  assert.deepEqual(await pending.take(handle(2)),plaintext);
  assert.equal(await credentials.load(),saved);
  assert.deepEqual(await credentials.loadKey('operator-key:tenant:op'),seed());
  assert.equal(credentials.slot(),'tenant:op');
  await pending.put(handle(3),plaintext);
  await credentials.forget();
  assert.deepEqual(await pending.take(handle(3)),plaintext,'credential signout cannot delete pending database');
});

test('redirect: expiry and future records are cleaned on the next operation',async()=>{
  const db=new IDBFactory();
  const pending=pendingWorker(db).wendyPending;
  await pending.put(handle(1),plaintext);
  await pending.put(handle(2),plaintext);
  const first=await pendingDB(db,s=>s.get(handle(1)));
  const second=await pendingDB(db,s=>s.get(handle(2)));
  await pendingDB(db,s=>s.put({...first,created:Date.now()-600001},handle(1)));
  await pendingDB(db,s=>s.put({...second,created:Date.now()+60000},handle(2)));
  await pending.put(handle(3),plaintext);
  assert.deepEqual(await pendingDB(db,s=>s.getAllKeys()),[handle(3)]);
  assert.equal(await pending.take(handle(1)),undefined);
});

test('redirect: tampering and swapping records fail after burning them',async()=>{
  const db=new IDBFactory();
  const pending=pendingWorker(db).wendyPending;
  await pending.put(handle(1),plaintext);
  const record=await pendingDB(db,s=>s.get(handle(1)));
  await pendingDB(db,s=>s.put(record,handle(2)));
  await assert.rejects(pending.take(handle(2)));
  assert.equal(await pending.take(handle(2)),undefined);
  new Uint8Array(record.wrapped)[0]^=1;
  await pendingDB(db,s=>s.put(record,handle(1)));
  await assert.rejects(pending.take(handle(1)));
  assert.equal(await pending.take(handle(1)),undefined);
});

test('redirect: size, handle and collision bounds refuse without overwriting',async()=>{
  const db=new IDBFactory();
  const pending=pendingWorker(db).wendyPending;
  await assert.rejects(pending.put('bad',plaintext),/handle/);
  await assert.rejects(pending.put(handle(1),new Uint8Array(8193)),/size/);
  await assert.rejects(pending.put(handle(1),new Uint8Array()),/size/);
  await pending.put(handle(1),plaintext);
  await assert.rejects(pending.put(handle(1),new Uint8Array([1])));
  assert.deepEqual(await pending.take(handle(1)),plaintext);
});

test('redirect: past 32 live attempts the oldest makes room instead of locking out the origin',async()=>{
  const db=new IDBFactory();
  let now=Date.now();
  const pending=pendingWorker(db,{Date:{now:()=>now}}).wendyPending;
  for (let i=0;i<32;i++) {await pending.put(handle(i),plaintext);now++;}
  await pending.put(handle(32),plaintext);
  const keys=await pendingDB(db,s=>s.getAllKeys());
  assert.equal(keys.length,32);
  assert.equal(keys.includes(handle(0)),false,'oldest attempt was evicted');
  assert.equal(await pending.take(handle(0)),undefined);
  // A refused collision rolls back with its eviction.
  await assert.rejects(pending.put(handle(32),plaintext));
  assert.deepEqual(await pending.take(handle(1)),plaintext,'collision evicted nothing');
  assert.deepEqual(await pending.take(handle(32)),plaintext);
});

test('redirect: blocked storage rejects before a record can be used',async()=>{
  const blocked={open(){const r={};setTimeout(()=>r.onblocked(),0);return r;}};
  const pending=pendingWorker(blocked).wendyPending;
  await assert.rejects(pending.put(handle(1),plaintext),/blocked/);
  await assert.rejects(pending.take(handle(1)),/blocked/);
  await assert.rejects(pending.delete(handle(1)),/blocked/);
});

test('redirect: aborted take cannot return plaintext',async()=>{
  const db=new IDBFactory();
  const pending=pendingWorker(db).wendyPending;
  await pending.put(handle(1),plaintext);
  const broken={open(...args){const r=db.open(...args);r.addEventListener('success',()=>{
    const connection=r.result;
    const original=connection.transaction.bind(connection);
    connection.transaction=(...txArgs)=>{const tx=original(...txArgs);queueMicrotask(()=>tx.abort());return tx;};
  });return r;}};
  await assert.rejects(pendingWorker(broken).wendyPending.take(handle(1)));
  assert.deepEqual(await pending.take(handle(1)),plaintext,'aborted deletion never consumed or returned the record');
});
