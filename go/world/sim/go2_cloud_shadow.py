"""Explicitly invoked, bounded passive cloud shadow capture.

No ROS publishers, runtime HTTP, arming, task authority or motion commands exist
here. Local machine wall-clock age is an observation, not calibrated source time.
The fixed admission window includes process launch and decoding. Child cleanup
has a separate bounded allowance; report serialization/writing is outside both
windows and has no claimed hard latency bound.
"""
from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
from pathlib import Path
import selectors
import subprocess
import time

from .go2_cloud import SOURCE_DIGEST
from .go2_cloud_observer import TOPICS, output_path, integer
from .go2_cloud_stream import Assembler

HELPER = '/opt/world/world-go2-cloud-shadow'
MAX_ROW = 256 << 10
MAX_RAW = 16 << 20
MAX_STDOUT = 40 << 20
MAX_STDERR = 64 << 10
MAX_REPORT = 48 << 20
MAX_MESSAGES = 8192
MAX_EVENTS = 1024
MAX_CLOUDS = 128
MAX_AGE_NS = 250_000_000
CLOCK_DIVERGENCE_NS = 50_000_000
CLEANUP_NS = 1_000_000_000
ROW_FIELDS = frozenset(('kind','topic','publisher_gid','source_timestamp_ns',
                       'received_timestamp_ns','helper_elapsed_ns','payload_hex'))


class ShadowError(ValueError):
    pass


def require(ok, code):
    if not ok: raise ShadowError(code)


def encoded(value):
    return (json.dumps(value,sort_keys=True,separators=(',', ':'),allow_nan=False)+'\n').encode()


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def strict(raw):
    require(type(raw) is bytes and 0 < len(raw) <= MAX_ROW, 'helper_row_bound')
    def pairs(items):
        result={}
        for k,v in items:
            require(k not in result,'helper_duplicate_field');result[k]=v
        return result
    def whole(s):
        require(len(s)<=20,'helper_integer_bound');return int(s)
    def forbidden(_): raise ShadowError('helper_nonfinite_json')
    try:return json.loads(raw,object_pairs_hook=pairs,parse_int=whole,parse_constant=forbidden)
    except (ValueError,UnicodeError,RecursionError):raise ShadowError('helper_invalid_json') from None


