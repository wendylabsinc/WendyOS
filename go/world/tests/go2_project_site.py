#!/usr/bin/env python3
"""Bounded, read-only integration checks for private World-to-Go2 projections.

Project application/reset belongs to an explicit Wendy simulator operation.
This program has no HTTP client, Docker operation or motion command.
"""
import argparse
import hashlib
import contextlib
import io
import ipaddress
import json
import math
import os
from pathlib import Path
import re
import selectors
import signal
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
MAX_JSON = 2 << 20
MAX_PROCESS_OUTPUT = 2 << 20
HEX = re.compile(r'^[0-9a-f]{64}$')
TOKEN = re.compile(r'^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$')


class Refusal(ValueError):
    pass


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def unaliased(path):
    path = Path(os.path.abspath(path))
    current = Path(path.anchor)
    for part in path.parts[1:]:
        current = current / part
        if current.is_symlink():
            raise Refusal('aliased_path')
    return path


def read_file(path, limit=MAX_JSON):
    path = unaliased(path)
    flags = os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW
    fd = os.open(path, flags)
    try:
        before = os.fstat(fd)
        if not stat.S_ISREG(before.st_mode) or before.st_size > limit:
            raise Refusal('input_not_bounded_regular_file')
        raw = bytearray()
        while len(raw) <= limit:
            chunk = os.read(fd, min(65536, limit + 1 - len(raw)))
            if not chunk:
                break
            raw.extend(chunk)
        after = os.fstat(fd)
        if len(raw) > limit or len(raw) != before.st_size or before.st_size != after.st_size or before.st_mtime_ns != after.st_mtime_ns:
            raise Refusal('input_changed_or_oversized')
        return bytes(raw)
    finally:
        os.close(fd)


def decode(raw):
    def pairs(values):
        result = {}
        for key, value in values:
            if key in result:
                raise Refusal('duplicate_json_key')
            result[key] = value
        return result
    def constant(_):
        raise Refusal('nonfinite_json')
    def number(text):
        value = float(text)
        if not math.isfinite(value):
            raise Refusal('nonfinite_json')
        return value
    def integer(text):
        if len(text) > 21 or not -(1<<63) <= int(text) < (1<<64):
            raise Refusal('integer_outside_bound')
        return int(text)
    try:
        value = json.loads(raw.decode('utf-8'), object_pairs_hook=pairs, parse_constant=constant, parse_float=number, parse_int=integer)
    except (ValueError, UnicodeError, RecursionError) as error:
        if isinstance(error, Refusal):
            raise
        raise Refusal('malformed_json') from None
    return value


def load(path):
    raw = read_file(path)
    return decode(raw), raw


def write_new(path, value):
    raw = (json.dumps(value, indent=2, allow_nan=False) + '\n').encode()
    if len(raw) > MAX_JSON:
        raise Refusal('report_too_large')
    path = unaliased(path)
    if not path.parent.is_dir():
        raise Refusal('output_parent_missing')
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        view = memoryview(raw)
        while view:
            count = os.write(fd, view)
            if count <= 0:
                raise Refusal('output_incomplete')
            view = view[count:]
        os.fsync(fd)
    finally:
        os.close(fd)
    directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def endpoint(value):
    parts = urlsplit(value)
    try:
        address = ipaddress.ip_address(parts.hostname or '')
        port = parts.port
    except ValueError:
        raise Refusal('explicit_loopback_endpoint_required') from None
    canonical = 'http://' + ('[::1]' if str(address) == '::1' else str(address)) + ':' + str(port)
    if (str(address) not in {'127.0.0.1', '::1'} or parts.scheme != 'http' or
            not port or not 1 <= port <= 65535 or value not in {canonical, canonical+'/'}):
        raise Refusal('explicit_loopback_endpoint_required')
    return canonical


