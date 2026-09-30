// A local MCP Apps host for exercising the embedded panel. Bind only to loopback.
// Set WENDY_GATEWAY_URL and WENDY_GATEWAY_TOKEN to use a running HTTP gateway.
// Without them, every response is explicitly a fixture and no device is touched.
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";

const port = Number(process.env.PORT || 8790);
const gatewayURL = process.env.WENDY_GATEWAY_URL;
const gatewayToken = process.env.WENDY_GATEWAY_TOKEN;
if (!!gatewayURL !== !!gatewayToken) throw new Error("Set both gateway URL and token, or neither for fixtures.");
const panelURL = new URL("../go/internal/cli/mcp/desktop_app.html", import.meta.url);
const apps = new Map([["companion", "RUNNING"]]);
const host = `<!doctype html><html><head><meta name="viewport" content="width=device-width"><title>Wendy panel test host</title><style>body{margin:0;background:#e9eee9;font:14px system-ui}header{padding:12px 24px;color:#45534a}iframe{display:block;border:0;background:white;width:100%;max-width:1480px;min-height:820px;margin:0 auto}pre{max-width:920px;margin:16px auto;white-space:pre-wrap;word-break:break-word}</style></head><body><header>${gatewayURL ? "Local host connected to the Wendy gateway" : "Fixture preview. No robot connection."}</header><iframe id="panel" title="Wendy robot panel" src="/panel"></iframe><pre id="context">Model context will appear here.</pre><script>
const frame=document.getElementById('panel');
window.addEventListener('message',async event=>{
 if(event.source!==frame.contentWindow||event.origin!==location.origin)return;
 const m=event.data;if(!m||m.jsonrpc!=='2.0')return;
 let result={};try{
  if(m.method==='ui/initialize')result={protocolVersion:'2026-01-26',hostInfo:{name:'Wendy local test host',version:'1'},hostCapabilities:{serverTools:{},updateModelContext:{},message:{}},hostContext:{theme:matchMedia('(prefers-color-scheme:dark)').matches?'dark':'light'}};
  else if(m.method==='ui/notifications/initialized'){const r=await fetch('/call',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({name:'open_devices',arguments:{}})});frame.contentWindow.postMessage({jsonrpc:'2.0',method:'ui/notifications/tool-result',params:await r.json()},location.origin);return;}
  else if(m.method==='tools/call'){const r=await fetch('/call',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(m.params)});if(!r.ok)throw new Error(await r.text());result=await r.json()}
  else if(m.method==='ui/update-model-context'||m.method==='ui/message'){document.getElementById('context').textContent=JSON.stringify(m.params,(key,value)=>key==='data'?'[image bytes]':value,2)}
  else if(m.method==='ui/notifications/size-changed'){frame.style.height=m.params.height+'px'}
  if(m.id!==undefined)frame.contentWindow.postMessage({jsonrpc:'2.0',id:m.id,result},location.origin);
 }catch(error){if(m.id!==undefined)frame.contentWindow.postMessage({jsonrpc:'2.0',id:m.id,error:{code:-32603,message:error.message}},location.origin)}
});
</script></body></html>`;

async function fixture({ name, arguments: args }) {
  let data;
  switch (name) {
    case "open_devices": case "list_robots": data = {total_count:5,robots:[['go2','Lab Go2','Unitree Go2'],['orin','Vision bench','Jetson Orin Nano'],['dragonwing','Warehouse edge','Dragonwing IQ-9075'],['dgx','Training server','NVIDIA DGX Spark'],['macbook','Development Mac','MacBook']].map(([model,name,device_type])=>({id:model,model,name,device_type,cloud_presence:'online',can_capture:true,can_control_apps:true,can_read_events:true}))};break;
    case "get_device_model": if(!['go2','orin','dragonwing','dgx','macbook'].includes(args.model))throw Error('Unknown model');return {content:[],structuredContent:{model:args.model},_meta:{glb:(await readFile(new URL('../go/internal/cli/mcp/desktop_assets/'+args.model+'.glb',import.meta.url))).toString('base64')}};
    case "read_device_settings":data={values:{show_3d:true,include_offline:false}};break;
    case "update_device_settings":data={values:args.set};break;
    case "list_device_triggers":data={triggers:[{id:'people',name:'Person detected',event:'person_detected',state:'fixture',managed:false,can_configure:false}]};break;
    case "list_device_events":data={status:'observed',events:[{name:'person_detected',observed_at:new Date().toISOString(),attributes:{confidence:0.94},fixture:true}],gap:false};break;
    case "read_device_metrics":data={fixture:true,metrics:[{name:'cpu.utilization',value:23,unit:'%'},{name:'memory.used',value:3.2,unit:'GiB'}]};break;
    case "read_device_logs":data={fixture:true,logs:[{timestamp:new Date().toISOString(),message:'Fixture application ready'}]};break;
    case "inspect_robot": data = {robot_id:"preview",name:"Workshop robot",connected:true,observed_at:new Date().toISOString(),cameras:[{id:0,name:"Front camera"}],apps:[...apps].map(([name,state])=>({name,state,readiness:"unknown"})),warnings:[]}; break;
    case "start_robot_app": case "stop_robot_app": apps.set(args.app_name,name.startsWith("start")?"RUNNING":"STOPPED");data={robot_id:args.robot_id,app_name:args.app_name,state:apps.get(args.app_name),state_verified:true,readiness:"unknown"};break;
    case "capture_robot_image": return {isError:true,content:[{type:"text",text:"Fixture mode has no camera. Connect the preview host to a gateway to capture a real frame."}]};
    default: throw new Error("Unknown fixture tool");
  }
  return {structuredContent:data,content:[{type:"text",text:JSON.stringify(data)}]};
}

const server = createServer(async (req, res) => {
  res.setHeader("Cache-Control", "no-store");
  try {
    if (req.method === "GET" && req.url === "/") {res.setHeader("Content-Type","text/html; charset=utf-8");res.end(host);return;}
    if (req.method === "GET" && req.url === "/panel") {res.setHeader("Content-Type","text/html; charset=utf-8");res.end(await readFile(panelURL));return;}
    if (req.method !== "POST" || req.url !== "/call") {res.writeHead(404);res.end();return;}
    if (req.headers.origin !== `http://127.0.0.1:${port}`) {res.writeHead(403);res.end("Unexpected browser origin");return;}
    let body="";for await (const chunk of req) {body+=chunk;if(body.length>65536)throw new Error("Request too large");}
    const params=JSON.parse(body);
    let result;
    if (gatewayURL) {
      const response=await fetch(gatewayURL,{method:"POST",headers:{Authorization:`Bearer ${gatewayToken}`,"Content-Type":"application/json",Accept:"application/json, text/event-stream","MCP-Protocol-Version":"2025-03-26"},body:JSON.stringify({jsonrpc:"2.0",id:crypto.randomUUID(),method:"tools/call",params})});
      if(!response.ok)throw new Error(`Gateway returned HTTP ${response.status}`);
      const raw=await response.text();
      const payload=JSON.parse(raw.startsWith("event:")||raw.startsWith("data:")?raw.split("\n").find(l=>l.startsWith("data:")).slice(5):raw);
      if(payload.error)throw new Error(payload.error.message);
      result=payload.result;
    } else result=await fixture(params);
    res.setHeader("Content-Type","application/json");res.end(JSON.stringify(result));
  } catch(error) {res.writeHead(500);res.end(error.message);}
});
server.listen(port,"127.0.0.1",()=>console.log(`Wendy panel host: http://127.0.0.1:${port} (${gatewayURL?"gateway":"fixtures"})`));
