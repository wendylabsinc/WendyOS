import http.server, json, os, pathlib, signal, socket, subprocess, tempfile, threading, time, urllib.request
import argparse
parser = argparse.ArgumentParser(description='Exercise chat profiles, local subagents, and persistent A2A services against a mock model.')
parser.add_argument('--wendy', required=True, help='Path to the built Wendy CLI')
binary = str(pathlib.Path(parser.parse_args().wendy).resolve())
requests = []
class Model(http.server.BaseHTTPRequestHandler):
 def do_POST(self):
  data = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
  requests.append(data)
  messages = data['messages']
  text = 'Mock model verified the task.'
  user = next((m['content'] for m in messages if m['role'] == 'user'), '')
  if user == 'delegate both' and not any(m['role'] == 'tool' for m in messages):
   message = {'role':'assistant','tool_calls':[{'id':'batch','type':'function','function':{'name':'agent_delegate','arguments':json.dumps({'tasks':[{'profile':'developer','prompt':'child build'},{'profile':'debugger','prompt':'child debug'}]})}}]}
  else:
   message = {'role':'assistant','content':text}

  if 'tool_calls' in message: message['tool_calls'][0]['index']=0
  body = ('data: '+json.dumps({'choices':[{'delta':message,'finish_reason':'tool_calls' if 'tool_calls' in message else 'stop'}]})+'\n\ndata: [DONE]\n\n').encode()
  self.send_response(200); self.send_header('Content-Type','text/event-stream'); self.send_header('Content-Length',str(len(body))); self.end_headers(); self.wfile.write(body)
 def log_message(self,*args): pass
model = http.server.ThreadingHTTPServer(('127.0.0.1',0), Model)
threading.Thread(target=model.serve_forever,daemon=True).start()
env = dict(os.environ, WENDY_AGENT_TOKEN='test-only-runtime-token-123456', WENDY_ANALYTICS='false')
for key in ['WENDY_CHAT_API_KEY','WENDY_CHAT_PROVIDER','WENDY_CHAT_MODEL','WENDY_CHAT_BASE_URL','OPENAI_BASE_URL']:
 env.pop(key,None)
service = None
with tempfile.TemporaryDirectory(prefix='wendy-agent-smoke-') as temp:
 temp = pathlib.Path(temp)
 def cli(*args):
  p = subprocess.run([binary,*args],env=env,cwd=temp,capture_output=True,text=True,timeout=30)
  assert p.returncode == 0, (args,p.stderr,p.stdout)
  return p.stdout
 profiles = json.loads(cli('chat','--list-profiles','--json'))
 assert len(profiles)==8
 endpoint = f'http://127.0.0.1:{model.server_port}/v1'
 models = temp/'models.json';models.write_text(json.dumps({'debugger':{'provider':'local','model':'child-model','base_url':endpoint}}))
 out = cli('chat','--no-memory','--provider','local','--model','parent-model','--base-url',endpoint,'--agent-models',str(models),'--prompt','delegate both','--json')
 events = [json.loads(line) for line in out.splitlines()]
 assert sum(e['type']=='agent_done' for e in events)==2, events
 assert events[-1]['type']=='done'
 assert any(r['model']=='child-model' for r in requests)
 for r in requests:
  if any(m.get('content') in ['child build','child debug'] for m in r['messages']):
   assert all(t['function']['name'] not in ['agent_delegate','agent_remote'] for t in r['tools'])
 # The developer child inherits the parent's model; these are child requests, not parent tool results.
 assert any(r['model']=='parent-model' and any(m.get('content')=='child build' for m in r['messages']) for r in requests)
 sock=socket.socket();sock.bind(('127.0.0.1',0));port=sock.getsockname()[1];sock.close()
 url=f'http://127.0.0.1:{port}'
 config=temp/'agent.json'
 config.write_text(json.dumps({'name':'smoke','profile':'device-reasoning','workspace':str(temp),'model':{'provider':'local','model':'service-model','base_url':endpoint},'no_memory':True,'allow_tools':[],'triggers':[{'source':'camera','type':'detected','min_confidence':.8,'prompt':'Inspect this event'}]}))
 def start():
  p=subprocess.Popen([binary,'agent','serve','--config',str(config),'--state-dir',str(temp/'state'),'--listen',f'127.0.0.1:{port}'],env=env,cwd=temp,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
  for _ in range(100):
   if p.poll() is not None: raise AssertionError(p.communicate())
   try:
    with urllib.request.urlopen(url+'/.well-known/agent-card.json',timeout=.1) as response:
     card=json.load(response)
    assert card['supportedInterfaces'][0]['protocolVersion']=='1.0'
    return p
   except OSError:time.sleep(.05)
  p.terminate();raise AssertionError('service failed to start')
 try:
  service=start()
  task=json.loads(cli('agent','send','--url',url,'--prompt','inspect','--wait'))
  assert task['status']['state']=='TASK_STATE_COMPLETED',task
  observation=temp/'event.json';observation.write_text(json.dumps({'id':'first','source':'camera','type':'detected','timestamp':time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime()),'confidence':.95}))
  result=json.loads(cli('agent','event','--url',url,'--file',str(observation)))
  assert result['accepted'],result
  service.send_signal(signal.SIGINT);service.wait(timeout=10);service=None
  service=start()
  restored=json.loads(cli('agent','get','--url',url,'--task',task['id']))
  assert restored==task
  duplicate=json.loads(cli('agent','event','--url',url,'--file',str(observation)))
  assert duplicate['task_id']==result['task_id']
  print('PASS: profile listing, headless parallel delegation, per-child model selection, authenticated A2A, sensor events, persisted results, and restart deduplication')
 finally:
  if service is not None:service.send_signal(signal.SIGINT);service.wait(timeout=10)
model.shutdown()
