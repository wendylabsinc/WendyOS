"""Host-side fixture regressions; no Docker process or network is started."""
import copy
import contextlib
import io
import json
import hashlib
import os
import stat
import subprocess
import sys
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from supervision_load import collect_measurement, configure_repair, main, process_time_ms, receipt_log_summary, stop_timing_report, supervision_delta_samples_present, terminal_diagnostic, quiet_preload_window, preload_phase_metrics, preload_verification_report
from test_supervision_load_metrics import snapshot, later, observe


class ProcessTimestampTests(unittest.TestCase):
    def test_docker_zero_millisecond_microsecond_and_nanosecond_fractions(self):
        for timestamp,expected in (
            ("2023-11-14T22:13:20Z",1_700_000_000_000),
            ("2023-11-14T22:13:20.500Z",1_700_000_000_500),
            ("2023-11-14T22:13:20.500123Z",1_700_000_000_500),
            ("2023-11-14T22:13:20.500123987Z",1_700_000_000_500),
            ("2023-11-14T22:13:20.000000001Z",1_700_000_000_000),
            ("2023-11-14T22:13:20.999999999Z",1_700_000_000_999),
        ):
            with self.subTest(timestamp=timestamp):
                self.assertEqual(process_time_ms(timestamp),expected)

    def test_explicit_timezone_offsets_are_applied(self):
        for timestamp in (
            "2023-11-14T22:13:20.500000000+00:00",
            "2023-11-15T00:43:20.500000000+02:30",
            "2023-11-14T16:43:20.500000000-05:30",
        ):
            with self.subTest(timestamp=timestamp):
                self.assertEqual(process_time_ms(timestamp),1_700_000_000_500)
        self.assertEqual(process_time_ms("1969-12-31T23:59:59.999999999Z"),-1)

    def test_invalid_unbounded_and_ambiguous_timestamps_are_refused(self):
        for value in (None,123,"","2023-11-14T22:13:20","2023-11-14T22:13:20.1234567890Z",
                      "2023-11-14T22:13:20.-1Z","2023-11-14T22:13:20-00:00",
                      "2023-11-14T22:13:20+24:00","2023-11-14T22:13:20+01:60",
                      "2023-02-30T22:13:20Z","2023-11-14T22:13:60Z",
                      "2023-11-14T22:13:20Z trailing", "x"*10000):
            with self.subTest(value=repr(value)[:80]):
                with self.assertRaises(ValueError): process_time_ms(value)

    def test_nanosecond_exit_preserves_valid_pre_window_snapshot(self):
        before=snapshot()
        state={"Running":False,"FinishedAt":"2023-11-14T22:13:20.005123456Z"}
        result=collect_measurement(before,copy.deepcopy(before),state,1_700_000_000_010,1_700_000_024_000)
        self.assertTrue(result["complete"])
        self.assertEqual(result["status"],"process_exited_before_window")
        self.assertFalse(result["window_delta_available"])
        self.assertEqual(result["lifetime_after"],before)

    def test_diagnostics_are_static_classes_without_raw_text(self):
        result=terminal_diagnostic(b"secret-response-marker\nsupervision challenge exchange failed (HTTP 409)\n")
        self.assertEqual(result,{"class":"http_refusal","exchange":"challenge","http_status":409,"diagnostics_error_seen":False})


class StartupControlTests(unittest.TestCase):
    def receipt(self, challenge="a", issued=1000):
        return {"schema":"wendy.world.supervision-lease.v1","domain":"simulation","run_id":"run",
                "task_id":"load-mission","operator_principal":"lab/supervisor-a","gateway_principal":"lab/node-1",
                "executor_epoch":1,"challenge_id":challenge*64,"ack_sha256":"b"*64,"spec_sha256":"c"*64,
                "issued_at_unix_ms":issued,"expires_at_unix_ms":issued+500}

    def test_receipts_count_unique_cli_successes_by_signed_issue_time(self):
        first=self.receipt(issued=999)
        second=self.receipt("b",issued=1200)
        third=self.receipt("c",issued=2001)
        data="\n".join(json.dumps(value) for value in (first,second,second,third)).encode()
        result=receipt_log_summary(data,"run","lab/supervisor-a",1000,2000)
        self.assertTrue(result["complete"])
        self.assertEqual(result["successful_receipts"],3)
        self.assertEqual(result["challenge_issued_in_window_count"],1)
        self.assertEqual(result["duplicate_receipt_lines"],1)
        self.assertEqual(result["first_challenge_issued_at_unix_ms"],999)
        self.assertEqual(result["last_receipt_expires_at_unix_ms"],2501)
        self.assertNotIn("challenge_id",result)

    def test_wrong_context_and_malformed_receipts_do_not_count(self):
        for mutate in (
            lambda v:v.update(run_id="other"), lambda v:v.update(operator_principal="other"),
            lambda v:v.update(domain="physical"), lambda v:v.update(gateway_principal="other"),
            lambda v:v.update(expires_at_unix_ms=1600), lambda v:v.update(executor_epoch=True),
            lambda v:v.update(issued_at_unix_ms=False), lambda v:v.update(ack_sha256="private-marker"),
        ):
            value=self.receipt();mutate(value)
            result=receipt_log_summary(json.dumps(value).encode(),"run","lab/supervisor-a",1000,2000)
            self.assertFalse(result["complete"])
            self.assertEqual(result["successful_receipts"],0)
            self.assertNotIn("private-marker",json.dumps(result))
        self.assertFalse(receipt_log_summary(b"x"*((1<<20)+1),"run","lab/supervisor-a",0,1)["complete"])
        self.assertFalse(receipt_log_summary(b"line\n"*501,"run","lab/supervisor-a",0,1)["complete"])

    def test_snapshot_interval_alone_is_not_supervision_samples(self):
        empty={"window_delta_available":True,"durations":[{"name":"supervisor.exchange","count":0}]}
        self.assertFalse(supervision_delta_samples_present({"a":empty,"b":empty}))
        sampled=copy.deepcopy(empty);sampled["durations"][0]["count"]=1
        self.assertFalse(supervision_delta_samples_present({"a":sampled,"b":empty}))
        self.assertTrue(supervision_delta_samples_present({"a":sampled,"b":sampled}))

    def test_control_pair_needs_only_normal_images_and_no_publisher(self):
        image_calls=[];cells=[]
        def fake_command(args,**kwargs):
            image_calls.append(args)
            return b"sha256:test-image\n"
        def fake_cell(args,storage,profile,diagnostics):
            cells.append((storage,profile,diagnostics))
            self.assertIsNone(args.publisher_binary)
            self.assertIsNone(args.fault_node_image)
            return {"result":"measured"}
        out=io.StringIO()
        with patch("supervision_load.command",side_effect=fake_command),patch("supervision_load.run_cell",side_effect=fake_cell),contextlib.redirect_stdout(out),contextlib.redirect_stderr(io.StringIO()):
            main(["--startup-control","--storage","host-bind"])
        self.assertEqual(cells,[("host-bind","startup-control",True),("host-bind","startup-control",False)])
        self.assertEqual(len(image_calls),2)
        self.assertTrue(all(call[:3]==["docker","image","inspect"] for call in image_calls))
        self.assertEqual(json.loads(out.getvalue())["schema"],"wendy.world.supervision-startup-control.v1")

    def test_control_refuses_fault_profile_before_any_docker(self):
        with patch("supervision_load.command") as command,contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as raised:
                main(["--startup-control","--profile","disk-delay"])
            self.assertEqual(raised.exception.code,2)
            command.assert_not_called()


