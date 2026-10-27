import os
import unittest
from concurrent.futures import ThreadPoolExecutor
import wendy_core


class CoreTest(unittest.TestCase):
    def test_contract(self):
        self.assertEqual(wendy_core.__all__, ["WendyHelloWorld", "WendyCoreVersion"])
        self.assertEqual(wendy_core.WendyHelloWorld(), 42)

    def test_version(self):
        self.assertEqual(wendy_core.WendyCoreVersion(), os.environ["WENDY_CORE_VERSION"])
        with ThreadPoolExecutor(max_workers=4) as executor:
            values = list(executor.map(lambda _: wendy_core.WendyCoreVersion(), range(100)))
        self.assertEqual(values, [os.environ["WENDY_CORE_VERSION"]] * 100)

    def test_concurrent_calls(self):
        with ThreadPoolExecutor(max_workers=4) as executor:
            values = list(executor.map(lambda _: wendy_core.WendyHelloWorld(), range(100)))
        self.assertEqual(values, [42] * 100)


if __name__ == "__main__":
    unittest.main()
