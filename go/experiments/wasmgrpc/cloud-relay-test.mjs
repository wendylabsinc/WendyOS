// Run browserauth's opt-in live relay test under Go WASM and browser WebSocket.
// GOOS=js GOARCH=wasm go test -c -o /tmp/wendy-browserauth.test.wasm ./go/internal/cli/browserauth
import {readFileSync} from 'node:fs';
import vm from 'node:vm';
import WebSocket from '../../../web-client/node_modules/ws/wrapper.mjs';
const nodeProcess=process;
globalThis.WebSocket=class extends WebSocket {constructor(url,protocols){super(url,protocols,{origin:'http://localhost:5173'});}};
vm.runInThisContext(readFileSync('web-client/public/wasm_exec.js','utf8'));
const go=new Go();
go.argv=['browserauth.test','-test.run=TestLiveCloudRelay','-test.v'];
const broker = 'wendy-cloud-dev-tunnel-broker-nkohwk7hda-uc.a.run.app:443';
const checkBroker = nodeProcess.argv.includes('--broker');
go.env={
 WENDY_TEST_CLOUD_RELAY: 'ws://127.0.0.1:8788' + (checkBroker ? '/broker?endpoint='+encodeURIComponent(broker) : '/cloud'),
 WENDY_TEST_CLOUD_AUTHORITY: checkBroker ? broker : 'api.dev.wendy.sh:443',
 WENDY_TEST_BROKER_JOIN: checkBroker ? '1' : '0',
};
go.exit=code=>{nodeProcess.exitCode=code;};
globalThis.process=Object.create(globalThis.process);
Object.defineProperty(globalThis.process,"argv0",{value:"browser"});
const {instance}=await WebAssembly.instantiate(readFileSync('/tmp/wendy-browserauth.test.wasm'),go.importObject);
await go.run(instance);

nodeProcess.exit(nodeProcess.exitCode || 0);
