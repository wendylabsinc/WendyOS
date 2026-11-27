"""Input routing checks without moving the user's cursor or opening a window."""
import unittest
from unittest.mock import Mock
from sim.collaborative_viewer import OfficeView, arc_points


class ArcControlsTests(unittest.TestCase):
    def view(self):
        view = OfficeView.__new__(OfficeView)
        view.glfw = Mock(MOUSE_BUTTON_LEFT=0, PRESS=1, RELEASE=0)
        view.glfw.get_window_size.return_value = (800, 600)
        view.width, view.height, view.panel = 1600, 1200, 330
        view.window = None
        view.runtime = Mock()
        view.flags = {'links': True}
        view.buttons = []
        view.dragged = False
        view.arc_hits = [('atlas--hub', [(800, 700), (900, 750)]), ('hub--iris', [(800, 500), (900, 550)])]
        return view

    def test_retina_arc_click_routes_to_the_nearest_robot(self):
        view = self.view()
        view.glfw.get_cursor_pos.return_value = (425, 237.5)
        view.mouse(None, 0, 0, 0)
        view.runtime.toggle_edge.assert_called_once_with('atlas--hub')

    def test_orbit_drag_hidden_links_and_empty_space_do_not_cut_links(self):
        for mode in ('drag', 'hidden', 'empty'):
            view = self.view()
            view.glfw.get_cursor_pos.return_value = (425, 237.5)
            if mode == 'drag': view.dragged = True
            if mode == 'hidden': view.flags['links'] = False
            if mode == 'empty': view.glfw.get_cursor_pos.return_value = (700, 400)
            view.mouse(None, 0, 0, 0)
            view.runtime.toggle_edge.assert_not_called()

    def test_arch_is_anchored_at_both_devices(self):
        points = arc_points([0, 0, 1], [4, 0, 1], height=2)
        self.assertEqual(points[0].tolist(), [0, 0, 1])
        self.assertEqual(points[-1].tolist(), [4, 0, 1])
        self.assertAlmostEqual(points[12][2], 3)


if __name__ == '__main__':
    unittest.main()
