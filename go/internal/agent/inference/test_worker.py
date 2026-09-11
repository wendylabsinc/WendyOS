import threading
import unittest

from worker import StreamBytes


class StreamTests(unittest.TestCase):
    def test_byte_order_across_camera_chunks(self):
        stream = StreamBytes()
        stream.feed(b"abc")
        stream.feed(b"def")
        self.assertEqual(stream.read(2), b"ab")
        self.assertEqual(stream.read(100), b"c")
        self.assertEqual(stream.read(3), b"def")
        self.assertEqual(stream.pending, 0)
        self.assertEqual(stream.read(0), b"")

    def test_bounded_encoded_queue(self):
        stream = StreamBytes()
        self.assertTrue(stream.feed(b"x" * (8 << 20)))
        self.assertFalse(stream.feed(b"y"))
        stream.stop()
        self.assertEqual(stream.pending, 0)
        self.assertFalse(stream.feed(b"z"))

    def test_stop_unblocks_libav_read(self):
        stream = StreamBytes()
        result = []
        reader = threading.Thread(target=lambda: result.append(stream.read(4096)))
        reader.start()
        stream.stop()
        reader.join(timeout=1)
        self.assertFalse(reader.is_alive())
        self.assertEqual(result, [b""])


if __name__ == "__main__":
    unittest.main()
