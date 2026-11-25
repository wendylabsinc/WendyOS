"""A slow optional evidence read must not block current replica adaptation."""
import threading
import unittest

from integration.livetwin.media import EvidenceCache


class EvidenceTests(unittest.TestCase):
    def test_current_results_do_not_wait_for_optional_evidence(self):
        entered, release = threading.Event(), threading.Event()
        saved = []
        cache = EvidenceCache('owned-test-container', '/test-only', lambda *v: saved.append(v))
        digest = 'a' * 64
        media = [{'source': 'unchanged-original', 'data': 'original-bytes'}]
        def slow_fetch(wanted):
            self.assertEqual(wanted, digest)
            entered.set()
            if not release.wait(2):
                raise RuntimeError('test release missing')
            return {digest: {'publication': {'media': media}}}
        cache._fetch = slow_fetch
        try:
            self.assertEqual(cache.available([digest]), [])
            self.assertTrue(entered.wait(1))
            # It returns while the actual worker is still blocked.
            self.assertEqual(cache.available([digest]), [])
            self.assertFalse(cache.pending[1].done())
            self.assertEqual(cache.available(['b' * 64]), [])
            release.set()
            cache.pending[1].result(timeout=1)
            self.assertEqual(cache.available([digest]), media)
            self.assertEqual(len(saved), 1)
            self.assertEqual(cache.available([]), [])
        finally:
            release.set()
            cache.close()


if __name__ == '__main__':
    unittest.main()
