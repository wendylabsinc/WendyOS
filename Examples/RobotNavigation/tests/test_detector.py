import math
import unittest

from robot_navigation.detector import IoUTracker, intersection_over_union, resize_dimensions, restore_box


class DetectorTrackingTests(unittest.TestCase):
    def test_unique_overlaps_preserve_ids_even_if_detection_order_changes(self):
        tracker = IoUTracker()
        a, b = (0,0,50,100), (100,0,150,100)
        ids = tracker.update([a, b], 1.0)
        moved = tracker.update([(102,0,152,100), (2,0,52,100)], 1.1)
        self.assertEqual([ids[1], ids[0]], moved)

    def test_ambiguous_overlap_does_not_pick_largest_or_nearest_match(self):
        tracker = IoUTracker()
        old = tracker.update([(0,0,50,100), (55,0,105,100)], 1.0)
        new = tracker.update([(20,0,80,100)], 1.1)
        self.assertNotIn(new[0], old)

    def test_split_detection_does_not_copy_an_old_id(self):
        tracker = IoUTracker()
        old = tracker.update([(0,0,100,100)], 1.0)
        new = tracker.update([(0,0,50,100), (50,0,100,100)], 1.1)
        self.assertEqual(2, len(set(new)))
        self.assertTrue(set(old).isdisjoint(new))

    def test_current_overlapping_people_receive_new_ids(self):
        tracker = IoUTracker()
        old = tracker.update([(0,0,50,100), (60,0,110,100)], 1.0)
        new = tracker.update([(10,0,60,100), (40,0,90,100)], 1.1)
        self.assertTrue(set(old).isdisjoint(new))

    def test_absence_gap_duplicate_time_and_clock_reversal_invalidate_ids(self):
        box = (0,0,50,100)
        for next_stamp in (1.0, 0.9, 2.0):
            tracker = IoUTracker()
            old = tracker.update([box], 1.0)
            self.assertNotEqual(old, tracker.update([box], next_stamp))
        tracker = IoUTracker()
        old = tracker.update([box], 1.0)
        self.assertEqual([], tracker.update([], 1.1))
        self.assertNotEqual(old, tracker.update([box], 1.2))

    def test_nonoverlapping_person_never_inherits_an_id(self):
        tracker = IoUTracker()
        old = tracker.update([(0,0,50,100)], 1.0)
        self.assertNotEqual(old, tracker.update([(80,0,130,100)], 1.1))

    def test_invalid_or_excessive_detections_fail_closed(self):
        tracker = IoUTracker(max_detections=1)
        old = tracker.update([(0,0,50,100)], 1.0)
        with self.assertRaises(ValueError):
            tracker.update([(0,0,50,100)]*2, 1.1)
        self.assertNotEqual(old, tracker.update([(0,0,50,100)], 1.2))
        for box in ((0,0,math.nan,100), (-1,0,50,100), (0,0,0,100)):
            with self.assertRaises(ValueError):
                tracker.update([box], 1.3)

    def test_iou_and_restored_box_coordinates(self):
        self.assertAlmostEqual(1/3, intersection_over_union((0,0,10,10), (5,0,15,10)))
        self.assertEqual(0, intersection_over_union((0,0,10,10), (20,0,30,10)))
        self.assertEqual((640,360), resize_dimensions(1920,1080))
        self.assertEqual((320,480), resize_dimensions(800,1200))
        self.assertEqual((30,60,330,660), restore_box((10,20,100,200), (1920,1080), (640,360)))
        with self.assertRaises(ValueError):
            resize_dimensions(100000,100000)


if __name__ == "__main__":
    unittest.main()