class RepairControlTests(unittest.TestCase):
    def test_repair_off_changes_only_configured_peers(self):
        original={"peers":[{"url":"http://node2:8080"}],"listen":":8080","tasking":{"run_id":"run"},"diagnostics":{"path":"/metrics/new.json"}}
        self.assertEqual(configure_repair(copy.deepcopy(original),True),original)
        changed=configure_repair(copy.deepcopy(original),False)
        self.assertEqual(changed,{**original,"peers":[]})

    def test_repair_pair_requires_helper_and_uses_only_normal_images(self):
        calls=[];cells=[]
        def fake_command(args,**kwargs): calls.append(args);return b"sha256:normal\n"
        def fake_cell(args,storage,profile,diagnostics,repair):
            cells.append((storage,profile,diagnostics,repair))
            self.assertIsNone(args.fault_node_image)
            return {"result":"measured"}
        with tempfile.TemporaryDirectory() as folder:
            helper=Path(folder)/"helper";helper.write_bytes(b"fixture");helper.chmod(0o700)
            out=io.StringIO()
            with patch("supervision_load.command",side_effect=fake_command),patch("supervision_load.run_cell",side_effect=fake_cell),contextlib.redirect_stdout(out),contextlib.redirect_stderr(io.StringIO()):
                main(["--repair-control","--storage","vm-volume","--publisher-binary",str(helper),"--stop-binary",str(helper)])
        self.assertEqual(cells,[("vm-volume","repair-control",True,True),("vm-volume","repair-control",True,False)])
        self.assertEqual(len(calls),2)
        self.assertEqual(json.loads(out.getvalue())["schema"],"wendy.world.supervision-repair-control.v1")

    def test_conflicting_modes_and_missing_stop_refused_before_docker(self):
        for args in (["--repair-control","--startup-control"],["--repair-control"],["--repair-mode","off"],
                     ["--repair-control","--profile","disk-delay"],["--repair-control","--control-diagnostics","off"]):
            with self.subTest(args=args),patch("supervision_load.command") as command,contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit) as raised:main(args)
                self.assertEqual(raised.exception.code,2);command.assert_not_called()

    def report(self):
        return {"schema":"wendy.world.stop-timing.v1","domain":"simulation","request_attempted":True,
                "response_matches_task":True,"outcome":"terminal_response","returned_phase":"canceled","error_class":"","http_status":0,
                "initialization_ns":10,"instruction_ns":20,"request_ns":3000,"response_validation_ns":10,"total_ns":3050,"request_deadline_ns":2_000_000_000}

    def test_stop_timing_separates_host_duration_and_unknown_outcome(self):
        value=self.report()
        result=stop_timing_report(json.dumps(value).encode(),0,50_000_000)
        self.assertEqual(result["request_ns"],3000)
        self.assertEqual(result["host_docker_elapsed_ns"],50_000_000)
        value.update(response_matches_task=False,outcome="unknown",returned_phase="",error_class="timeout",request_ns=2_000_000_000,total_ns=2_000_000_100)
        result=stop_timing_report(json.dumps(value).encode(),1,2_100_000_000)
        self.assertEqual(result["outcome"],"unknown")
        self.assertFalse(result["response_matches_task"])

    def test_stop_report_rejects_raw_fields_and_inconsistent_confirmation(self):
        for mutation in ({"private_key":"secret"},{"domain":"physical"},{"request_ns":True},{"outcome":"unknown"},
                         {"error_class":"private-marker"},{"request_attempted":False},{"total_ns":1},{"request_deadline_ns":3_000_000_000}):
            value=self.report();value.update(mutation)
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):
                stop_timing_report(json.dumps(value).encode(),0,50_000_000)
        with self.assertRaises(ValueError):stop_timing_report(b"x"*16385,1,50_000_000)



