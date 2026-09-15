import base64
import unittest
from types import SimpleNamespace

from audio import MAX_PACKET_BYTES, MAX_PUBLISH_BYTES, decode_packet, summarize


class AudioPackets(unittest.TestCase):
    def test_lossless_bytes_and_unsigned_timestamp(self):
        data = bytes(range(256))
        result = summarize(SimpleNamespace(data=data, time_frame=2**64-1), "/audiosender", True)
        self.assertEqual(decode_packet(result["data_base64"]), data)
        self.assertEqual(result["time_frame"], 2**64-1)
        self.assertEqual(result["byte_count"], 256)
        self.assertEqual(base64.b64decode(result["preview_base64"]), data[:32])
        self.assertEqual(result["encoding"], "unknown")

    def test_default_summary_is_bounded(self):
        result = summarize(SimpleNamespace(data=bytes(MAX_PACKET_BYTES), time_frame=7), "/audioreceiver")
        self.assertNotIn("data_base64", result)
        self.assertLess(len(str(result)), 1024)

    def test_reject_bad_base64_and_oversized_packets(self):
        for value in ("", "////!", base64.b64encode(bytes(MAX_PUBLISH_BYTES+1)).decode()):
            with self.assertRaises(ValueError):
                decode_packet(value)
        with self.assertRaises(ValueError):
            summarize(SimpleNamespace(data=bytes(MAX_PACKET_BYTES+1), time_frame=0), "/audio")


if __name__ == "__main__":
    unittest.main()
