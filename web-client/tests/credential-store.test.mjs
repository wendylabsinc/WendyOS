import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';
import {indexedDB} from 'fake-indexeddb';
const source=readFileSync(new URL('../public/wendy-credentials.js',import.meta.url),'utf8');
function workerStore(){const scope={self:{},indexedDB};vm.runInNewContext(source,scope);return scope.self.wendyCredentials;}
const first=workerStore();
assert.equal(await first.load(),'');
const saved=JSON.stringify({certificate:'test-public-certificate',privateKey:'test-only-key',refreshToken:'test-only-refresh'});
await first.save(saved);
const reopened=workerStore();
assert.equal(await reopened.load(),saved,'credentials survive a new worker');
await reopened.save('rotated-token');
assert.equal(await first.load(),'rotated-token','existing workers see committed token rotation');
await reopened.clear();
assert.equal(await workerStore().load(),'','sign-out removes the saved session');
console.log('PASS: IndexedDB save, new-worker restore, token replacement, sign-out deletion');
