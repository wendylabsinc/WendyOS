import unittest
from unittest.mock import patch

import tasks


class KotlinPrerequisiteTests(unittest.TestCase):
    def test_missing_gradle_reports_requirement_before_building_jni(self):
        with patch.object(tasks.shutil, 'which', return_value=None), \
                patch.object(tasks.build, 'jni') as jni:
            with self.assertRaisesRegex(SystemExit, r'Gradle 9\.8\.1.*PATH'):
                tasks.kotlin()
            jni.assert_not_called()
