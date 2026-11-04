import {Type} from "@mariozechner/pi-ai";
import {defineTool, type ExtensionAPI} from "@mariozechner/pi-coding-agent";
const url = "http://127.0.0.1:18081";
const version = Type.Union([Type.Literal("past"),Type.Literal("stable"),Type.Literal("nightly")]);
const baseline = Type.Union([Type.Literal("past"),Type.Literal("stable")]);
const kind = Type.Union(["info","cameras","audio","apps","help_root","help_run","help_device","scratch_start","scratch_stop","scratch_remove","scratch_lifecycle"].map(x=>Type.Literal(x)));
async function call(path:string,args:unknown) {
 const r=await fetch(url+path,{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(args),signal:AbortSignal.timeout(120000)});
 const text=await r.text();return {content:[{type:"text" as const,text}],details:{status:r.status},isError:!r.ok};
}
export default function(pi:ExtensionAPI) {
 let calls=0;
 function register(name:string,description:string,parameters:any,path:string,transform:(p:any)=>any=p=>p) {
  pi.registerTool(defineTool({name,label:name,description,parameters,async execute(_id,p){if(++calls>16)throw Error("Session tool budget exhausted");return call(path,transform(p));}}));
 }
 register("scout_probe","Fixed local-self CLI probe or create the one test-owned scratch app. No other targets/commands.",Type.Object({operation:Type.Union([Type.Literal("probe"),Type.Literal("scratch_create")]),version:Type.Optional(version),kind:Type.Optional(kind)},{additionalProperties:false}),"/tool");
 register("script_write","Write a small Python experiment. Offline sandbox only; inputs.json has fresh challenge and recent probe observations. No network, secrets, device access or package installation. Only precreated output.txt is writable via r+.",Type.Object({name:Type.String(),text:Type.String()},{additionalProperties:false}),"/script",p=>({action:"write",...p}));
 register("script_run","Execute a written Python experiment with kernel isolation. Available offline CLIs: /opt/scout-offline/<exact-version>/wendy. Print or write output.txt with r+. Results are offline evidence, not live-device observations.",Type.Object({name:Type.String()},{additionalProperties:false}),"/script",p=>({action:"run",...p}));
 register("scout_report","Replay a suspected compatibility failure twice on both versions; trusted harness validates evidence, checks current stable and existing issues, then creates an issue only for a verified finding. No arbitrary issue text.",Type.Object({kind,version:baseline,predicate:Type.Union(["exit","invalid_json","field_missing","field_changed","help_flag_missing"].map(x=>Type.Literal(x))),field:Type.Optional(Type.String())},{additionalProperties:false}),"/tool",p=>({operation:"reproduce",...p}));
}
