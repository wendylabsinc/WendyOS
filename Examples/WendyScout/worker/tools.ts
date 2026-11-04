import {Type} from "@mariozechner/pi-ai";
import {defineTool, type ExtensionAPI} from "@mariozechner/pi-coding-agent";
const url = "http://127.0.0.1:18081";
const version = Type.Union(["past","stable","nightly"].map(x=>Type.Literal(x)));
const baseline = Type.Union([Type.Literal("past"),Type.Literal("stable")]);
const kind = Type.Union(["restart_persistence","reject_extra_args","logs_output"].map(x=>Type.Literal(x)));
async function call(path:string,args:unknown) {
 const r=await fetch(url+path,{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(args),signal:AbortSignal.timeout(120000)});
 const text=await r.text();return {content:[{type:"text" as const,text}],details:{status:r.status},isError:!r.ok};
}
export default function(pi:ExtensionAPI) {
 let calls=0;
 function register(name:string,description:string,parameters:any,path:string,transform:(p:any)=>any=p=>p) {
  pi.registerTool(defineTool({name,label:name,description,parameters,async execute(_id,p){if(++calls>16)throw Error("Session tool budget exhausted");return call(path,transform(p));}}));
 }
 register("scout_experiment","Run one real functional workflow on this Development device and ONLY the fixed owned scratch app/volume. Each invocation resets identical state, starts the fixture, checks readiness and CLI state against the canonical Agent, then cleans up. restart_persistence checks stop/restart and retained start counter; reject_extra_args checks invalid stop arguments cannot change running state; logs_output checks attached and stored readiness output. Returns fresh evidence ID, commands and literal expected/actual checks. No arbitrary arguments or targets.",Type.Object({kind,version},{additionalProperties:false}),"/tool",p=>({operation:"experiment",...p}));
 register("script_write","Write a bounded offline Python analysis experiment. inputs.json contains fresh functional observations and verified CLI paths. No network/device/secrets/packages. Only existing output.txt is writable via r+. Offline output alone cannot establish live behavior or justify publication.",Type.Object({name:Type.String(),text:Type.String()},{additionalProperties:false}),"/script",p=>({action:"write",...p}));
 register("script_run","Execute an existing .py experiment in the kernel sandbox and inspect bounded output. This is optional offline diagnosis; no mandatory help/version/arithmetic ritual.",Type.Object({name:Type.String()},{additionalProperties:false}),"/script",p=>({action:"run",...p}));
 register("scout_report","ONLY after a fresh scout_experiment returned outcome=failed: replay that exact functional workflow twice on baseline/nightly with clean state, contrast current stable, check existing issues and publish only a verified functional regression. Passing, blocked, help-only and invented observations are rejected. No fields, free-form issue text or arbitrary commands.",Type.Object({kind,version:baseline},{additionalProperties:false}),"/tool",p=>({operation:"reproduce",...p}));
}
