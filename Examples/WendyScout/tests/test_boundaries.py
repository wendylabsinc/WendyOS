import copy,http.server,json,pathlib,sys,tempfile,threading,unittest,urllib.request
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).resolve().parents[1]/'control'))
import common,inference,probes,publisher

class InferenceTests(unittest.TestCase):
    def test_text_and_tool_results(self):
        b={'model':'local','messages':[{'role':'user','content':'A URL in inert text: https://example.invalid/'},{'role':'assistant','content':None,'tool_calls':[{'id':'one','type':'function','function':{'name':'script_run','arguments':'{"name":"test.py"}'}}]},{'role':'tool','tool_call_id':'one','content':[{'type':'text','text':'offline result'}]}],'stream':True,'max_tokens':9999}
        self.assertEqual(inference.validate(b,'local')['max_tokens'],1536)
    def test_fetch_and_override_features_rejected(self):
        base={'model':'local','messages':[{'role':'user','content':'hello'}]}
        for key,value in [('image_url','https://example.invalid'),('chat_template_kwargs',{}),('lora','/state/secrets'),('grammar','x'),('response_format',{})]:
            with self.subTest(key=key),self.assertRaises(ValueError):inference.validate({**base,key:value},'local')
        for item in [{'type':'image_url','image_url':{'url':'https://example.invalid'}},{'type':'input_audio','input_audio':{'data':'x'}}]:
            with self.assertRaises(ValueError):inference.validate({**base,'messages':[{'role':'user','content':[item]}]},'local')
        with self.assertRaises(ValueError):inference.validate({**base,'tools':[{'type':'function','function':{'name':'bash','parameters':{}}}]},'local')

class ProjectionTests(unittest.TestCase):
    def test_private_values_excluded_and_versions_retained(self):
        raw={'version':'2026.10.29-210857','osVersion':'WendyOS-0.17.0','cpuArchitecture':'arm64','stable_id':'by-id:usb-Test_TESTSERIAL-video-index0','ip_addresses':['2001:db8::1'],'primary_mac':'02:00:00:00:00:01','token':'github'+'_pat_'+'A'*40}
        out=common.public_observation('info',raw,0)
        self.assertEqual(out,{'version':raw['version'],'osVersion':raw['osVersion'],'cpuArchitecture':'arm64'})
        self.assertEqual(common.public_observation('cameras',[raw],0),{'count':1})
    def test_raw_error_not_published(self):
        text='by-id:usb-Test_TESTSERIAL-video-index0 2001:db8::1 02:00:00:00:00:01 https://example.invalid/ @someone '+('github'+'_pat_'+'A'*40)
        cleaned=common.scrub(text)
        for sensitive in ['TESTSERIAL','2001:db8','02:00:00','https://','@someone','A'*40]:self.assertNotIn(sensitive,cleaned)
        out=common.public_observation('audio',{'error':{'code':'error','message':text}},1)
        self.assertNotIn('TESTSERIAL',json.dumps(out))
    def test_null_issue_body(self):
        with patch.object(common,'api',side_effect=[[{'sha':'abc','commit':{'message':'subject'}}],[{'number':1,'title':'title','state':'closed','body':None}]]):
            self.assertEqual(common.context()['issues'][0]['body'],'')

class ReplayTests(unittest.TestCase):
    def instance(self,results,new_stable=False):
        p=probes.Probes.__new__(probes.Probes);p.agent='test-agent';p.versions={'past':{'published_at':'2026-10-01'},'nightly':{'published_at':'2026-11-01'},'stable':{'published_at':'2026-11-02' if new_stable else '2026-10-20'}}
        p.run=lambda *args:copy.deepcopy(results.pop(0));return p
    def r(self,exit=0,output=None):return {'exit':exit,'output':{} if output is None else output,'version':'2026.10.29-210857','kind':'info','failure_identity':'test'}
    def test_removed_command_after_working_baseline_is_regression(self):
        values=[self.r(),self.r(2,{'error_class':'unknown-command'}),self.r(),self.r(2,{'error_class':'unknown-command'}),self.r(),self.r()]
        self.assertTrue(self.instance(values).replay('info','past','nightly',None)['verified'])
    def test_already_fixed_current_stable_not_published(self):
        values=[self.r(),self.r(1),self.r(),self.r(1),self.r(),self.r()]
        d=self.instance(values,True).replay('info','past','nightly',None)
        self.assertFalse(d['verified']);self.assertTrue(d['already_fixed_in_newer_stable'])
    def test_missing_baseline_field_is_unverified(self):
        d=self.instance([self.r() for _ in range(4)]).replay('info','past','nightly',None,'field_changed','osVersion')
        self.assertFalse(d['verified'])
    def test_shared_failures_are_not_regression(self):
        self.assertFalse(self.instance([self.r(1) for _ in range(4)]).replay('info','past','nightly',None)['verified'])
    def test_targets_commands_and_fields_are_not_freeform(self):
        p=self.instance([])
        for recipe,version,predicate,field in [('shell','past','exit',''),('info','prod','exit',''),('info','past','field_changed','/state/secrets'),('help_root','past','help_flag_missing','--device prod')]:
            with self.assertRaises(ValueError):p.replay(recipe,version,'nightly',None,predicate,field)

