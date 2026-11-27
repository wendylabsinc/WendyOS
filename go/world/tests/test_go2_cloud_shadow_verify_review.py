"""Independent mutations of process chronology and retained evidence bounds."""
import copy
import unittest

from sim.go2_cloud_stream import Assembler
from sim.test_go2_cloud_shadow import Clock, capture, rows, row, completion, MS
from sim.test_go2_cloud_stream import odom_wire, STAMP
from tests.test_go2_cloud_shadow_verify import shadow, verify


def stepped_report():
    clock=Clock()
    class Decoder(Assembler):
        def ingest(self,value,now):
            events=super().ingest(value,now)
            clock.advance(MS)
            return events
    c,_=capture(clock,assembler_factory=Decoder)
    values=rows()+[row('/odom',odom_wire(STAMP+10*MS),99)]
    lines=[]
    for value in values+[completion(values)]:
        clock.advance(MS);line=shadow.encoded(value);lines.append(line);c.ingest_line(line)
    clock.advance(MS);eof=c.tick()['monotonic_after_ns']
    clock.advance(MS);exited=c.tick()['monotonic_after_ns']
    return c.finish(0,True,{'reaped':True,'actions':[],'exit_code':0},
                    stdout_bytes=sum(map(len,lines)),eof_observed_ns=eof,
                    exit_observed_ns=exited,stderr_observed_bytes=0)


def candidate_clock(report,value):
    candidate=report['candidates'][0];candidate['postdecode_clock']=copy.deepcopy(value)
    candidate['local_wall_ages_ns']={key:value['wall_ns']-stamp
                                   for key,stamp in candidate['original_capture_stamps_ns'].items()}


class VerifierReviewTests(unittest.TestCase):
    def reject(self,change,code):
        report=stepped_report();change(report)
        with self.assertRaisesRegex(ValueError,'^'+code+'$'):verify(report)

    def test_original_has_serial_decoder_completion_and_process_clocks(self):
        result=verify(stepped_report())
        self.assertTrue(result['integrity_valid']);self.assertTrue(result['capture_complete'])
        self.assertEqual(result['retained_candidates'],1)

    def test_completion_cannot_precede_last_decoded_row(self):
        self.reject(lambda r:r['helper_completion'].update(arrival_clock=r['start_clock']),
                    'completion_before_processed_rows')

    def test_eof_and_exit_cannot_precede_work_or_follow_last_clock(self):
        self.reject(lambda r:r.update(helper_eof_observed_monotonic_ns=r['start_clock']['monotonic_after_ns']),
                    'eof_observation_order')
        self.reject(lambda r:r.update(helper_exit_observed_monotonic_ns=r['helper_eof_observed_monotonic_ns']-1),
                    'exit_observation_order')
        self.reject(lambda r:r.update(helper_exit_observed_monotonic_ns=r['last_clock']['monotonic_after_ns']+1),
                    'exit_observation_order')

    def test_candidate_cannot_precede_decoder_or_follow_next_input(self):
        self.reject(lambda r:candidate_clock(r,r['messages'][4]['arrival_clock']),
                    'candidate_clock_precedes_decode')
        self.reject(lambda r:candidate_clock(r,r['messages'][5]['postdecode_clock']),
                    'candidate_checked_after_next_row')

    def test_clock_brackets_cannot_overlap_preceding_work(self):
        self.reject(lambda r:r['messages'][5]['arrival_clock'].update(
            monotonic_before_ns=r['messages'][4]['postdecode_clock']['monotonic_after_ns']-1),
                    'arrival_deadline_or_order')

    def test_ready_event_cannot_be_removed_duplicated_or_retimed(self):
        self.reject(lambda r:r.update(events=[e for e in r['events'] if e['kind']!='assembler_ready']),
                    'missing_or_reordered_ready_event')
        self.reject(lambda r:r['events'].append(copy.deepcopy(r['events'][0])),
                    'duplicate_ready_event')
        self.reject(lambda r:r['events'][0].update(observed_monotonic_ns=0),
                    'ready_event_decode_time')

    def test_complete_close_and_cleanup_are_consistent(self):
        self.reject(lambda r:r['admission_closed'].update(monotonic_ns=0),
                    'complete_admission_close')
        self.reject(lambda r:r['admission_closed'].update(reason='invented'),
                    'complete_admission_close')
        self.reject(lambda r:r['cleanup'].update(actions=['terminate']),
                    'cleanup_interrupted_success')
        self.reject(lambda r:r['cleanup'].update(actions=['kill']),
                    'cleanup_actions')

    def test_stdout_count_cannot_be_less_than_original_rows(self):
        self.reject(lambda r:r.update(stdout_bytes=0),'stdout_count_below_retained_bytes')

    def test_observed_terminal_and_stderr_sizes_are_bounded(self):
        self.reject(lambda r:r.update(partial_stdout_observed_bytes=(256<<10)+65537),
                    'integer_bound')
        self.reject(lambda r:r.update(stderr_observed_bytes=(128<<10)+1),'integer_bound')
        self.reject(lambda r:r.update(terminal_input={'prefix_hex':'','observed_bytes':(256<<10)+65537,
                                                       'sha256':'0'*64,'truncated':True}),
                    'integer_bound')

    def test_complete_capture_cannot_include_unconsumed_terminal_bytes(self):
        self.reject(lambda r:r.update(terminal_input={'prefix_hex':'7b','observed_bytes':1,
                                                       'sha256':shadow.sha(b'{'),'truncated':False}),
                    'terminal_bytes_in_success')

    def test_completion_reason_and_counts_cannot_be_promoted(self):
        self.reject(lambda r:r['helper_completion']['row'].update(reason='invented'),'native_completion')
        self.reject(lambda r:r['helper_completion']['row'].update(reason='interrupted'),'false_capture_complete')
        self.reject(lambda r:r.update(max_clouds=2),'native_max_clouds_count')

    def test_refs_cannot_substitute_later_odometry(self):
        def substitute(r):
            later=r['messages'][5];record=later['row']
            r['candidates'][0]['sample_references']['odom']={
                'message_index':5,'topic':'/odom','serialized_sha256':later['serialized_sha256'],
                'publisher_gid':record['publisher_gid'],'source_timestamp_ns':record['source_timestamp_ns'],
                'received_timestamp_ns':record['received_timestamp_ns'],
                'arrived_monotonic_ns':later['arrival_clock']['monotonic_after_ns']}
        self.reject(substitute,'sample_reference_changed')


if __name__=='__main__':unittest.main()