class PreloadControlTests(unittest.TestCase):
    def test_verification_failure_report_keeps_only_fixed_context_and_class(self):
        failure={"schema":"wendy.world.preload-verification-failure.v1","domain":"simulation","run_id":"run",
                 "node_index":2,"count":200,"failure_class":"clock_regression"}
        self.assertEqual(preload_verification_report(json.dumps(failure).encode(),1,"run",2,200),failure)
        for extra in ({"failure_class":"secret-message"},{"node_index":True},{"count":False},{"domain":"physical"},{"raw":"secret-message"}):
            value={**failure,**extra}
            with self.subTest(extra=extra),self.assertRaises(ValueError):preload_verification_report(json.dumps(value).encode(),1,"run",2,200)
        with self.assertRaises(ValueError):preload_verification_report(json.dumps(failure).encode(),0,"run",2,200)
        success={"schema":"wendy.world.preload-verification.v1","domain":"simulation","run_id":"run","node_index":2,"count":200,
                 "epoch":1,"verified_at_unix_ms":1000,"minimum_remaining_ms":60000,"exact_signed_set":True}
        self.assertEqual(preload_verification_report(json.dumps(success).encode(),0,"run",2,200),success)
        with self.assertRaises(ValueError):preload_verification_report(json.dumps(success).encode(),1,"run",2,200)

    def test_full_control_has_eight_fixed_cells_and_only_normal_images(self):
        calls=[];cells=[]
        def fake_command(args,**kwargs):calls.append(args);return b"sha256:normal\n"
        def fake_cell(args,storage,profile,diagnostics,repair,count):
            cells.append((storage,profile,diagnostics,repair,count));return {"result":"measured"}
        with tempfile.TemporaryDirectory() as folder:
            helper=Path(folder)/"helper";helper.write_bytes(b"fixture");helper.chmod(0o700)
            out=io.StringIO()
            with patch("supervision_load.command",side_effect=fake_command),patch("supervision_load.run_cell",side_effect=fake_cell),contextlib.redirect_stdout(out),contextlib.redirect_stderr(io.StringIO()):
                main(["--preload-control","--publisher-binary",str(helper),"--stop-binary",str(helper),"--gate-binary",str(helper)])
        self.assertEqual(cells,[(storage,"preload-control",True,repair,count) for storage in ("host-bind","vm-volume") for count in (0,200) for repair in (True,False)])
        self.assertEqual(len(calls),2)
        self.assertEqual(json.loads(out.getvalue())["schema"],"wendy.world.supervision-preload-control.v1")

    def test_preload_smoke_filters_and_invalid_modes(self):
        with tempfile.TemporaryDirectory() as folder:
            helper=Path(folder)/"helper";helper.write_bytes(b"fixture");helper.chmod(0o700)
            common=["--preload-control","--publisher-binary",str(helper),"--stop-binary",str(helper),"--gate-binary",str(helper)]
            with patch("supervision_load.command",return_value=b"sha256:normal\n"),patch("supervision_load.run_cell",return_value={"result":"measured"}) as run,contextlib.redirect_stdout(io.StringIO()),contextlib.redirect_stderr(io.StringIO()):
                main(common+["--storage","host-bind","--repair-mode","on","--preload-records","200"])
                self.assertEqual(run.call_args.args[1:],("host-bind","preload-control",True,True,200))
            for args in (common+["--profile","disk-delay"],common+["--startup-control"],common+["--repair-control"],
                         ["--preload-records","200"],["--preload-control"],common+["--control-diagnostics","off"]):
                with self.subTest(args=args),patch("supervision_load.command") as command,contextlib.redirect_stderr(io.StringIO()):
                    with self.assertRaises(SystemExit) as raised:main(args)
                    self.assertEqual(raised.exception.code,2);command.assert_not_called()

    def test_fixed_window_uses_only_local_snapshot_reads_and_fixed_deadlines(self):
        current=[10.0];reads=[]
        def clock():return current[0]
        def sleep(duration):self.assertGreaterEqual(duration,0);current[0]+=duration
        def read(path):reads.append((str(path),clock()));return snapshot()
        before={"snapshots":{"node1":snapshot()}}
        with patch("supervision_load.bounded_json",side_effect=read),patch("supervision_load.command",side_effect=AssertionError("Docker in window")),patch("supervision_load.subprocess.run",side_effect=AssertionError("subprocess in window")):
            result=quiet_preload_window(10.5,Path("/metrics"),before,clock,sleep)
        self.assertAlmostEqual(current[0],34.5)
        self.assertEqual(set(result["first_supervisor_snapshots"]),{"a","b"})
        self.assertIs(result["before_release"],before)
        self.assertEqual(len(reads),12)
        self.assertTrue(all(at in (10.0,12.5,34.5) for _,at in reads))

    def test_phase_metrics_retain_actual_intervals_without_inventing_zeros(self):
        first=snapshot();second=later(first);observe(second,5000);third=later(second);observe(third,6000)
        bounds={"before_release":{"snapshots":{"node1":first}},"first_supervisor_snapshots":{"a":{"snapshot":first}},
                "at_two_seconds":{"snapshots":{"node1":second,"a":first}},"at_window_end":{"snapshots":{"node1":third,"a":first}}}
        result=preload_phase_metrics(bounds)
        self.assertEqual(result["startup_0_to_2"]["node1"]["status"],"snapshot_interval")
        self.assertEqual(result["later_2_to_24"]["node1"]["durations"][0]["count"],1)
        self.assertEqual(result["later_2_to_24"]["a"]["status"],"identical_snapshot_no_delta")
        self.assertNotIn("durations",result["later_2_to_24"]["a"])
        self.assertEqual(result["startup_0_to_2"]["b"]["status"],"snapshot_unavailable")