class ShadowCapture:
    """Single-owner capture with injectable clocks for deterministic tests.

    Candidates are assembler results plus fixed post-decode diagnostic checks.
    They are summarized once, never refreshed or exposed as motion permission.
    """
    def __init__(self, source_digest, expected_epoch, duration_ms, max_clouds, *,
                 startup_ms=1000, companion_ms=100, monotonic_ns=None, wall_ns=None,
                 assembler_factory=None):
        require(source_digest==SOURCE_DIGEST,'source_digest')
        integer(expected_epoch,1);integer(duration_ms,100,60000);integer(max_clouds,1,MAX_CLOUDS)
        integer(startup_ms,1,min(5000,duration_ms));integer(companion_ms,1,min(100,duration_ms))
        self.monotonic_ns=monotonic_ns or time.monotonic_ns
        self.wall_ns=wall_ns or time.time_ns
        self.source_digest=source_digest;self.expected_epoch=expected_epoch
        self.duration_ms=duration_ms;self.max_clouds=max_clouds
        self.error=None;self.admission_closed=None;self.start_clock=None;self.last_clock=None;self.finished=False
        self.messages=[];self.events=[];self.candidates=[];self.completion=None
        self.raw_bytes=0;self.clouds=0;self.retained=0;self.late_rows=0
        self.terminal_input=None;self.references={};self.native_gids={};self.native_sources={}
        self.native_elapsed=0
        self.start_clock=self._clock();self.started_ns=self.start_clock['monotonic_after_ns']
        self.deadline_ns=self.started_ns+duration_ms*1_000_000
        self.startup_deadline_ns=self.started_ns+startup_ms*1_000_000
        self.companion_ns=companion_ms*1_000_000
        factory=assembler_factory or Assembler
        self.assembler=factory(source_digest,expected_epoch,start_ns=self.started_ns,
            startup_timeout_ns=startup_ms*1_000_000,run_timeout_ns=duration_ms*1_000_000,
            companion_timeout_ns=self.companion_ns)

    def _clock(self):
        before=integer(self.monotonic_ns());wall=integer(self.wall_ns(),1);after=integer(self.monotonic_ns())
        require(after>=before,'monotonic_regression')
        require(after-before<=CLOCK_DIVERGENCE_NS,'clock_observation_span')
        current={'monotonic_before_ns':before,'wall_ns':wall,'monotonic_after_ns':after}
        if self.last_clock is not None:
            require(before>=self.last_clock['monotonic_after_ns'],'monotonic_regression')
            require(wall>=self.last_clock['wall_ns'],'wall_regression')
        if self.start_clock is not None:
            base=self.start_clock
            delta=(wall-base['wall_ns'])-(after-base['monotonic_after_ns'])
            width=(after-before)+(base['monotonic_after_ns']-base['monotonic_before_ns'])
            require(abs(delta)<=CLOCK_DIVERGENCE_NS+width,'wall_monotonic_divergence')
        self.last_clock=current
        return current

    def fail(self, code):
        if self.error is None:self.error=code
        if self.admission_closed is None:
            self.admission_closed={'reason':code,'monotonic_ns':self.last_clock['monotonic_after_ns']}
            self.assembler.close(self.last_clock['monotonic_after_ns'])

    def _event(self, kind, reason, capture_id, now):
        require(len(self.events)<MAX_EVENTS,'event_limit')
        self.events.append({'kind':'assembler_ready' if kind=='ready' else kind,'reason':reason,'capture_id':capture_id,'observed_monotonic_ns':now})

    def _check_time(self, clock):
        now=clock['monotonic_after_ns']
        if self.admission_closed is not None:return
        if now>=self.deadline_ns:
            self.fail('run_deadline')
        elif self.retained==0 and now>=self.startup_deadline_ns:
            self.fail('startup_deadline')

    def tick(self):
        if self.error or self.finished:return
        try:
            current=self._clock();self._check_time(current)
            if self.admission_closed is None:
                for event in self.assembler.poll(current['monotonic_after_ns']):
                    require(event.kind!='ready','unexpected_poll_candidate')
                    self._event(event.kind,event.reason,event.capture_id,current['monotonic_after_ns'])
                    if event.kind=='fatal':self.fail('assembler_'+event.reason)
            return current
        except (ValueError,TypeError,OverflowError) as error:
            self.fail(self._code(error))

    @staticmethod
    def _code(error):
        code=str(error)
        return code if len(code)<=96 and all(c in 'abcdefghijklmnopqrstuvwxyz0123456789_' for c in code) else 'shadow_invalid'

    def _sample_ref(self, sample):
        key=(sample.topic,sample.serialized_sha256,sample.arrived_ns,sample.source_timestamp_ns)
        require(key in self.references,'sample_reference_missing')
        index=self.references[key]
        row=self.messages[index]
        require(bytes.fromhex(row['row']['payload_hex'])==sample.raw_cdr,'sample_reference_bytes')
        return {'message_index':index,'topic':sample.topic,'serialized_sha256':sample.serialized_sha256,
                'publisher_gid':sample.publisher_gid,'source_timestamp_ns':sample.source_timestamp_ns,
                'received_timestamp_ns':sample.received_timestamp_ns,'arrived_monotonic_ns':sample.arrived_ns}

    def _candidate(self, event, checked):
        c=event.candidate
        require(c is not None and len(self.candidates)<self.max_clouds,'candidate_limit_or_shape')
        refs={key:self._sample_ref(getattr(c,key)) for key in ('epoch_sample','cloud','scan','odom')}
        refs['transforms']=[self._sample_ref(s) for s in c.transforms]
        stamps={'cloud':c.cloud.value.capture_unix_ns,'scan':c.scan.value['capture_unix_ns'],
                'odom':c.odom.value['capture_unix_ns']}
        checked=self._clock();self._check_time(checked)
        now=checked['monotonic_after_ns'];wall=checked['wall_ns']
        ages={key:wall-stamp for key,stamp in stamps.items()}
        reason=None
        if self.error:reason=self.error
        elif self.admission_closed:reason=self.admission_closed['reason']
        elif now>=c.companion_deadline_ns:reason='postdecode_companion_deadline'
        elif any(age<0 for age in ages.values()):reason='original_stamp_future_local_wall'
        elif any(age>MAX_AGE_NS for age in ages.values()):reason='original_age_exceeds_local_limit'
        result={'capture_id':c.capture_id,'source_digest':c.source_digest,'observed_epoch':c.observed_epoch,
                'sample_references':refs,'assembled_monotonic_ns':c.assembled_at_ns,
                'companion_deadline_ns':c.companion_deadline_ns,'postdecode_clock':dict(checked),
                'original_capture_stamps_ns':stamps,'local_wall_ages_ns':ages,
                'odom_minus_cloud_ns':c.odom_minus_cloud_ns,'disposition':'refused' if reason else 'retained',
                'refusal_reason':reason,'age_role':'uncalibrated_local_machine_wall_comparison',
                'operational_authority':False}
        self.candidates.append(result)
        if reason is None:self.retained+=1

    def ingest_line(self, raw):
        if self.error or self.finished:return
        try:
            arrival=self._clock();self._check_time(arrival)
            if self.error:
                self._terminal_bytes(raw);return
            row=strict(raw)
            require(type(row) is dict,'helper_schema')
            if row.get('kind')=='complete':
                require(self.completion is None and set(row)=={'kind','reason','messages','clouds','serialized_bytes'},'completion_schema')
                require(row['reason'] in {'max_clouds','duration','capture_limit','interrupted'},'completion_reason')
                require(integer(row['messages'])==len(self.messages) and integer(row['clouds'])==self.clouds and integer(row['serialized_bytes'])==self.raw_bytes,'completion_counts')
                require(row['reason']!='max_clouds' or self.clouds==self.max_clouds,'completion_cloud_limit')
                self.completion={'row':row,'arrival_clock':arrival,'line_sha256':sha(raw)}
                checked=self._clock();self._check_time(checked)
                return
            require(self.completion is None,'sample_after_completion')
            require(len(row)==len(ROW_FIELDS) and set(row)==ROW_FIELDS and row['kind']=='sample','helper_schema')
            topic=row['topic'];require(type(topic) is str and topic in TOPICS,'helper_topic')
            payload=row['payload_hex'];require(type(payload) is str and 0<len(payload)<=2*TOPICS[topic] and len(payload)%2==0,'payload_bound')
            require(all(c in '0123456789abcdef' for c in payload),'payload_encoding')
            body=bytes.fromhex(payload)
            require(len(self.messages)<MAX_MESSAGES and self.raw_bytes+len(body)<=MAX_RAW,'capture_resource_limit')
            require(topic!='/utlidar/cloud' or self.clouds<self.max_clouds,'cloud_limit')
            gid=row['publisher_gid'];require(type(gid) is str and len(gid)==48 and all(c in '0123456789abcdef' for c in gid) and gid!='0'*48,'helper_gid')
            source=integer(row['source_timestamp_ns'],1);integer(row['received_timestamp_ns']);elapsed=integer(row['helper_elapsed_ns'])
            require(topic not in self.native_gids or self.native_gids[topic]==gid,'publisher_changed')
            require(source>self.native_sources.get(topic,0),'source_timestamp_nonadvancing')
            require(elapsed>=self.native_elapsed,'helper_clock_regression')
            require(elapsed<self.duration_ms*1_000_000,'helper_after_native_deadline')
            self.native_gids[topic]=gid;self.native_sources[topic]=source;self.native_elapsed=elapsed
            index=len(self.messages);digest=sha(body)
            saved={'index':index,'row':row,'arrival_clock':arrival,'postdecode_clock':None,
                   'line_sha256':sha(raw),'serialized_sha256':digest,'serialized_bytes':len(body),
                   'admission':'closed_unassembled' if self.admission_closed else 'submitted_to_assembler'}
            self.messages.append(saved);self.raw_bytes+=len(body);self.clouds+=topic=='/utlidar/cloud'
            key=(topic,digest,arrival['monotonic_after_ns'],source);self.references[key]=index
            batch=() if self.admission_closed else self.assembler.ingest(row,arrival['monotonic_after_ns'])
            if self.admission_closed:self.late_rows+=1
            checked=self._clock();saved['postdecode_clock']=checked;self._check_time(checked)
            for event in batch:
                self._event(event.kind,event.reason,event.capture_id,checked['monotonic_after_ns'])
                if event.kind=='fatal':self.fail('assembler_'+event.reason)
                if event.kind=='ready':self._candidate(event,checked)
        except (ValueError,TypeError,KeyError,OverflowError,RecursionError) as error:
            self._terminal_bytes(raw)
            self.fail(self._code(error))

    def _terminal_bytes(self, raw):
        if self.terminal_input is None:
            prefix=raw[:MAX_ROW] if type(raw) is bytes else b''
            self.terminal_input={'prefix_hex':prefix.hex(),'observed_bytes':len(raw) if type(raw) is bytes else None,
                                 'sha256':sha(raw) if type(raw) is bytes else None,
                                 'truncated':type(raw) is bytes and len(raw)>MAX_ROW}

    def finish(self, exit_code, eof, cleanup, *, stdout_bytes=0, stderr=b'', partial=b'',
               eof_observed_ns=None, exit_observed_ns=None, stderr_observed_bytes=None):
        if self.error is None:
            if partial:self.fail('helper_partial_row')
            elif self.completion is None:self.fail('helper_missing_completion')
            elif not eof:self.fail('helper_missing_eof')
            elif exit_code!=0:self.fail('helper_nonzero_exit')
            elif not cleanup.get('reaped',False):self.fail('helper_cleanup_incomplete')
        if self.admission_closed is None:
            self.admission_closed={'reason':'capture_finished','monotonic_ns':self.last_clock['monotonic_after_ns']}
            for event in self.assembler.close(self.last_clock['monotonic_after_ns']):
                self._event(event.kind,event.reason,event.capture_id,self.last_clock['monotonic_after_ns'])
        complete=self.error is None and self.completion['row']['reason'] in {'max_clouds','duration'} and bool(self.clouds)
        report={'schema':'wendy.world.go2-cloud-shadow.v1','simulation':True,'operational_authority':False,
                'authorization_status':'not_evaluated','clock_status':'uncalibrated_local_machine_observations',
                'commands_published':0,'runtime_http_calls':0,'source_digest':self.source_digest,'expected_epoch':self.expected_epoch,
                'helper':HELPER,'duration_ms':self.duration_ms,'max_clouds':self.max_clouds,
                'start_clock':self.start_clock,'last_clock':self.last_clock,'run_deadline_ns':self.deadline_ns,
                'startup_deadline_ns':self.startup_deadline_ns,'companion_timeout_ns':self.companion_ns,
                'admission_closed':self.admission_closed,
                'error':self.error,'capture_complete':complete,'messages':self.messages,'events':self.events,
                'candidates':self.candidates,'retained_candidates':self.retained,
                'refused_candidates':len(self.candidates)-self.retained,'late_unassembled_rows':self.late_rows,
                'cloud_count':self.clouds,'serialized_bytes':self.raw_bytes,'stdout_bytes':stdout_bytes,
                'helper_completion':self.completion,'helper_exit_code':exit_code,'helper_eof':eof,'cleanup':cleanup,
                'helper_eof_observed_monotonic_ns':eof_observed_ns,
                'helper_exit_observed_monotonic_ns':exit_observed_ns,
                'helper_exit_observed_before_cleanup':exit_observed_ns is not None,
                'stderr_hex':stderr[:MAX_STDERR].hex(),'stderr_observed_bytes':stderr_observed_bytes,
                'partial_stdout_hex':partial[:MAX_ROW].hex(),'partial_stdout_observed_bytes':len(partial),
                'partial_stdout_sha256':sha(partial),'partial_stdout_truncated':len(partial)>MAX_ROW,
                'terminal_input':self.terminal_input,'assembler_status':self.assembler.status(),
                'limits':{'row_bytes':MAX_ROW,'raw_bytes':MAX_RAW,'stdout_bytes':MAX_STDOUT,'stderr_bytes':MAX_STDERR,
                          'messages':MAX_MESSAGES,'report_bytes':MAX_REPORT,'events':MAX_EVENTS,
                          'local_age_max_ns_inclusive':MAX_AGE_NS,'clock_divergence_ns':CLOCK_DIVERGENCE_NS,
                          'cleanup_allowance_ns':CLEANUP_NS},
                'scope':['Original CDR is retained as native reserialization, not original transport packets.',
                         'Publisher identifiers and source/epoch observations are not authenticated runtime identity.',
                         'Candidate retention uses uncalibrated machine wall age; it is not permission, calibrated freshness or clearance.',
                         'Admission ends at the fixed parent deadline. Cleanup only terminates and reaps; timeout remains incomplete.',
                         'Report encoding, file writing and fsync are outside capture/cleanup timing guarantees.']}
        require(len(encoded(report))<=MAX_REPORT,'report_size_bound')
        self.finished=True
        return copy.deepcopy(report)


