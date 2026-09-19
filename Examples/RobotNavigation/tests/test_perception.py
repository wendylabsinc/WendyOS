from array import array
from types import SimpleNamespace as NS
import math
import struct
import unittest

from robot_navigation.grounding import GroundingError
from robot_navigation.perception import camera_intrinsics, decode_depth, detections_from_message, stamp_seconds


def header(sec=10, nano=0, frame="camera_optical"):
    return NS(stamp=NS(sec=sec,nanosec=nano), frame_id=frame)


def camera_info():
    return NS(width=640,height=480,header=header(),
              p=[500,0,320,0,0,500,240,0,0,0,1,0], r=[1,0,0,0,1,0,0,0,1],
              binning_x=0,binning_y=0,roi=NS(x_offset=0,y_offset=0,width=0,height=0))


def detection():
    hypothesis = NS(hypothesis=NS(class_id="person",score=0.9))
    bbox = NS(center=NS(position=NS(x=100.0,y=100.0),theta=0.0),size_x=40.0,size_y=80.0)
    return NS(header=header(),bbox=bbox,id="tracked-person",results=[hypothesis])


class PerceptionDecodeTests(unittest.TestCase):
    def test_depth_millimeters_with_padded_rows_and_big_endian(self):
        for big in (False,True):
            prefix = ">" if big else "<"
            data = struct.pack(prefix+"2H", 1000,2000) + b"xx" + struct.pack(prefix+"2H", 0,3500) + b"xx"
            msg = NS(width=2,height=2,step=6,data=data,encoding="16UC1",is_bigendian=big,header=header())
            depth = decode_depth(msg)
            self.assertEqual([1.0,2.0,0.0,3.5], list(depth.values))
            self.assertEqual("camera_optical", depth.frame_id)

    def test_float_depth_preserves_meters_and_invalid_values_for_grounding(self):
        msg = NS(width=2,height=1,step=8,data=struct.pack(">2f", 1.25,math.nan),
                 encoding="32FC1",is_bigendian=True,header=header())
        values = decode_depth(msg).values
        self.assertEqual(1.25, values[0])
        self.assertTrue(math.isnan(values[1]))

    def test_unsupported_encoding_and_malformed_lengths_are_rejected(self):
        msg = NS(width=2,height=1,step=4,data=b"1234",encoding="16UC1",is_bigendian=False,header=header())
        for key, value in (("encoding","8UC1"),("step",2),("data",b"1"),("width",99999)):
            changed = NS(**vars(msg))
            setattr(changed,key,value)
            with self.assertRaises(GroundingError):
                decode_depth(changed)

    def test_rectified_camera_uses_p_and_rejects_missing_or_unsupported_calibration(self):
        info = camera_info()
        intrinsics = camera_intrinsics(info)
        self.assertEqual((500,500,320,240), (intrinsics.fx,intrinsics.fy,intrinsics.cx,intrinsics.cy))
        for modify in (lambda i: setattr(i,"p",[0]*12),
                       lambda i: i.p.__setitem__(3,10),
                       lambda i: i.r.__setitem__(0,0),
                       lambda i: setattr(i,"binning_x",2),
                       lambda i: setattr(i.roi,"x_offset",10)):
            info = camera_info()
            modify(info)
            with self.assertRaises(GroundingError):
                camera_intrinsics(info)

    def test_detection_conversion_keeps_id_and_pixel_bounds(self):
        converted = detections_from_message(NS(header=header(),detections=[detection()]), 0.05)
        self.assertEqual("tracked-person", converted[0].id)
        self.assertEqual((80,60,120,140), converted[0].bbox)

    def test_detection_header_rotation_and_ambiguous_classes_are_rejected(self):
        for modify in (lambda d: setattr(d.header,"frame_id","wrong"),
                       lambda d: setattr(d.header.stamp,"nanosec",100_000_000),
                       lambda d: setattr(d.bbox.center,"theta",0.1),
                       lambda d: d.results.append(NS(hypothesis=NS(class_id="poster",score=0.9)))):
            item = detection()
            modify(item)
            with self.assertRaises(GroundingError):
                detections_from_message(NS(header=header(),detections=[item]),0.05)

    def test_malformed_ros_timestamps_rejected(self):
        self.assertEqual(10.5, stamp_seconds(header(nano=500_000_000)))
        for source in (header(sec=-1), header(nano=1_000_000_000), header(nano=-1)):
            with self.assertRaises(GroundingError):
                stamp_seconds(source)


if __name__ == "__main__":
    unittest.main()
