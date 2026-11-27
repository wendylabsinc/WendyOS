"""Regression for prepare/publication replies sharing one line-oriented process."""
import concurrent.futures,threading,time,unittest
from unittest.mock import patch
from rising_inspection import SerialRPC,RisingInspectionDemo

class RPCOwnershipTests(unittest.TestCase):
    def test_parallel_publication_and_prepare_keep_their_own_replies(self):
        rpc=SerialRPC.__new__(SerialRPC);rpc.call_lock=threading.Lock()
        entered=threading.Event();release=threading.Event();second=threading.Event();queue=[]
        def transport(request,allow_error=False):
            queue.append(dict(reply_to=request['op']))
            if request['op']=='publish':
                entered.set();self.assertTrue(release.wait(1))
            return queue.pop(0)
        def prepare():second.set();return rpc.call(dict(op='prepare-inspection'))
        with patch('gas_replicas.RPC.call',side_effect=transport):
            with concurrent.futures.ThreadPoolExecutor(max_workers=2)as pool:
                publication=pool.submit(rpc.call,dict(op='publish'));self.assertTrue(entered.wait(1))
                selection=pool.submit(prepare);self.assertTrue(second.wait(1));time.sleep(.02);release.set()
                self.assertEqual(publication.result(1),dict(reply_to='publish'))
                self.assertEqual(selection.result(1),dict(reply_to='prepare-inspection'))
    def test_selection_waits_for_its_last_publication_acknowledgment(self):
        demo=RisingInspectionDemo.__new__(RisingInspectionDemo);future=concurrent.futures.Future();demo.publication_futures={'east':future};called=[]
        with patch('incident_inspection.InspectionDemo.poll_selection',side_effect=lambda _:called.append('selected')):
            with concurrent.futures.ThreadPoolExecutor(max_workers=1)as pool:
                pending=pool.submit(demo.poll_selection);time.sleep(.02);self.assertEqual(called,[])
                future.set_result({'published':True});pending.result(1)
        self.assertEqual(called,['selected'])

if __name__=='__main__':unittest.main()