def stop_helper(child):
    actions=[]
    if child.poll() is None:
        child.terminate();actions.append('terminate')
        try:child.wait(timeout=.5)
        except subprocess.TimeoutExpired:
            child.kill();actions.append('kill');child.wait(timeout=.5)
    code=child.poll()
    return {'actions':actions,'reaped':code is not None,'exit_code':code}


def observe(source_digest, expected_epoch, duration_ms, max_clouds, *, startup_ms=1000, companion_ms=100):
    capture=ShadowCapture(source_digest,expected_epoch,duration_ms,max_clouds,startup_ms=startup_ms,companion_ms=companion_ms)
    child=None;selector=None;pending=b'';stderr=bytearray();stdout_bytes=0;eof=False;code=None
    eof_ns=None;exit_ns=None;stderr_bytes=0
    cleanup={'actions':[],'reaped':False,'exit_code':None}
    try:
        child=subprocess.Popen([HELPER,'--duration-ms',str(duration_ms),'--max-clouds',str(max_clouds)],
            stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.PIPE,close_fds=True)
        selector=selectors.DefaultSelector();selector.register(child.stdout,selectors.EVENT_READ,'stdout');selector.register(child.stderr,selectors.EVENT_READ,'stderr')
        while selector.get_map():
            capture.tick()
            if capture.error:break
            now=time.monotonic_ns()
            limit=min(capture.deadline_ns,capture.startup_deadline_ns if capture.retained==0 else capture.deadline_ns)
            if now>=limit:
                capture.fail('run_deadline' if now>=capture.deadline_ns else 'startup_deadline');break
            events=selector.select(min(.05,(limit-now)/1e9))
            for key,_ in events:
                chunk=os.read(key.fileobj.fileno(),65536)
                if not chunk:
                    selector.unregister(key.fileobj)
                    observed=capture.tick()
                    if key.data=='stdout':
                        eof=True;eof_ns=observed['monotonic_after_ns'] if observed is not None else None
                    continue
                if key.data=='stderr':
                    stderr_bytes+=len(chunk)
                    if len(stderr)+len(chunk)>MAX_STDERR:
                        stderr.extend(chunk[:MAX_STDERR-len(stderr)]);capture.fail('stderr_limit');break
                    stderr.extend(chunk);continue
                stdout_bytes+=len(chunk)
                if stdout_bytes>MAX_STDOUT:
                    capture._terminal_bytes(chunk);capture.fail('stdout_limit');break
                pending+=chunk
                while b'\n' in pending and not capture.error:
                    line,pending=pending.split(b'\n',1)
                    capture.ingest_line(line+b'\n')
                if len(pending)>MAX_ROW:
                    capture.fail('helper_row_bound');break
            if capture.error:break
        if capture.error is None:
            remaining=(capture.deadline_ns-time.monotonic_ns())/1e9
            require(remaining>0,'helper_exit_timeout')
            code=child.wait(timeout=min(.5,remaining))
            observed=capture.tick();exit_ns=observed['monotonic_after_ns'] if observed is not None else None
    except subprocess.TimeoutExpired:capture.fail('helper_exit_timeout')
    except KeyboardInterrupt:capture.fail('capture_interrupted')
    except (OSError,ValueError,KeyError,TypeError,RuntimeError) as error:capture.fail(capture._code(error))
    finally:
        if selector is not None:selector.close()
        if child is not None:
            try:cleanup=stop_helper(child);code=cleanup['exit_code']
            except (OSError,subprocess.TimeoutExpired):capture.fail('helper_cleanup_incomplete')
            for pipe in (child.stdout,child.stderr):
                if pipe is not None:pipe.close()
    return capture.finish(code,eof,cleanup,stdout_bytes=stdout_bytes,stderr=bytes(stderr),partial=pending,
                          eof_observed_ns=eof_ns,exit_observed_ns=exit_ns,stderr_observed_bytes=stderr_bytes)


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-digest',required=True);parser.add_argument('--expected-epoch',type=int,required=True)
    parser.add_argument('--duration-ms',type=int,required=True);parser.add_argument('--max-clouds',type=int,required=True)
    parser.add_argument('--startup-ms',type=int,default=1000);parser.add_argument('--companion-ms',type=int,default=100)
    parser.add_argument('--out',required=True);args=parser.parse_args(argv)
    path=output_path(args.out)
    result=observe(args.source_digest,args.expected_epoch,args.duration_ms,args.max_clouds,startup_ms=args.startup_ms,companion_ms=args.companion_ms)
    raw=encoded(result);require(len(raw)<=MAX_REPORT,'report_size_bound')
    fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,'wb') as out:out.write(raw);out.flush();os.fsync(out.fileno())
    print(encoded({k:result[k] for k in ('schema','capture_complete','retained_candidates','refused_candidates','late_unassembled_rows','error','commands_published','runtime_http_calls')}).decode(),end='')
    return 0 if result['capture_complete'] and result['retained_candidates']>0 else 1


if __name__=='__main__':
    try:raise SystemExit(main())
    except (OSError,ValueError):
        print('passive_cloud_shadow_refused',file=__import__('sys').stderr);raise SystemExit(1)
