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

