import test from "node:test";
import assert from "node:assert/strict";
import { WendyClient } from "../lib/client.ts";

test("topology alone has a phase-sized worker ceiling", async (t) => {
 t.mock.timers.enable({ apis: ["setTimeout"] });
 const original=globalThis.Worker;let worker;
 globalThis.Worker=class { constructor(){worker=this;queueMicrotask(()=>this.onmessage({data:{ready:true}}));} postMessage(message){this.last=message;} terminate(){} };
 t.after(()=>{if(original===undefined)delete globalThis.Worker;else globalThis.Worker=original;});
 const client=new WendyClient();
 const ordinary=client.call("device-status");const ordinaryFailure=assert.rejects(ordinary,/did not respond/);
 const topology=client.call("mesh-topology");let complete=false;topology.then(()=>{complete=true});
 await new Promise(resolve=>queueMicrotask(resolve));
 t.mock.timers.tick(40001);await ordinaryFailure;assert.equal(complete,false);
 t.mock.timers.tick(100000);assert.equal(complete,false);
 worker.onmessage({data:{id:2,result:{status:"ok"}}});assert.deepEqual(await topology,{status:"ok"});client.dispose();
});
