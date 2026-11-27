import struct
import unittest
from types import SimpleNamespace
from ros_contributor import cloud_points, transform_point

class ContributorTests(unittest.TestCase):
    def test_padded_rows_and_big_endian(self):
        fields = [SimpleNamespace(name=n, datatype=7, count=1, offset=i*4) for i,n in enumerate(('x','y','z'))]
        msg = SimpleNamespace(fields=fields, width=1, height=2, point_step=12, row_step=16,
                              is_bigendian=True, data=struct.pack('>fffIfffI',1,2,3,0,4,5,6,0))
        self.assertEqual(list(cloud_points(msg)), [(1,2,3),(4,5,6)])
        msg.data = msg.data[:-1]
        with self.assertRaises(ValueError): list(cloud_points(msg))
    def test_capture_frame_rotation(self):
        import math
        result = transform_point((1,0,0), (10,20,30), (0,0,math.sqrt(.5),math.sqrt(.5)))
        for actual, expected in zip(result, (10,21,30)): self.assertAlmostEqual(actual,expected)

if __name__ == '__main__': unittest.main()
