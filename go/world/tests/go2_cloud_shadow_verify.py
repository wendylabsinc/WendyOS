#!/usr/bin/env python3
"""Bounded independent integrity verification of a passive shadow report.

Failure captures can have valid integrity. This verifies original byte lineage
and declared local-clock checks, not runtime identity, calibrated time, geometry
or authority. No network, ROS, process launch or report mutation occurs.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys

ROOT=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(ROOT))
from sim.go2_cloud import SOURCE_DIGEST
from sim.go2_cloud_stream import Assembler

MAX_REPORT=48<<20
MAX_ROW=256<<10
MAX_RAW=16<<20
MAX_MESSAGES=8192
TOPICS={'/utlidar/cloud':110000,'/scan':49152,'/odom':8192,'/simulation/epoch':64,'/tf_static':16384}
ROW_KEYS={'kind','topic','publisher_gid','source_timestamp_ns','received_timestamp_ns','helper_elapsed_ns','payload_hex'}
TOP_KEYS={'schema','simulation','operational_authority','authorization_status','clock_status','commands_published','runtime_http_calls','source_digest','expected_epoch','helper','duration_ms','max_clouds','start_clock','last_clock','run_deadline_ns','startup_deadline_ns','companion_timeout_ns','admission_closed','error','capture_complete','messages','events','candidates','retained_candidates','refused_candidates','late_unassembled_rows','cloud_count','serialized_bytes','stdout_bytes','helper_completion','helper_exit_code','helper_eof','cleanup','helper_eof_observed_monotonic_ns','helper_exit_observed_monotonic_ns','helper_exit_observed_before_cleanup','stderr_hex','stderr_observed_bytes','partial_stdout_hex','partial_stdout_observed_bytes','partial_stdout_sha256','partial_stdout_truncated','terminal_input','assembler_status','limits','scope'}
SAVED_KEYS={'index','row','arrival_clock','postdecode_clock','line_sha256','serialized_sha256','serialized_bytes','admission'}
CANDIDATE_KEYS={'capture_id','source_digest','observed_epoch','sample_references','assembled_monotonic_ns','companion_deadline_ns','postdecode_clock','original_capture_stamps_ns','local_wall_ages_ns','odom_minus_cloud_ns','disposition','refusal_reason','age_role','operational_authority'}
REF_KEYS={'message_index','topic','serialized_sha256','publisher_gid','source_timestamp_ns','received_timestamp_ns','arrived_monotonic_ns'}


class Invalid(ValueError):pass

def need(ok,code):
    if not ok:raise Invalid(code)

def integer(x,low=0,high=(1<<63)-1):
    need(type(x) is int and low<=x<=high,'integer_bound');return x

def obj(x,keys,code):need(type(x) is dict and set(x)==keys,code)

def digest(x):return hashlib.sha256(x).hexdigest()

def same(a,b):return json.dumps(a,sort_keys=True,separators=(',',':'))==json.dumps(b,sort_keys=True,separators=(',',':'))

def hashvalue(x):need(type(x) is str and re.fullmatch('[0-9a-f]{64}',x) is not None,'digest_shape')

def unhex(x,maximum):
    need(type(x) is str and len(x)<=2*maximum and len(x)%2==0 and re.fullmatch('[0-9a-f]*',x) is not None,'hex_bound')
    return bytes.fromhex(x)

def strict(raw):
    need(type(raw) is bytes and 0<len(raw)<=MAX_REPORT,'report_bound')
    def pairs(items):
        out={}
        for k,v in items:need(k not in out,'duplicate_field');out[k]=v
        return out
    def whole(s):need(len(s)<=20,'integer_text_bound');return integer(int(s),-(1<<63))
    def bad(_):raise Invalid('noninteger_or_nonfinite_number')
    try:return json.loads(raw,object_pairs_hook=pairs,parse_int=whole,parse_float=bad,parse_constant=bad)
    except (ValueError,UnicodeError,RecursionError):raise Invalid('invalid_json') from None


def clock(c,base=None):
    obj(c,{'monotonic_before_ns','wall_ns','monotonic_after_ns'},'clock_shape')
    before=integer(c['monotonic_before_ns']);after=integer(c['monotonic_after_ns']);wall=integer(c['wall_ns'],1)
    need(before<=after and after-before<=50_000_000,'clock_bracket')
    if base:
        width=after-before+base['monotonic_after_ns']-base['monotonic_before_ns']
        delta=(wall-base['wall_ns'])-(after-base['monotonic_after_ns'])
        need(abs(delta)<=50_000_000+width,'recorded_clock_divergence')
    return after


def verify(raw):
    r=strict(raw);obj(r,TOP_KEYS,'report_schema_fields')
    need(r['schema']=='wendy.world.go2-cloud-shadow.v1' and r['simulation'] is True and r['operational_authority'] is False,'report_scope')
    need(r['authorization_status']=='not_evaluated' and r['clock_status']=='uncalibrated_local_machine_observations','authority_or_clock_claim')
    need(type(r['capture_complete']) is bool and type(r['helper_eof']) is bool,'report_boolean')
    for name in ('cloud_count','serialized_bytes','retained_candidates','refused_candidates','late_unassembled_rows','stdout_bytes'):
        integer(r[name])
    need(r['commands_published']==0 and type(r['commands_published']) is int and r['runtime_http_calls']==0 and type(r['runtime_http_calls']) is int,'control_claim')
    need(r['source_digest']==SOURCE_DIGEST and r['helper']=='/opt/world/world-go2-cloud-shadow','source_or_helper')
    epoch=integer(r['expected_epoch'],1);duration=integer(r['duration_ms'],100,60000)*1_000_000
    maximum=integer(r['max_clouds'],1,128);start=clock(r['start_clock']);end=integer(r['run_deadline_ns'])
    need(end==start+duration,'run_deadline_changed')
    startup=integer(r['startup_deadline_ns']);need(start<startup<=min(end,start+5_000_000_000),'startup_bound')
    companion=integer(r['companion_timeout_ns'],1,min(duration,100_000_000))
    clocks=[r['start_clock']];clock(r['last_clock'],r['start_clock']);clocks.append(r['last_clock'])
    need(r['error'] is None or type(r['error']) is str and 0<len(r['error'])<=96,'error_shape')
    obj(r['admission_closed'],{'reason','monotonic_ns'},'admission_closed_shape')
    closed=integer(r['admission_closed']['monotonic_ns']);need(type(r['admission_closed']['reason']) is str,'admission_closed_reason')
    expected_limits={'row_bytes':MAX_ROW,'raw_bytes':MAX_RAW,'stdout_bytes':40<<20,'stderr_bytes':64<<10,'messages':MAX_MESSAGES,'report_bytes':MAX_REPORT,'events':1024,'local_age_max_ns_inclusive':250_000_000,'clock_divergence_ns':50_000_000,'cleanup_allowance_ns':1_000_000_000}
    need(same(r['limits'],expected_limits),'limits_changed')
    rows=r['messages'];need(type(rows) is list and len(rows)<=MAX_MESSAGES,'message_bound')
    assembler=Assembler(SOURCE_DIGEST,epoch,start_ns=start,startup_timeout_ns=startup-start,run_timeout_ns=duration,companion_timeout_ns=companion)
    emitted={};emission_rows={};cloud_indices={};replay_unavailable={};ref_lookup={};rawbytes=0;cloud_count=0;last_arrival=start;last_post=start;last_helper=0;gids={};sources={};replay_fault=None;minimum_stdout=0
    def consume(events,row_index=None):
        nonlocal replay_fault
        for event in events:
            if event.kind=='ready':
                need(event.capture_id not in emitted,'duplicate_replay_candidate');emitted[event.capture_id]=event.candidate
                emission_rows[event.capture_id]=row_index
            elif event.kind=='unavailable':replay_unavailable[event.capture_id]=event.reason
            elif event.kind=='fatal':replay_fault=event.reason
    for i,saved in enumerate(rows):
        obj(saved,SAVED_KEYS,'saved_row_fields');need(saved['index']==i and type(saved['index']) is int,'message_index')
        row=saved['row'];obj(row,ROW_KEYS,'native_row_fields')
        need(row['kind']=='sample' and row['topic'] in TOPICS,'native_sample')
        topic=row['topic'];body=unhex(row['payload_hex'],TOPICS[topic]);rawbytes+=len(body)
        need(0<len(body) and rawbytes<=MAX_RAW,'raw_bound')
        need(saved['serialized_bytes']==len(body) and type(saved['serialized_bytes']) is int,'serialized_size')
        need(saved['serialized_sha256']==digest(body),'serialized_hash');hashvalue(saved['line_sha256'])
        arrival=clock(saved['arrival_clock'],r['start_clock']);clocks.append(saved['arrival_clock'])
        need(max(last_arrival,last_post)<=saved['arrival_clock']['monotonic_before_ns'] and arrival<end and arrival>=start,'arrival_deadline_or_order');last_arrival=arrival
        need(saved['admission']=='submitted_to_assembler','late_row_admitted')
        if saved['postdecode_clock'] is not None:
            post=clock(saved['postdecode_clock'],r['start_clock']);clocks.append(saved['postdecode_clock']);need(saved['postdecode_clock']['monotonic_before_ns']>=arrival,'postdecode_before_arrival');last_post=post
        else:need(r['error'] is not None and i==len(rows)-1,'missing_postdecode_without_failure')
        gid=row['publisher_gid'];need(type(gid) is str and re.fullmatch('[0-9a-f]{48}',gid) is not None and gid!='0'*48,'publisher_gid')
        source=integer(row['source_timestamp_ns'],1);integer(row['received_timestamp_ns']);elapsed=integer(row['helper_elapsed_ns'])
        need(gids.get(topic,gid)==gid and source>sources.get(topic,0) and last_helper<=elapsed<duration,'native_metadata_order')
        gids[topic]=gid;sources[topic]=source;last_helper=elapsed
        ref_lookup[(topic,digest(body),arrival,source)]=i
        if topic=='/utlidar/cloud':cloud_count+=1;cloud_indices[cloud_count]=i
        need(cloud_count<=maximum,'cloud_limit')
        minimum_stdout+=len(json.dumps(row,separators=(',',':')).encode())+1
        consume(assembler.ingest(row,arrival),i)
    need(r['cloud_count']==cloud_count and type(r['cloud_count']) is int and r['serialized_bytes']==rawbytes,'capture_counts')
    need(r['late_unassembled_rows']==0 and type(r['late_unassembled_rows']) is int,'late_rows')
    integer(r['stdout_bytes'],0,(40<<20)+65536)
    need(type(r['events']) is list and len(r['events'])<=1024,'event_bound')
    for event in r['events']:
        obj(event,{'kind','reason','capture_id','observed_monotonic_ns'},'event_fields')
        need(event['kind'] in {'assembler_ready','unavailable','fatal','closed'} and type(event['reason']) is str and len(event['reason'])<=96,'event_shape')
        integer(event['observed_monotonic_ns'])
        if event['capture_id'] is not None:integer(event['capture_id'],1,maximum)
    candidates=r['candidates'];need(type(candidates) is list and len(candidates)<=maximum,'candidate_bound')
    seen=set();retained=0;first_retained=None;dispositions={};gaps=[];last_candidate_row=-1;last_candidate_check=start
    def reference(ref,sample):
        obj(ref,REF_KEYS,'reference_fields')
        key=(sample.topic,sample.serialized_sha256,sample.arrived_ns,sample.source_timestamp_ns)
        need(key in ref_lookup,'sample_not_in_capture');index=ref_lookup[key]
        expected={'message_index':index,'topic':sample.topic,'serialized_sha256':sample.serialized_sha256,'publisher_gid':sample.publisher_gid,'source_timestamp_ns':sample.source_timestamp_ns,'received_timestamp_ns':sample.received_timestamp_ns,'arrived_monotonic_ns':sample.arrived_ns}
        need(same(ref,expected),'sample_reference_changed')
        need(sample.raw_cdr==bytes.fromhex(rows[index]['row']['payload_hex']),'sample_bytes_changed')
    for c in candidates:
        obj(c,CANDIDATE_KEYS,'candidate_fields');cid=integer(c['capture_id'],1,maximum)
        need(cid not in seen and cid in emitted,'candidate_not_unique_original');seen.add(cid);original=emitted[cid]
        for name in ('observed_epoch','assembled_monotonic_ns','companion_deadline_ns'):integer(c[name])
        integer(c['odom_minus_cloud_ns'],-50_000_000,50_000_000)
        need(c['source_digest']==SOURCE_DIGEST and c['observed_epoch']==epoch and c['operational_authority'] is False,'candidate_scope')
        refs=c['sample_references'];obj(refs,{'epoch_sample','cloud','scan','odom','transforms'},'reference_names')
        for name in ('epoch_sample','cloud','scan','odom'):reference(refs[name],getattr(original,name))
        need(type(refs['transforms']) is list and len(refs['transforms'])==2,'mount_reference_count')
        for ref,sample in zip(refs['transforms'],original.transforms):reference(ref,sample)
        need(refs['epoch_sample']['message_index']<refs['cloud']['message_index'],'epoch_backfill')
        need(c['assembled_monotonic_ns']==original.assembled_at_ns,'assembly_time_changed')
        need(c['companion_deadline_ns']==original.companion_deadline_ns==min(original.cloud.arrived_ns+companion,end),'companion_deadline_changed')
        checked=clock(c['postdecode_clock'],r['start_clock']);clocks.append(c['postdecode_clock'])
        trigger=emission_rows[cid]
        need(type(trigger) is int and trigger>=last_candidate_row,'candidate_emission_order')
        decoded=rows[trigger]['postdecode_clock'];need(decoded is not None,'candidate_missing_decoder_clock')
        need(c['postdecode_clock']['monotonic_before_ns']>=max(decoded['monotonic_after_ns'],last_candidate_check),'candidate_clock_precedes_decode')
        if trigger+1<len(rows):need(checked<=rows[trigger+1]['arrival_clock']['monotonic_before_ns'],'candidate_checked_after_next_row')
        last_candidate_row=trigger;last_candidate_check=checked
        stamps={'cloud':original.cloud.value.capture_unix_ns,'scan':original.scan.value['capture_unix_ns'],'odom':original.odom.value['capture_unix_ns']}
        ages={key:c['postdecode_clock']['wall_ns']-value for key,value in stamps.items()}
        need(same(c['original_capture_stamps_ns'],stamps) and same(c['local_wall_ages_ns'],ages),'age_or_original_stamp_changed')
        need(c['odom_minus_cloud_ns']==original.odom_minus_cloud_ns,'odom_pair_changed')
        need(c['age_role']=='uncalibrated_local_machine_wall_comparison','age_authority_claim')
        need(c['disposition'] in {'retained','refused'},'candidate_disposition')
        if c['disposition']=='retained':
            need(c['refusal_reason'] is None and checked<end and checked<original.companion_deadline_ns and (first_retained is not None or checked<startup),'retained_after_deadline')
            need(all(0<=age<=250_000_000 for age in ages.values()),'retained_stale_or_future')
            if first_retained is None:first_retained=checked
            retained+=1
        else:
            reason=c['refusal_reason'];need(type(reason) is str and reason,'refusal_reason')
            expected=None
            if checked>=end:expected='run_deadline'
            elif first_retained is None and checked>=startup:expected='startup_deadline'
            elif checked>=original.companion_deadline_ns:expected='postdecode_companion_deadline'
            elif any(x<0 for x in ages.values()):expected='original_stamp_future_local_wall'
            elif any(x>250_000_000 for x in ages.values()):expected='original_age_exceeds_local_limit'
            need(reason==expected or reason==r['error'] and r['error'] is not None,'unexplained_candidate_refusal')
        dispositions[cid]={'capture_id':cid,'message_index':cloud_indices[cid],'disposition':c['disposition'],'reason':c['refusal_reason']}
    need(r['retained_candidates']==retained and type(r['retained_candidates']) is int and r['refused_candidates']==len(candidates)-retained,'candidate_counts')
    # Reproduce final missing/expired candidates without inventing a companion.
    consume(assembler.poll(r['last_clock']['monotonic_after_ns']))
    consume(assembler.close(r['last_clock']['monotonic_after_ns']))
    for cid,index in cloud_indices.items():
        if cid in dispositions:continue
        reason=replay_unavailable.get(cid)
        if cid in emitted:
            need(r['error'] is not None,'unreported_assembled_candidate')
            reason='assembled_but_suppressed_during_failure';gaps.append('No candidate postcheck record for capture '+str(cid)+'; failure capture cannot establish its completed clock check.')
        elif reason is None:
            need(r['error'] is not None,'missing_cloud_disposition');reason='assembly_stopped_by_failure'
        dispositions[cid]={'capture_id':cid,'message_index':index,'disposition':'unavailable','reason':reason}
    if replay_fault:need(r['error'] is not None,'unreported_assembler_fault')
    need(same(r['assembler_status'],assembler.status()),'assembler_final_status_changed')
    for event in r['events']:
        if event['kind']=='assembler_ready':
            need(event['capture_id'] in emitted and event['reason']=='coherent_originals','invented_ready_event')
            decoded=rows[emission_rows[event['capture_id']]]['postdecode_clock']
            need(decoded is not None and event['observed_monotonic_ns']==decoded['monotonic_after_ns'],'ready_event_decode_time')
    ready_ids=[event['capture_id'] for event in r['events'] if event['kind']=='assembler_ready']
    need(len(set(ready_ids))==len(ready_ids),'duplicate_ready_event')
    if r['capture_complete']:need(ready_ids==list(emitted),'missing_or_reordered_ready_event')
    done=r['helper_completion']
    if done is not None:
        obj(done,{'row','arrival_clock','line_sha256'},'completion_fields');hashvalue(done['line_sha256'])
        observed=clock(done['arrival_clock'],r['start_clock']);clocks.append(done['arrival_clock']);need(observed<end,'completion_after_cutoff')
        need(done['arrival_clock']['monotonic_before_ns']>=max(last_arrival,last_post,last_candidate_check),'completion_before_processed_rows')
        d=done['row'];obj(d,{'kind','reason','messages','clouds','serialized_bytes'},'native_completion_fields')
        need(d['kind']=='complete' and d['reason'] in {'max_clouds','duration','capture_limit','interrupted'},'native_completion')
        for name in ('messages','clouds','serialized_bytes'):integer(d[name])
        need(d['messages']==len(rows) and d['clouds']==cloud_count and d['serialized_bytes']==rawbytes,'native_completion_counts')
        if d['reason']=='max_clouds':need(cloud_count==maximum,'native_max_clouds_count')
        minimum_stdout+=len(json.dumps(d,separators=(',',':')).encode())+1
    for name in ('helper_eof_observed_monotonic_ns','helper_exit_observed_monotonic_ns'):
        if r[name] is not None:integer(r[name],start)
    need(r['helper_exit_observed_before_cleanup'] is (r['helper_exit_observed_monotonic_ns'] is not None),'exit_observation_flag')
    eof_time=r['helper_eof_observed_monotonic_ns'];exit_time=r['helper_exit_observed_monotonic_ns']
    if not r['helper_eof']:need(eof_time is None,'eof_time_without_eof')
    observed_work=max(last_arrival,last_post,last_candidate_check,done['arrival_clock']['monotonic_after_ns'] if done else start)
    if eof_time is not None:need(observed_work<=eof_time<=r['last_clock']['monotonic_after_ns'],'eof_observation_order')
    if exit_time is not None:need(max(observed_work,eof_time if eof_time is not None else start)<=exit_time<=r['last_clock']['monotonic_after_ns'],'exit_observation_order')
    cleanup=r['cleanup'];need(type(cleanup) is dict and set(cleanup)=={'actions','reaped','exit_code'},'cleanup_shape')
    need(cleanup['actions'] in ([],['terminate'],['terminate','kill']) and type(cleanup['reaped']) is bool,'cleanup_actions')
    if cleanup['exit_code'] is not None:integer(cleanup['exit_code'],-255,255)
    need(cleanup['exit_code']==r['helper_exit_code'],'cleanup_exit_changed')
    if r['helper_exit_code'] is not None:integer(r['helper_exit_code'],-255,255)
    if r['capture_complete']:
        need(r['error'] is None and done is not None and done['row']['reason'] in {'max_clouds','duration'} and cloud_count>0,'false_capture_complete')
        need(r['helper_eof'] is True and r['helper_exit_code']==0 and cleanup['reaped'] is True and cleanup['exit_code']==0,'incomplete_process_success')
        need(r['helper_eof_observed_monotonic_ns'] is not None and r['helper_exit_observed_monotonic_ns'] is not None and r['helper_eof_observed_monotonic_ns']<end and r['helper_exit_observed_monotonic_ns']<end,'late_or_unknown_process_success')
        need(cleanup['actions']==[],'cleanup_interrupted_success')
        need(r['admission_closed']['reason']=='capture_finished' and closed==r['last_clock']['monotonic_after_ns'],'complete_admission_close')
    else:need(r['error'] is not None or done is not None and done['row']['reason'] in {'capture_limit','interrupted'} or cloud_count==0,'unexplained_incomplete_capture')
    need(start<=closed<=r['last_clock']['monotonic_after_ns'],'admission_close_order')
    need(r['admission_closed']['reason']==('capture_finished' if r['error'] is None else r['error']),'admission_close_reason')
    need(type(r['scope']) is list and len(r['scope'])<=16 and all(type(v) is str and len(v)<=512 for v in r['scope']),'scope_bound')
    unhex(r['stderr_hex'],64<<10);tail=unhex(r['partial_stdout_hex'],MAX_ROW)
    if r['stderr_observed_bytes'] is not None:integer(r['stderr_observed_bytes'],len(r['stderr_hex'])//2,128<<10)
    tail_size=integer(r['partial_stdout_observed_bytes'],0,MAX_ROW+65536);need(type(r['partial_stdout_truncated']) is bool and r['partial_stdout_truncated']==(tail_size>MAX_ROW),'tail_truncation')
    if not r['partial_stdout_truncated']:need(len(tail)==tail_size and digest(tail)==r['partial_stdout_sha256'],'tail_hash')
    else:hashvalue(r['partial_stdout_sha256']);gaps.append('Only bounded partial stdout prefix retained; full-tail digest cannot be independently recomputed.')
    terminal=r['terminal_input']
    if terminal is not None:
        obj(terminal,{'prefix_hex','observed_bytes','sha256','truncated'},'terminal_input_shape')
        prefix=unhex(terminal['prefix_hex'],MAX_ROW);size=integer(terminal['observed_bytes'],0,MAX_ROW+65536)
        need(terminal['truncated'] is (size>MAX_ROW),'terminal_truncation')
        if not terminal['truncated']:need(size==len(prefix) and terminal['sha256']==digest(prefix),'terminal_hash')
        else:hashvalue(terminal['sha256']);gaps.append('Terminal input exceeds retained prefix; original full bytes are unavailable.')
    need(r['stdout_bytes']>=minimum_stdout+tail_size,'stdout_count_below_retained_bytes')
    if r['capture_complete']:need(tail_size==0 and terminal is None,'terminal_bytes_in_success')
    # Successful clock observations must be internally ordered, without claiming
    # anything about the omitted value that caused a reported clock failure.
    need(r['last_clock']['monotonic_after_ns']>=max(c['monotonic_after_ns'] for c in clocks),'last_clock_precedes_recorded_work')
    clocks.sort(key=lambda c:(c['monotonic_after_ns'],c['monotonic_before_ns'],c['wall_ns']))
    need(all(a['wall_ns']<=b['wall_ns'] for a,b in zip(clocks,clocks[1:])),'recorded_wall_regression')
    return {'schema':'wendy.world.go2-cloud-shadow-verification.v1','integrity_valid':True,
            'report_sha256':digest(raw),'capture_complete':r['capture_complete'],'capture_error':r['error'],
            'messages':len(rows),'clouds':cloud_count,'serialized_bytes':rawbytes,
            'retained_candidates':retained,'refused_candidates':len(candidates)-retained,
            'unavailable_clouds':sum(v['disposition']=='unavailable' for v in dispositions.values()),
            'cloud_dispositions':[dispositions[k] for k in sorted(dispositions)],
            'operational_authority':False,'gaps':gaps,
            'limits':['Native line hashes cannot be reconstructed from parsed row fields; original CDR hashes and semantic lineage were checked.',
                      'Clock observations are local machine comparisons, not calibrated time, authenticated runtime identity or epoch-in-cloud proof.',
                      'Failure-causing invalid clock readings are not retained; only the prior successful observations can be verified.',
                      'Code/container identity requires separate release receipts. This verifier makes no geometry, control or motion claim.']}


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--report',type=Path,required=True);args=parser.parse_args(argv)
    try:
        fd=os.open(args.report,os.O_RDONLY|os.O_NONBLOCK|os.O_NOFOLLOW)
        with os.fdopen(fd,'rb') as f:
            info=os.fstat(f.fileno());need(stat.S_ISREG(info.st_mode) and 0<info.st_size<=MAX_REPORT,'bounded_regular_report')
            raw=f.read(MAX_REPORT+1);need(len(raw)==info.st_size,'report_changed_or_exceeded_bound')
        result=verify(raw)
        print(json.dumps(result,sort_keys=True,separators=(',',':'),allow_nan=False));return 0
    except (ValueError,KeyError,TypeError,OverflowError,RecursionError,OSError) as error:
        code=str(error) if isinstance(error,Invalid) else 'report_integrity_refused'
        if len(code)>96 or re.fullmatch('[a-z0-9_]+',code) is None:code='report_integrity_refused'
        print(json.dumps({'schema':'wendy.world.go2-cloud-shadow-verification.v1','integrity_valid':False,'error':code},sort_keys=True));return 1


if __name__=='__main__':raise SystemExit(main())
