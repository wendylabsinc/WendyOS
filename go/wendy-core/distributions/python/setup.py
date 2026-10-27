import platform as host
import re
import sys

from setuptools import Distribution, setup
from wheel.bdist_wheel import bdist_wheel


class NativeDistribution(Distribution):
    def has_ext_modules(self):
        return True


class NativeWheel(bdist_wheel):
    def get_tag(self):
        _, _, platform = super().get_tag()
        if sys.platform == "darwin":
            # A universal2 Python interpreter does not make our Go dylib fat.
            minimum = re.match(r"macosx_(\d+)_(\d+)_", platform)
            if minimum is None:
                raise ValueError("Unexpected macOS wheel platform")
            architecture = {"arm64": "arm64", "x86_64": "x86_64"}[host.machine()]
            platform = f"macosx_{minimum[1]}_{minimum[2]}_{architecture}"
        return "py3", "none", platform


setup(
    packages=["wendy_core"],
    include_package_data=False,
    package_data={"wendy_core": ["native/*"]},
    distclass=NativeDistribution,
    cmdclass={"bdist_wheel": NativeWheel},
)