class OnsetControlTests(unittest.TestCase):
    def test_interleaved_variants_reverse_storage_and_variant_order(self):
        from supervision_load import onset_order
        self.assertEqual(onset_order(2),[
            (1,'host-bind','fixed'),(1,'host-bind','changing'),(1,'vm-volume','fixed'),(1,'vm-volume','changing'),
            (2,'vm-volume','changing'),(2,'vm-volume','fixed'),(2,'host-bind','changing'),(2,'host-bind','fixed')])
        self.assertEqual(onset_order(1,'vm-volume','changing'),[(1,'vm-volume','changing')])
        with self.assertRaises(ValueError):onset_order(4)

    def experiment(self,live=True,trace=False,armed=True,expires_after_trace=True,trace_at_seconds=None,trigger_fails=False,trace_read_delay=0):
        from supervision_load import quiet_onset_window
        now=[0.0];calls=[]
        def sleep(seconds):now[0]+=seconds
        def observation(*_):
            calls.append(now[0])
            eligible=live and (expires_after_trace or now[0]<2.02)
            return {'eligible':eligible,'operators':{k:{'latest':{'expires_at_unix_ms':1_700_000_000_000+int(now[0]*1000)+400}} for k in ('a','b')}}
        def read(path,*_):
            if path.name=='request-trace.json' and armed:
                now[0]+=trace_read_delay
                return {'schema':'wendy.world.request-trace.v1','run_nonce':'a'*32,'state':'recording','armed_at_unix_ms':1_700_000_000_000+int(now[0]*1000)}
            raise OSError('not yet available')
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            if trigger_fails:(root/'trace-trigger.json').write_text('existing one-shot trigger')
            with patch('supervision_load.onset_live_observation',side_effect=observation),patch('supervision_load.bounded_json',side_effect=read),patch('supervision_load.preload_boundary',return_value={'snapshots':{}}),patch('supervision_load.subprocess.run') as subprocess:
                result=quiet_onset_window(0,root,root,'run','changing',{'snapshots':{}},'a'*32 if trace else None,clock=lambda:now[0],sleep=sleep,wall=lambda:1_700_000_000_000+int(now[0]*1000),trace_at_seconds=trace_at_seconds)
                subprocess.assert_not_called()
            release=json.loads((root/'publication-release.json').read_text())
        self.assertGreaterEqual(now[0],24)
        self.assertLess(now[0],24.1)
        return result,release

    def test_two_seconds_sampled_live_then_single_release_no_docker(self):
        result,release=self.experiment()
        self.assertEqual(result['status'],'released')
        self.assertTrue(release['publish'])
        self.assertGreaterEqual(result['decision_at_seconds'],2)
        self.assertLess(result['decision_at_seconds'],2.1)

    def test_no_warmup_never_retries_or_publishes(self):
        result,release=self.experiment(live=False)
        self.assertEqual(result['status'],'onset_not_exercised')
        self.assertFalse(release['publish'])
        self.assertGreaterEqual(result['decision_at_seconds'],6)
        self.assertLess(result['decision_at_seconds'],6.1)

    def test_unarmed_trace_refuses_publication(self):
        result,release=self.experiment(trace=True,armed=False)
        self.assertFalse(result['trace_ready'])
        self.assertEqual(result['status'],'onset_not_exercised')
        self.assertFalse(release['publish'])

    def test_receipts_resampled_after_trace_wait(self):
        # Delayed trace polling crosses the receipt-expiry boundary.
        result,release=self.experiment(trace=True,armed=False,expires_after_trace=False)
        self.assertFalse(result['after_trace_live_evidence']['eligible'])
        self.assertFalse(release['publish'])

    def test_fixed_offsets_do_not_delay_or_gate_publication(self):
        for offset in (0,6,18):
            for armed in (False,True):
                with self.subTest(offset=offset,armed=armed):
                    result,release=self.experiment(trace=True,trace_at_seconds=offset,armed=armed)
                    self.assertTrue(release['publish'])
                    self.assertGreaterEqual(result['decision_at_seconds'],2)
                    self.assertLess(result['decision_at_seconds'],2.1)
                    trace=result['fixed_trace']
                    self.assertEqual(trace['trigger_attempts'],1)
                    self.assertGreaterEqual(trace['trigger_created_offset_seconds'],offset)
                    self.assertLess(trace['trigger_created_offset_seconds'],offset+.051)
                    self.assertEqual(trace['scheduled_trigger_unix_ms'],1_700_000_000_000+offset*1000)
                    self.assertEqual(trace['status'],'armed_observed' if armed else 'ack_not_observed_within_bound')
                    self.assertIn('at_trigger_observation',trace)
                    if armed:
                        self.assertIn('at_ack_observation',trace)
                        self.assertGreaterEqual(trace['armed_minus_supervisor_release_ms'],offset*1000)
                    if offset==0:
                        self.assertTrue(trace['zero_offset_does_not_cover_requests_before_gateway_arming'])

    def test_fixed_trace_still_triggers_when_warmup_is_withheld(self):
        for offset in (0,6,18):
            with self.subTest(offset=offset):
                result,release=self.experiment(live=False,trace=True,trace_at_seconds=offset)
                self.assertEqual(result['status'],'onset_not_exercised')
                self.assertFalse(release['publish'])
                self.assertEqual(result['fixed_trace']['status'],'armed_observed')
                self.assertFalse(result['fixed_trace']['at_trigger_observation']['eligible'])
                self.assertEqual(result['fixed_trace']['trigger_attempts'],1)

    def test_fixed_trace_trigger_failure_is_not_retried_or_a_publication_gate(self):
        result,release=self.experiment(trace=True,trace_at_seconds=6,trigger_fails=True)
        self.assertTrue(release['publish'])
        self.assertEqual(result['fixed_trace']['status'],'trigger_failed')
        self.assertEqual(result['fixed_trace']['trigger_attempts'],1)
        self.assertEqual(result['fixed_trace']['error_class'],'local_trigger_io')

    def test_fixed_trace_late_file_read_is_labeled_outside_ack_bound(self):
        result,release=self.experiment(trace=True,trace_at_seconds=0,trace_read_delay=.12)
        self.assertTrue(release['publish'])
        self.assertEqual(result['fixed_trace']['status'],'armed_observed_after_bound')
        self.assertFalse(result['fixed_trace']['ack_observed_within_bound'])
        self.assertEqual(result['fixed_trace']['trigger_attempts'],1)

    def test_fixed_collection_has_bounded_local_wait_and_scope_check(self):
        from supervision_load import collect_fixed_trace
        for complete in (False,True):
            now=[0.0]
            def sleep(seconds):now[0]+=seconds
            def read(*_):
                return {'schema':'wendy.world.request-trace.v1','run_nonce':'a'*32,
                        'state':'complete' if complete and now[0]>=.2 else 'recording','armed_at_unix_ms':1_700_000_000_000}
            with patch('supervision_load.bounded_json',side_effect=read),patch('supervision_load.subprocess.run') as subprocess:
                value,report=collect_fixed_trace(Path('trace.json'),'a'*32,clock=lambda:now[0],sleep=sleep)
                subprocess.assert_not_called()
            self.assertEqual(report['complete'],complete)
            self.assertLessEqual(report['wait_seconds'],.751)
            self.assertFalse(report['measurement_window_extended'])
            self.assertEqual(value['state'],'complete' if complete else 'recording')
        with patch('supervision_load.bounded_json',return_value={'schema':'wrong','run_nonce':'a'*32,'state':'complete'}):
            value,report=collect_fixed_trace(Path('trace.json'),'a'*32)
        self.assertIsNone(value)
        self.assertFalse(report['complete'])
        self.assertEqual(report['status'],'scope_or_state_invalid')

    def test_fixed_collection_never_starts_read_at_deadline_and_labels_late_completion(self):
        from supervision_load import collect_fixed_trace
        now=[0.0];starts=[]
        def sleep(seconds):now[0]+=seconds
        def read(*_):
            starts.append(now[0])
            return {'schema':'wendy.world.request-trace.v1','run_nonce':'a'*32,'state':'recording','armed_at_unix_ms':1}
        with patch('supervision_load.bounded_json',side_effect=read):
            _,report=collect_fixed_trace(Path('trace.json'),'a'*32,clock=lambda:now[0],sleep=sleep)
        self.assertTrue(starts)
        self.assertTrue(all(start<.75 for start in starts))
        self.assertFalse(report['read_completed_after_retry_budget'])
        now[0]=0
        def late(*_):
            now[0]=.8
            return {'schema':'wendy.world.request-trace.v1','run_nonce':'a'*32,'state':'complete','armed_at_unix_ms':1}
        with patch('supervision_load.bounded_json',side_effect=late):
            value,report=collect_fixed_trace(Path('trace.json'),'a'*32,clock=lambda:now[0],sleep=sleep)
        self.assertEqual(value['state'],'complete')
        self.assertTrue(report['complete'])
        self.assertTrue(report['read_completed_after_retry_budget'])
        self.assertFalse(report['complete_observed_within_retry_budget'])
        self.assertEqual(report['read_attempts'],1)
        self.assertEqual(report['wait_seconds'],.8)

    def test_fixed_trigger_skips_window_when_descheduled_between_clock_samples(self):
        from supervision_load import quiet_onset_window
        times=iter((23.99,24.01))
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            with patch('supervision_load.trigger_request_trace') as trigger,patch('supervision_load.preload_boundary',return_value={'snapshots':{}}):
                result=quiet_onset_window(0,root,root,'run','changing',{'snapshots':{}},'a'*32,
                    clock=lambda:next(times),sleep=lambda _:self.fail('slept outside window'),wall=lambda:1_700_000_024_010,
                    trace_at_seconds=18,supervisor_release_unix_ms=1_700_000_000_000)
            trigger.assert_not_called()
            self.assertEqual(result['fixed_trace']['status'],'scheduled_offset_missed_window')
            self.assertEqual(result['fixed_trace']['trigger_attempts'],0)
            self.assertFalse((root/'trace-trigger.json').exists())

    def test_fixed_trigger_rechecks_window_after_blocking_observation(self):
        from supervision_load import quiet_onset_window
        now=[18.0]
        def observation(*_):
            now[0]=24.1
            return {'eligible':False,'operators':{}}
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            with patch('supervision_load.trigger_request_trace') as trigger,patch('supervision_load.onset_live_observation',side_effect=observation),patch('supervision_load.preload_boundary',return_value={'snapshots':{}}):
                result=quiet_onset_window(0,root,root,'run','changing',{'snapshots':{}},'a'*32,
                    clock=lambda:now[0],sleep=lambda _:self.fail('slept outside window'),wall=lambda:1_700_000_000_000+int(now[0]*1000),
                    trace_at_seconds=18,supervisor_release_unix_ms=1_700_000_000_000)
            trigger.assert_not_called()
            self.assertEqual(result['fixed_trace']['status'],'scheduled_offset_missed_window')
            self.assertEqual(result['fixed_trace']['trigger_attempts'],0)

    def test_receipt_completion_count_differs_from_issue_time(self):
        from supervision_load import onset_receipt_summary
        value={'receipts':[{'issued_at_unix_ms':990,'expires_at_unix_ms':1490,'observed_at_unix_ms':1010,'observed_elapsed_ns':10}],
            'truncated':False,'invalid_lines':0,'write_failed':False}
        result=onset_receipt_summary(value,1000,1200)
        self.assertEqual(result['receipt_observed_in_window_count'],1)
        self.assertEqual(result['challenge_issued_in_window_count'],0)

    def test_onset_rejects_faults_and_conflicting_controls_before_docker(self):
        for args in (['--onset-control','--startup-control'],['--onset-control','--profile','disk-delay'],['--onset-variant','fixed'],['--onset-control'],
                     ['--onset-trace-at-seconds','0'],['--onset-control','--onset-trace-at-seconds','6'],['--onset-control','--onset-trace','--onset-trace-at-seconds','1']):
            with patch('supervision_load.command') as command,contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit):main(args)
                command.assert_not_called()

    def test_onset_main_keeps_normal_images_and_exact_interleaving(self):
        cells=[]
        def fake_cell(args,storage,profile,diagnostics,repair,count,variant):
            cells.append((storage,variant));self.assertTrue(diagnostics and repair);self.assertEqual(count,200)
            return {'result':'measured'}
        with tempfile.TemporaryDirectory() as directory:
            binary=Path(directory)/'helper';binary.write_bytes(b'fixture');binary.chmod(0o700)
            with patch('supervision_load.command',return_value=b'sha256:test\n') as command,patch('supervision_load.run_cell',side_effect=fake_cell),contextlib.redirect_stdout(io.StringIO()) as output,contextlib.redirect_stderr(io.StringIO()):
                main(['--onset-control','--publisher-binary',str(binary),'--gate-binary',str(binary),'--stop-binary',str(binary)])
            self.assertEqual(command.call_count,2)
            self.assertEqual(len(cells),8)
            self.assertEqual(cells[:2],[('host-bind','fixed'),('host-bind','changing')])
            self.assertEqual(cells[4:6],[('vm-volume','changing'),('vm-volume','fixed')])
            self.assertEqual(json.loads(output.getvalue())['schema'],'wendy.world.supervision-onset-control.v1')