def run_bounded(argv, deadline=25):
    """Drain both pipes under one deadline and a combined byte cap; never print them."""
    begin = time.monotonic()
    child = subprocess.Popen(argv, cwd=ROOT, stdin=subprocess.DEVNULL,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    selector = selectors.DefaultSelector()
    output = {'stdout': bytearray(), 'stderr': bytearray()}
    error_class = None
    for name, stream in [('stdout', child.stdout), ('stderr', child.stderr)]:
        os.set_blocking(stream.fileno(), False)
        selector.register(stream, selectors.EVENT_READ, name)
    try:
        while selector.get_map():
            remaining = deadline - (time.monotonic()-begin)
            if remaining <= 0:
                error_class = 'subprocess_deadline'
                break
            for key, _ in selector.select(min(.1, remaining)):
                chunk = os.read(key.fd, 65536)
                if not chunk:
                    selector.unregister(key.fileobj)
                    continue
                output[key.data].extend(chunk)
                if sum(len(v) for v in output.values()) > MAX_PROCESS_OUTPUT:
                    error_class = 'subprocess_output_bound'
                    break
            if error_class:
                break
        if error_class:
            try: os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError: pass
        try:
            child.wait(timeout=max(.01, deadline-(time.monotonic()-begin)))
        except subprocess.TimeoutExpired:
            error_class = 'subprocess_deadline'
            try: os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError: pass
            child.wait(timeout=2)
    finally:
        selector.close()
        child.stdout.close()
        child.stderr.close()
        if child.poll() is None:
            try: os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError: pass
            child.wait(timeout=2)
    return {'exit_code': child.returncode, 'elapsed_ms': round((time.monotonic()-begin)*1000, 3),
            'error_class': error_class, **{name: {'bytes': len(raw), 'sha256': digest(raw)} for name, raw in output.items()}}


def projection(package_path, project_dir, report_path):
    report, report_raw = load(report_path)
    package, package_raw = load(package_path)
    project_dir = unaliased(project_dir)
    if not project_dir.is_dir() or {p.name for p in project_dir.iterdir()} != {'scene.json'}:
        raise Refusal('project_requires_scene_only')
    scene_raw = read_file(project_dir/'scene.json')
    scene = decode(scene_raw)
    if (not isinstance(report, dict) or report.get('schema') != 'wendy.world.go2-project-conversion.v1'
            or report.get('domain') != 'simulation' or report.get('operational') is not False):
        raise Refusal('conversion_report_scope')
    source, target = report.get('source', {}), report.get('target', {})
    if not isinstance(package, dict) or not isinstance(source, dict) or not isinstance(target, dict):
        raise Refusal('malformed_projection_objects')
    if (package.get('schema_version') != 'wendy.world.package.v0' or package.get('provenance') != 'synthetic'
            or source.get('provenance') != 'synthetic' or source.get('package_sha256') != digest(package_raw)
            or any(source.get(k) != package.get(k) for k in ('site_id', 'package_revision', 'frame_revision'))
            or not TOKEN.fullmatch(source.get('run_id', ''))
            or source.get('clock_domain') != 'simulation:'+source['run_id']):
        raise Refusal('source_package_binding')
    revision = digest(b'scene.json\0' + scene_raw + b'\0')
    if (target.get('profile') != 'go2' or target.get('coordinates') != {'units':'metres','up':'Z','quaternion':'wxyz'}
            or target.get('project_revision') != revision
            or target.get('files') != [{'path':'scene.json','size':len(scene_raw),'sha256':digest(scene_raw)}]):
        raise Refusal('project_digest_or_files')
    if (not isinstance(scene, dict) or set(scene) != {'version','profile','geoms'} or type(scene['version']) is not int
            or scene['version'] != 1 or scene['profile'] != 'go2' or not isinstance(scene['geoms'], list)
            or not 1 <= len(scene['geoms']) <= 64):
        raise Refusal('unsupported_project_scene')
    selection = report.get('selection', [])
    if not isinstance(selection, list) or len(selection) != len(scene['geoms']):
        raise Refusal('selection_cardinality')
    for geom, selected in zip(scene['geoms'], selection):
        if not isinstance(geom, dict) or not isinstance(selected, dict):
            raise Refusal('malformed_geometry')
        if geom.get('type') not in {'box','sphere','cylinder','capsule','ellipsoid'}:
            raise Refusal('unsupported_selected_geometry')
        if geom != {'name':selected.get('project_geom_name'), **{k:selected.get(k) for k in ('type','size','position','quaternion','rgba')}}:
            raise Refusal('selection_scene_mismatch')
    if (not isinstance(report.get('differences'), list) or not report['differences']
            or not isinstance(report.get('exclusions'), dict)
            or not all(isinstance(v,list) for v in report['exclusions'].values())
            or not isinstance(report.get('selected_property_differences'),list)
            or len(report['selected_property_differences']) != len(selection)):
        raise Refusal('loss_report_missing')
    return {'conversion_report_sha256': digest(report_raw), 'package_sha256': digest(package_raw),
            'project_revision':revision, 'scene_sha256':digest(scene_raw), 'scene_bytes':len(scene_raw),
            'selected_geometries':len(selection), 'geometry_types':sorted({g['type'] for g in scene['geoms']}),
            'source':source, 'differences':report['differences'], 'exclusion_counts':{k:len(v) for k,v in report['exclusions'].items()},
            'verification_scope':'artifact consistency; Go binder separately verifies source asset closure and runtime pins'}


def binder_argv(args, verb, result_path):
    command = [str(Path(args.binder).absolute()), verb, '--package', str(Path(args.package).absolute()),
               '--project-dir', str(Path(args.project_dir).absolute()),
               '--conversion-report', str(Path(args.conversion_report).absolute()), '--out', str(result_path.absolute())]
    if verb == 'bind':
        if not TOKEN.fullmatch(args.vm_name) or not re.fullmatch(r'sha256:[0-9a-f]{64}', args.source_digest) or not re.fullmatch(r'[0-9a-f]{32}', args.project_id) or not HEX.fullmatch(args.project_revision) or args.world_epoch < 1:
            raise Refusal('explicit_runtime_pins_required')
        command += ['--endpoint', endpoint(args.endpoint), '--vm-name', args.vm_name,
                    '--source-digest', args.source_digest, '--project-id', args.project_id,
                    '--project-revision',args.project_revision,'--world-epoch', str(args.world_epoch)]
    else:
        command += ['--binding', str(Path(args.binding).absolute())]
    return command


def execute(args):
    result = {'schema':'wendy.world.go2-project-site-check.v1','domain':'simulation','operational':False,
              'mode':args.mode,'operational_authority':False,'runtime_mutations':0,'motion_tested':False,'result':'failed'}
    if args.mode == 'convert':
        if not 1 <= len(args.geom) <= 64 or len(set(args.geom)) != len(args.geom) or not TOKEN.fullmatch(args.run_id):
            raise Refusal('invalid_converter_selection')
        if Path(args.destination).exists():
            raise Refusal('conversion_destination_exists')
        command = [str(Path(args.python).absolute()), '-m', 'sim.go2_project', '--package', str(Path(args.package).absolute()),
                   '--run-id', args.run_id, '--out', str(Path(args.destination).absolute())]
        for geom in args.geom:
            command += ['--geom',geom]
        result['process'] = run_bounded(command)
        if result['process']['exit_code'] != 0 or result['process']['error_class']:
            result['error_class'] = 'converter_failed'
            return result
        args.project_dir = str(Path(args.destination)/'project')
        args.conversion_report = str(Path(args.destination)/'conversion.json')
    result['projection'] = projection(args.package,args.project_dir,args.conversion_report)
    if args.mode in {'verify','convert'}:
        result['result'] = 'verified'
        return result
    # Binder makes one GET only. The output is a new, explicit artifact, even for
    # a rejected old binding; neither the old binding nor runtime is changed.
    if args.mode == 'bind' and args.project_revision != result['projection']['project_revision']:
        raise Refusal('explicit_project_revision_mismatch')
    prior_raw = read_file(args.binding) if args.mode == 'check' else None
    result_path = unaliased(args.binding_report)
    if result_path.exists():
        raise Refusal('binding_report_exists')
    binary = read_file(args.binder, 64 << 20)
    result['binder_sha256'] = digest(binary)
    result['process'] = run_bounded(binder_argv(args,args.mode,result_path),deadline=8)
    if result['process']['error_class']:
        result['error_class'] = result['process']['error_class']
        return result
    try:
        value, raw = load(result_path)
    except (OSError, Refusal):
        result['error_class'] = 'binding_report_unavailable'
        return result
    result['binding_report_sha256'] = digest(raw)
    result['binding_report'] = value
    expected_schema = 'wendy.world.go2-project-binding.v1' if args.mode == 'bind' else 'wendy.world.go2-project-binding-check.v1'
    if (not isinstance(value,dict) or value.get('schema') != expected_schema or value.get('domain') != 'simulation'
            or value.get('authority') != 'unsigned-local-observation' or value.get('operational') is not False
            or value.get('operational_authority') is not False or value.get('runtime_instance_verified') is not False):
        result['error_class'] = 'binding_report_scope'
        return result
    code = result['process']['exit_code']
    if args.mode == 'bind':
        expected={'endpoint':endpoint(args.endpoint),'vm_name':args.vm_name,'source_digest':args.source_digest,
                  'project_id':args.project_id,'project_revision':args.project_revision,'world_epoch':args.world_epoch}
        projected=value.get('projection',{})
        local=result['projection']
        okay = (code == 0 and value.get('pins') == expected and isinstance(projected,dict)
                and projected.get('source') == local['source']
                and projected.get('conversion_sha256') == local['conversion_report_sha256']
                and projected.get('project_revision') == local['project_revision']
                and projected.get('files') == [{'path':'scene.json','size':local['scene_bytes'],'sha256':local['scene_sha256']}]
                and projected.get('selected_geoms') == local['selected_geometries']
                and isinstance(projected.get('loss_sha256'),str) and bool(HEX.fullmatch(projected['loss_sha256'])))
    elif args.expect_invalid:
        okay = (code > 0 and value.get('valid') is False and value.get('binding_sha256') == digest(prior_raw)
                and value.get('reason') == args.expect_reason and read_file(args.binding) == prior_raw)
        result['expected_invalidation'] = True
    else:
        okay = (code == 0 and value.get('valid') is True and value.get('binding_sha256') == digest(prior_raw)
                and value.get('reason') == 'binding_unchanged' and read_file(args.binding) == prior_raw)
    result['result'] = 'verified' if okay else 'failed'
    if not okay:
        result['error_class'] = 'unexpected_binding_outcome'
    return result


def parser():
    p = argparse.ArgumentParser(description=__doc__)
    sub = p.add_subparsers(dest='mode',required=True)
    sub.add_parser('self-test',help='local checks only; no simulator or MuJoCo')
    for mode in ('convert','verify','bind','check'):
        q=sub.add_parser(mode);q.add_argument('--package',required=True);q.add_argument('--out',required=True)
        if mode=='convert':
            q.add_argument('--python',default=sys.executable);q.add_argument('--geom',action='append',required=True)
            q.add_argument('--run-id',required=True);q.add_argument('--destination',required=True)
        else:
            q.add_argument('--project-dir',required=True);q.add_argument('--conversion-report',required=True)
        if mode in {'bind','check'}:
            q.add_argument('--binder',required=True);q.add_argument('--binding-report',required=True)
        if mode=='bind':
            for flag in ('endpoint','vm-name','source-digest','project-id','project-revision'):q.add_argument('--'+flag,required=True)
            q.add_argument('--world-epoch',required=True,type=int)
        if mode=='check':
            q.add_argument('--binding',required=True);q.add_argument('--expect-invalid',action='store_true')
            q.add_argument('--expect-reason',choices=['binding_invalid','conversion_invalid','conversion_changed','runtime_unavailable_or_changed','runtime_reset_or_regressed'])
    return p


class LocalChecks(unittest.TestCase):
    def fixture(self,root):
        package={'schema_version':'wendy.world.package.v0','provenance':'synthetic','site_id':'site',
                 'package_revision':'p1','frame_revision':'f1'}
        raw=json.dumps(package).encode();(root/'world.json').write_bytes(raw)
        project=root/'project';project.mkdir()
        geom={'name':'world_geom_000','type':'box','size':[.2,.3,.4],'position':[3,0,.4],
              'quaternion':[1,0,0,0],'rgba':[.2,.3,.4,1]}
        scene=json.dumps({'version':1,'profile':'go2','geoms':[geom]}).encode();(project/'scene.json').write_bytes(scene)
        report={'schema':'wendy.world.go2-project-conversion.v1','domain':'simulation','operational':False,
                'source':{**{k:package[k] for k in ('site_id','package_revision','frame_revision','provenance')},
                          'package_sha256':digest(raw),'run_id':'test-run','clock_domain':'simulation:test-run'},
                'target':{'profile':'go2','coordinates':{'units':'metres','up':'Z','quaternion':'wxyz'},
                          'project_revision':digest(b'scene.json\0'+scene+b'\0'),
                          'files':[{'path':'scene.json','size':len(scene),'sha256':digest(scene)}]},
                'selection':[{'project_geom_name':geom['name'],**{k:v for k,v in geom.items() if k!='name'}}],
                'differences':['source physics excluded'],'selected_property_differences':[{}],'exclusions':{'geoms':[]}}
        (root/'conversion.json').write_text(json.dumps(report))
        return report
    def test_exact_projection_and_changed_digest(self):
        with tempfile.TemporaryDirectory() as folder:
            root=Path(folder).resolve();self.fixture(root)
            result=projection(root/'world.json',root/'project',root/'conversion.json')
            self.assertEqual(result['selected_geometries'],1)
            (root/'project/scene.json').write_text('{}')
            with self.assertRaisesRegex(Refusal,'project_digest'):projection(root/'world.json',root/'project',root/'conversion.json')
    def test_mismatch_source_selection_and_extra_files(self):
        for mutation in ('source','selection','extra','loss'):
            with self.subTest(mutation=mutation),tempfile.TemporaryDirectory() as folder:
                root=Path(folder).resolve();report=self.fixture(root)
                if mutation=='source':report['source']['frame_revision']='other'
                elif mutation=='selection':report['selection'][0]['position']=[5,0,0]
                elif mutation=='loss':report['differences']=[]
                else:(root/'project/extra').write_text('x')
                (root/'conversion.json').write_text(json.dumps(report))
                with self.assertRaises(Refusal):projection(root/'world.json',root/'project',root/'conversion.json')
    def test_json_duplicate_and_nonfinite(self):
        for raw in (b'{"x":1,"x":2}',b'{"x":NaN}',b'{"x":1e999}',b'{"x":999999999999999999999999999}',b'\xff',b'['*2000+b']'*2000):
            with self.subTest(raw=raw),self.assertRaises(Refusal):decode(raw)
    def test_fifo_symlink_and_bounds(self):
        with tempfile.TemporaryDirectory() as folder:
            root=Path(folder).resolve();(root/'regular').write_bytes(b'abc');(root/'link').symlink_to(root/'regular');os.mkfifo(root/'fifo')
            for path,limit in [(root/'link',10),(root/'fifo',10),(root/'regular',2)]:
                with self.subTest(path=path),self.assertRaises(Refusal):read_file(path,limit)
    def test_exclusive_private_output(self):
        with tempfile.TemporaryDirectory() as folder:
            path=Path(folder).resolve()/'report.json';write_new(path,{'operational':False})
            self.assertEqual(stat.S_IMODE(path.stat().st_mode),0o600)
            with self.assertRaises(FileExistsError):write_new(path,{'changed':True})
            self.assertEqual(load(path)[0],{'operational':False})
    def test_endpoint_requires_exact_local_origin(self):
        self.assertEqual(endpoint('http://127.0.0.1:8890/'),'http://127.0.0.1:8890')
        for value in ('http://localhost:8890','http://10.0.0.2:8890','http://127.0.0.1:8890/api/reset',
                      'http://user@127.0.0.1:8890','http://127.0.0.1:8890?x=1','http://127.0.0.1:08890'):
            with self.subTest(value=value),self.assertRaises(Refusal):endpoint(value)
    def test_only_fixed_readonly_binder_verbs(self):
        args=argparse.Namespace(binder='/bin/binder',package='world.json',project_dir='project',conversion_report='conversion.json',
             endpoint='http://127.0.0.1:8890',vm_name='lab',source_digest='sha256:'+'a'*64,project_id='b'*32,project_revision='c'*64,world_epoch=3,binding='prior.json')
        command=binder_argv(args,'bind',Path('out.json'))
        self.assertEqual(command[1],'bind');self.assertNotIn('reset',command);self.assertNotIn('sync',command)
        self.assertEqual(binder_argv(args,'check',Path('out.json'))[1],'check')
        with contextlib.redirect_stderr(io.StringIO()),self.assertRaises(SystemExit):parser().parse_args(['bind','--apply'])
    def test_subprocess_output_bound_and_timeout(self):
        # Local child programs only, with no socket or simulator access.
        result=run_bounded([sys.executable,'-c','print("x"*3000000)'],deadline=2)
        self.assertEqual(result['error_class'],'subprocess_output_bound')
        result=run_bounded([sys.executable,'-c','import time;time.sleep(2)'],deadline=.05)
        self.assertEqual(result['error_class'],'subprocess_deadline')
    def binder_fixture(self,root,mode='bind'):
        report=self.fixture(root)
        (root/'binder').write_bytes(b'local test executable placeholder')
        (root/'prior.json').write_bytes(b'{"immutable_prior":true}\n')
        args=argparse.Namespace(mode=mode,package=root/'world.json',project_dir=root/'project',
             conversion_report=root/'conversion.json',binder=root/'binder',binding_report=root/'bound.json',
             endpoint='http://127.0.0.1:8890',vm_name='lab',source_digest='sha256:'+'a'*64,
             project_id='b'*32,project_revision=report['target']['project_revision'],world_epoch=3,
             binding=root/'prior.json',expect_invalid=False,expect_reason=None)
        value={'schema':'wendy.world.go2-project-binding.v1','domain':'simulation',
               'authority':'unsigned-local-observation','operational':False,'operational_authority':False,
               'runtime_instance_verified':False,
               'pins':{k:getattr(args,k) for k in ('endpoint','vm_name','source_digest','project_id','project_revision','world_epoch')},
               'projection':{'source':report['source'],'conversion_sha256':digest(read_file(args.conversion_report)),
                             'loss_sha256':'d'*64,'project_revision':args.project_revision,
                             'files':report['target']['files'],'selected_geoms':1}}
        return args,value
    def fake_binder(self,args,value,code=0,mutate_prior=False):
        def run(command,deadline):
            self.assertEqual(command[1],args.mode);self.assertEqual(deadline,8)
            write_new(args.binding_report,value)
            if mutate_prior:args.binding.write_text('{}')
            return {'exit_code':code,'elapsed_ms':1,'error_class':None}
        return run
    def test_bind_exact_pins_projection_and_nonoperational_scope(self):
        for mutation in ('none','pin','projection','operational','authority','instance','missing'):
            with self.subTest(mutation=mutation),tempfile.TemporaryDirectory() as folder:
                root=Path(folder).resolve();args,value=self.binder_fixture(root)
                if mutation=='pin':value['pins']['world_epoch']=4
                elif mutation=='projection':value['projection']['conversion_sha256']='f'*64
                elif mutation=='operational':value['operational']=True
                elif mutation=='authority':value['authority']='signed-authority'
                elif mutation=='instance':value['runtime_instance_verified']=True
                elif mutation=='missing':del value['operational_authority']
                with mock.patch(__name__+'.run_bounded',side_effect=self.fake_binder(args,value)):
                    result=execute(args)
                self.assertEqual(result['result'],'verified' if mutation=='none' else 'failed')
    def test_nonzero_invalidation_requires_exact_reason_hash_and_unchanged_prior(self):
        for mutation in ('none','hash','reason','exit','prior'):
            with self.subTest(mutation=mutation),tempfile.TemporaryDirectory() as folder:
                root=Path(folder).resolve();args,_=self.binder_fixture(root,'check')
                args.expect_invalid=True;args.expect_reason='runtime_unavailable_or_changed'
                value={'schema':'wendy.world.go2-project-binding-check.v1','domain':'simulation',
                       'authority':'unsigned-local-observation','operational':False,'operational_authority':False,
                       'runtime_instance_verified':False,'valid':False,'reason':args.expect_reason,
                       'binding_sha256':digest(read_file(args.binding))}
                if mutation=='hash':value['binding_sha256']='f'*64
                elif mutation=='reason':value['reason']='binding_invalid'
                with mock.patch(__name__+'.run_bounded',side_effect=self.fake_binder(args,value,0 if mutation=='exit' else 1,mutation=='prior')):
                    result=execute(args)
                self.assertEqual(result['result'],'verified' if mutation=='none' else 'failed')
                self.assertEqual(result['process']['exit_code'],0 if mutation=='exit' else 1)
    def test_invalid_artifact_or_expected_revision_never_invokes_binder(self):
        for mutation in ('scene','revision'):
            with self.subTest(mutation=mutation),tempfile.TemporaryDirectory() as folder:
                root=Path(folder).resolve();args,_=self.binder_fixture(root)
                if mutation=='scene':(root/'project/scene.json').write_text('{}')
                else:args.project_revision='f'*64
                with mock.patch(__name__+'.run_bounded') as run,self.assertRaises(Refusal):execute(args)
                run.assert_not_called()
    def test_missing_or_malformed_child_report_is_not_invalidation(self):
        for raw in (None,b'[]',b'{'):
            with self.subTest(raw=raw),tempfile.TemporaryDirectory() as folder:
                root=Path(folder).resolve();args,_=self.binder_fixture(root,'check')
                args.expect_invalid=True;args.expect_reason='runtime_unavailable_or_changed'
                def run(*unused,**kw):
                    if raw is not None:args.binding_report.write_bytes(raw)
                    return {'exit_code':1,'error_class':None}
                with mock.patch(__name__+'.run_bounded',side_effect=run):result=execute(args)
                self.assertEqual(result['result'],'failed')


def main(argv=None):
    args=parser().parse_args(argv)
    if args.mode=='self-test':
        suite=unittest.defaultTestLoader.loadTestsFromTestCase(LocalChecks)
        return 0 if unittest.TextTestRunner(verbosity=2).run(suite).wasSuccessful() else 1
    if args.mode=='check' and bool(args.expect_invalid) != bool(args.expect_reason):
        raise Refusal('expected_invalidation_reason_required')
    output=unaliased(args.out)
    if output.exists():
        raise Refusal('output_exists')
    try:
        result=execute(args)
    except (OSError,ValueError,TypeError,KeyError,subprocess.SubprocessError) as error:
        result={'schema':'wendy.world.go2-project-site-check.v1','domain':'simulation','operational':False,
                'mode':args.mode,'operational_authority':False,'runtime_mutations':0,'motion_tested':False,'result':'failed',
                'error_class':str(error) if isinstance(error,Refusal) else type(error).__name__}
    write_new(output,result)
    print(json.dumps({'schema':result['schema'],'result':result['result'],'mode':args.mode,
                      'operational':False,'runtime_mutations':0,'report_sha256':digest(read_file(output))}))
    return 0 if result['result']=='verified' else 1


if __name__=='__main__':
    try:raise SystemExit(main())
    except (OSError,Refusal):
        print('Go2 project check refused before execution',file=sys.stderr);raise SystemExit(1)