class FakeGitHub(http.server.BaseHTTPRequestHandler):
    calls=[];issues=[];label=False
    def log_message(self,*_):pass
    def do_GET(self):
        self.__class__.calls.append(('GET',self.path))
        if '/labels/' in self.path and not self.__class__.label:self.send_error(404);return
        body=json.dumps(self.__class__.issues if '/issues?' in self.path else {'name':'ai-found'}).encode();self.send_response(200);self.end_headers();self.wfile.write(body)
    def do_POST(self):
        data=json.loads(self.rfile.read(int(self.headers['Content-Length'])));self.__class__.calls.append(('POST',self.path,data))
        if self.path.endswith('/labels'):self.__class__.label=True;out={'name':'ai-found'}
        elif self.path.endswith('/issues'):
            out={**data,'html_url':'https://github.com/example/test/issues/1'};self.__class__.issues.append(out)
        else:self.send_error(403);return
        self.send_response(201);self.end_headers();self.wfile.write(json.dumps(out).encode())
class FakeTransport:
    def __init__(self,url):self.url=url
    def request(self,method,suffix,data=None):
        # Test transport is injected only here; production base URL is immutable.
        req=urllib.request.Request(self.url+'/repos/example/test/'+suffix,method=method,data=json.dumps(data).encode() if data is not None else None,headers={'Content-Type':'application/json'})
        with urllib.request.urlopen(req,timeout=2) as r:return json.load(r)
class PublisherTests(unittest.TestCase):
    def setUp(self):
        FakeGitHub.calls=[];FakeGitHub.issues=[];FakeGitHub.label=False
        self.server=http.server.ThreadingHTTPServer(('127.0.0.1',0),FakeGitHub);threading.Thread(target=self.server.serve_forever,daemon=True).start()
        self.temp=tempfile.TemporaryDirectory();self.pub=publisher.Publisher(self.temp.name,repo='example/test',transport=FakeTransport('http://127.0.0.1:'+str(self.server.server_port)))
    def tearDown(self):self.server.shutdown();self.server.server_close();self.temp.cleanup()
    def finding(self,field='osVersion'):
        r={'kind':'info','version':'2026.10.29-210857','command':['wendy','--json','device','info'],'exit':0,'output':{'osVersion':'WendyOS-0.17.0'},'failure_identity':field}
        return {'verified':True,'kind':'field_changed-regression','field':field,'probe':'info','agent':'2026.10.29-210857','results':[copy.deepcopy(r) for _ in range(4)]}
    def test_local_fake_publication_label_and_duplicate(self):
        d=self.pub.publish(self.finding());self.assertEqual(d['status'],'published')
        self.assertEqual(self.pub.publish(self.finding())['status'],'duplicate-local')
        posts=[x for x in FakeGitHub.calls if x[0]=='POST'];self.assertEqual([x[1].split('/')[-1] for x in posts],['labels','issues'])
        self.assertEqual(posts[-1][2]['labels'],['ai-found']);self.assertIn('WendyOS-0.17.0',posts[-1][2]['body'])
    def test_closed_duplicate_and_rate_limit(self):
        self.pub.publish(self.finding());self.pub.ledger.unlink()
        FakeGitHub.issues[0]['state']='closed'
        self.assertEqual(self.pub.publish(self.finding())['status'],'duplicate')
        self.pub.publish(self.finding('gpuArch'))
        self.assertEqual(self.pub.publish(self.finding('cpuArchitecture'))['status'],'rate-limited')
    def test_unverified_fixed_and_unapproved_routes(self):
        for d in [{}, {**self.finding(),'already_fixed_in_newer_stable':True}]:
            with self.assertRaises(ValueError):self.pub.publish(d)
        gh=publisher.GitHub('example/test','/nonexistent')
        for method,path in [('POST','issues/1/comments'),('PUT','contents/file'),('POST','actions/workflows/run'),('POST','../other/issues')]:
            with self.assertRaises(ValueError):gh.request(method,path,{})

if __name__=='__main__':unittest.main()