class ConfigRestartReadbackTests(unittest.TestCase):
    def test_stage_config_is_exclusive_private_exact_and_syncs_file_and_parent(self):
        from supervision_load import write_stage_config,config_readback_report
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'config-measured.json'
            with patch('supervision_load.os.fsync',wraps=os.fsync) as sync:
                expected=write_stage_config(path,{'new':True,'nested':{'value':3}})
                self.assertEqual(sync.call_count,2)
            self.assertEqual(stat.S_IMODE(path.stat().st_mode),0o600)
            actual=path.read_bytes()
            self.assertEqual(expected['bytes'],len(actual))
            self.assertEqual(expected['sha256'],hashlib.sha256(actual).hexdigest())
            self.assertEqual(json.loads(actual),{'new':True,'nested':{'value':3}})
            report=self.report(expected)
            self.assertEqual(config_readback_report(json.dumps(report).encode(),expected),report)
            with self.assertRaises(FileExistsError):write_stage_config(path,{'replacement':True})
            self.assertEqual(path.read_bytes(),actual)

    def test_config_scope_and_input_refusals_leave_original_unchanged(self):
        from supervision_load import write_stage_config
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'config.json';path.write_bytes(b'original');path.chmod(0o600)
            for bad in ({'large':'x'*(1<<20)},{'nonfinite':float('nan')},[]):
                with self.assertRaises(ValueError):write_stage_config(path,bad)
                self.assertEqual(path.read_bytes(),b'original')
            link=Path(directory)/'link';link.symlink_to(path)
            with self.assertRaises(OSError):write_stage_config(link,{'new':True})
            fifo=Path(directory)/'fifo';os.mkfifo(fifo,0o600)
            with self.assertRaises(OSError):write_stage_config(fifo,{'new':True})
            path.chmod(0o644)
            with self.assertRaises(OSError):write_stage_config(path,{'new':True})
            self.assertEqual(path.read_bytes(),b'original')

    def report(self,expected):
        return {'schema':'wendy.world.config-readback.v2','expected_bytes':expected['bytes'],'expected_sha256':expected['sha256'],
                'observed_bytes':expected['bytes'],'observed_sha256':expected['sha256'],'exact':True,'error_class':''}

    def test_readback_failure_retains_mismatch_without_authorizing_restart(self):
        from supervision_load import config_readback_report
        expected={'bytes':2,'sha256':hashlib.sha256(b'{}').hexdigest()}
        mismatch={**self.report(expected),'observed_sha256':'0'*64,'exact':False,'error_class':'byte_mismatch'}
        self.assertEqual(config_readback_report(json.dumps(mismatch).encode(),expected,1),mismatch)
        for bad in ({**mismatch,'exact':True},{**mismatch,'expected_bytes':3},{**mismatch,'observed_bytes':True},
                    {**mismatch,'raw_config':'must-not-be-exported'},{**self.report(expected),'error_class':'byte_mismatch','exact':False}):
            with self.assertRaises(ValueError):config_readback_report(json.dumps(bad).encode(),expected,1)
        with self.assertRaises(ValueError):config_readback_report(json.dumps(mismatch).encode(),expected,0)
        with self.assertRaises(ValueError):config_readback_report(b'x'*1025,expected)

    def test_docker_failure_and_malformed_output_are_distinct_from_byte_mismatch(self):
        from supervision_load import execute_config_readback
        expected={'bytes':2,'sha256':hashlib.sha256(b'{}').hexdigest()}
        for code,kind in ((125,'docker_launch_failed'),(1,'helper_report_missing_or_invalid')):
            result=subprocess.CompletedProcess(['docker'],code,b'',b'secret-config-or-path')
            with patch('supervision_load.subprocess.run',return_value=result):value=execute_config_readback(['docker'],expected)
            self.assertFalse(value['exact']);self.assertEqual(value['error_class'],kind);self.assertEqual(value['helper_exit_code'],code)
            self.assertNotIn('secret',json.dumps(value))
        mismatch={**self.report(expected),'observed_sha256':'0'*64,'exact':False,'error_class':'byte_mismatch'}
        result=subprocess.CompletedProcess(['docker'],1,json.dumps(mismatch).encode(),b'')
        with patch('supervision_load.subprocess.run',return_value=result):value=execute_config_readback(['docker'],expected)
        self.assertEqual(value['error_class'],'byte_mismatch');self.assertEqual(value['report_status'],'validated')
        with patch('supervision_load.subprocess.run',side_effect=subprocess.TimeoutExpired(['docker'],5)):
            self.assertEqual(execute_config_readback(['docker'],expected)['error_class'],'docker_helper_timeout')

    def test_vm_reader_script_rejects_stale_bytes_and_never_prints_config(self):
        # Execute the exact reader against a local temporary file, without Docker.
        from supervision_load import CONFIG_READBACK,config_readback_report
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'config.json';raw=b'{"private_marker":"never-copy"}';path.write_bytes(raw)
            code=CONFIG_READBACK.replace("'/config.json'",repr(str(path)))
            expected={'schema':'wendy.world.config-readback.v1','bytes':len(raw),'sha256':hashlib.sha256(raw).hexdigest(),'exact':True}
            result=subprocess.run([sys.executable,'-c',code,expected['sha256'],str(len(raw))],capture_output=True,timeout=3)
            self.assertEqual(result.returncode,0)
            self.assertNotIn(b'never-copy',result.stdout+result.stderr)
            self.assertEqual(config_readback_report(result.stdout,expected),self.report(expected))
            failed=subprocess.run([sys.executable,'-c',code,'0'*64,str(len(raw))],capture_output=True,timeout=3)
            self.assertNotEqual(failed.returncode,0)
            failure=config_readback_report(failed.stdout,{**expected,'sha256':'0'*64},failed.returncode)
            self.assertEqual(failure['error_class'],'byte_mismatch')
            self.assertEqual(failure['observed_sha256'],expected['sha256'])
            self.assertEqual(failure['observed_bytes'],len(raw))
            self.assertEqual(failed.stderr,b'')
            self.assertNotIn(b'never-copy',failed.stdout)
            path.unlink()
            missing=subprocess.run([sys.executable,'-c',code,expected['sha256'],str(len(raw))],capture_output=True,timeout=3)
            self.assertEqual(config_readback_report(missing.stdout,expected,missing.returncode)['error_class'],'input_unavailable')
            self.assertEqual(missing.stderr,b'')

class ImmutableNodeRecreationTests(unittest.TestCase):
    def fixture(self, directory, control=True, storage='volume'):
        plans={}; originals={}; replacements={}
        def mount(kind, source, destination, writable=True):
            return {'Type':kind,'Name':source if kind=='volume' else '',
                    'Source':source,'Destination':destination,'RW':writable,'Propagation':''}
        for i in (1,2,3):
            old_config=str(Path(directory)/f'node-{i}.json')
            new_config=Path(directory)/f'node-{i}-measured.json'
            network='control' if i==1 else 'peer'; alias=f'world-node-{i}'
            config_mount=f'type=bind,src={old_config},dst=/config.json,readonly'
            flags=['--network-alias',alias,'--mount',f'type={storage},src=state-{i},dst=/state',
                   '--mount',config_mount,'--mount','type=bind,src=metrics,dst=/metrics']
            mounts={'/state':mount(storage,f'state-{i}','/state'),'/metrics':mount('bind','metrics','/metrics'),
                    '/config.json':mount('bind',old_config,'/config.json',False)}
            if control:
                flags+=['--mount','type=bind,src=controls,dst=/control,readonly']
                mounts['/control']=mount('bind','controls','/control',False)
            plans[i]={'name':f'node{i}','user':'501:20','flags':flags,'config_mount':config_mount,
                      'measured_config':new_config,'network':network,'alias':alias,'control_mount':control,
                      'extra_networks':[('peer',alias)] if i==1 else []}
            originals[i]={'container_id':str(i)*64,'image_id':'sha256:'+'a'*64,'user':'501:20',
                          'entrypoint':['/worldd'],'argv':['--config','/config.json'],'mounts':mounts,
                          'process':{'Status':'exited','ExitCode':0,'OOMKilled':False,'FinishedAt':'2026-11-10T12:00:00Z'},
                          'networks':{network:[alias],**({'peer':[alias]} if i==1 else {})}}
            replacements[i]=copy.deepcopy(originals[i])
            replacements[i]['container_id']=str(i+3)*64
            replacements[i]['mounts']['/config.json']['Source']=str(new_config)
        return plans,originals,replacements

    def execute(self, plans, originals, replacements, result, mutate_before_removal=None, mismatch=0):
        from supervision_load import recreate_preload_nodes
        self.events=[]; self.commands=[]; self.inspections={}; created=set()
        def snapshot(name, run_id, created=False):
            self.assertEqual(run_id,'run')
            index=next(i for i,p in plans.items() if name in (p['name'],replacements[i]['container_id']))
            self.inspections[index]=self.inspections.get(index,0)+1
            self.events.append(('inspect',index))
            value=copy.deepcopy(replacements[index] if created else originals[index])
            if mutate_before_removal and self.inspections[index]==2:mutate_before_removal(value)
            return value
        def command(argv, timeout=45):
            self.commands.append(argv); self.events.append(('command',argv[1]))
            if argv[1]=='rm':
                self.assertTrue(all(p['measured_config'].exists() for p in plans.values()))
                self.assertEqual(len(argv),3)
                self.assertIn(argv[2],[o['container_id'] for o in originals.values()])
            if argv[1]=='create':
                name=argv[argv.index('--name')+1]; i=next(i for i,p in plans.items() if p['name']==name)
                created.add(i);return (replacements[i]['container_id']+'\n').encode()
            return b''
        def verify(index, expected):
            self.assertEqual(created,{1,2,3})
            self.assertFalse(any(c[1]=='start' for c in self.commands))
            self.events.append(('verify',index))
            return {'exact':index!=mismatch,'expected_bytes':expected['bytes'],'expected_sha256':expected['sha256'],
                    'error_class':'byte_mismatch' if index==mismatch else ''}
        with patch('supervision_load.node_recreation_snapshot',side_effect=snapshot),patch('supervision_load.command',side_effect=command):
            recreate_preload_nodes(plans,'run',{i:{'stage':'measured','index':i} for i in plans},result,verify,lambda value:None)

    def test_uniform_recreation_preserves_state_image_user_networks_and_checks_all_before_start(self):
        for controls,storage in ((False,'volume'),(True,'volume'),(False,'bind'),(True,'bind')):
            with self.subTest(controls=controls,storage=storage),tempfile.TemporaryDirectory() as directory:
                plans,old,new=self.fixture(directory,controls,storage);result={}
                self.execute(plans,old,new,result)
                creates=[v for v in self.commands if v[1]=='create']
                self.assertEqual(len(creates),3)
                for i,argv in enumerate(creates,1):
                    self.assertEqual(argv[argv.index('--user')+1],old[i]['user'])
                    self.assertEqual(argv[-3:], [old[i]['image_id'],'--config','/config.json'])
                    self.assertIn(f'type={storage},src=state-{i},dst=/state',argv)
                    self.assertIn(f"type=bind,src={plans[i]['measured_config']},dst=/config.json,readonly",argv)
                    self.assertNotIn(plans[i]['config_mount'],argv)
                self.assertIn(['docker','network','connect','--alias','world-node-1','peer',new[1]['container_id']],self.commands)
                self.assertTrue(all(c[1] in ('rm','create','network') for c in self.commands))
                self.assertEqual([e for e in self.events if e[0]=='verify'],[('verify',1),('verify',2),('verify',3)])
                self.assertGreater(self.events.index(('verify',1)),max(i for i,e in enumerate(self.events) if e==('command','create')))
                for i in (1,2,3):
                    row=result['preload_container_recreation'][str(i)]
                    self.assertEqual(row['old_state_mount'],row['new_state_mount'])
                    self.assertEqual(row['state'],'readback_verified_stopped')
                    self.assertNotEqual(row['old_container_id'],row['new_container_id'])

    def test_reinspect_change_refuses_before_removal_and_bad_retained_mount_before_readback(self):
        with tempfile.TemporaryDirectory() as directory:
            plans,old,new=self.fixture(directory)
            with self.assertRaisesRegex(ValueError,'changed before removal'):
                self.execute(plans,old,new,{},lambda value:value.update(container_id='e'*64))
            self.assertEqual(self.commands,[])
        with tempfile.TemporaryDirectory() as directory:
            plans,old,new=self.fixture(directory);new[1]['mounts']['/state']['Source']='unrelated-volume'
            with self.assertRaisesRegex(ValueError,'retained mount changed'):self.execute(plans,old,new,{})
            self.assertFalse(any(e[0]=='verify' for e in self.events))
            self.assertFalse(any(c[1]=='start' for c in self.commands))

    def test_readback_failure_keeps_structured_result_all_nodes_stopped(self):
        with tempfile.TemporaryDirectory() as directory:
            plans,old,new=self.fixture(directory);result={}
            with self.assertRaisesRegex(RuntimeError,'VM config readback refused'):
                self.execute(plans,old,new,result,mismatch=2)
            self.assertEqual(result['preload_restart_config_readback']['2']['error_class'],'byte_mismatch')
            self.assertEqual(len([c for c in self.commands if c[1]=='create']),3)
            self.assertFalse(any(c[1]=='start' for c in self.commands))
            self.assertEqual([e for e in self.events if e[0]=='verify'],[('verify',1),('verify',2)])

    def test_snapshot_refuses_foreign_running_paused_restarting_or_missing_state(self):
        from supervision_load import node_recreation_snapshot,OWNERSHIP_LABEL
        value={'Id':'1'*64,'Image':'sha256:'+'a'*64,'Config':{'Labels':{OWNERSHIP_LABEL:'run'},'User':'501:20','Cmd':['--config','/config.json']},
               'State':{'Status':'exited','Running':False,'Paused':False,'Restarting':False},'Mounts':[],'NetworkSettings':{'Networks':{}}}
        changes=[lambda x:x['Config']['Labels'].update({OWNERSHIP_LABEL:'other'}),
                 lambda x:x['State'].update(Status='running',Running=True),lambda x:x['State'].update(Paused=True),
                 lambda x:x['State'].update(Restarting=True),lambda x:x['State'].pop('Running')]
        for change in changes:
            refused=copy.deepcopy(value);change(refused)
            with patch('supervision_load.command',return_value=json.dumps([refused]).encode()):
                with self.assertRaises(ValueError):node_recreation_snapshot('node','run')
        with patch('supervision_load.command',return_value=json.dumps([value]).encode()):
            self.assertEqual(node_recreation_snapshot('node','run')['container_id'],'1'*64)


if __name__=="__main__": unittest.main()
