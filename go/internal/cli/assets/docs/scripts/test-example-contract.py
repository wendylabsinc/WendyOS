#!/usr/bin/env python3
"""Check real upstream trees, then prove the rendered-doc regressions fail."""
import argparse
import importlib.util
from pathlib import Path
import tempfile

spec = importlib.util.spec_from_file_location("examples", Path(__file__).with_name("check-example-contract.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


def verify(root, templates, native):
    report = module.check(root, templates, native)
    print(report)
    # Only the two actual exported HTML files are copied. No fabricated
    # catalogs or mock filesystem responses participate in these controls.
    with tempfile.TemporaryDirectory(prefix="docs-example-controls-") as temporary:
        copy = Path(temporary)
        routes = ["advanced/wendy-lite/getting-started/index.html", "advanced/wendy-lite/native-apps/index.html"]
        for route in routes:
            target = copy / route
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes((root / route).read_bytes())
        controls = [
            (routes[0], ">blink-rgb-led</code>", ">blink-rgb</code>", "prose example differs"),
            (routes[0], "blink-rgb-led/main/main.c", "blink-rgb-led/main/missing.c", "editable file is absent"),
            (routes[1], "wendy-lite-native-apps/tree/main/blink-rgb", "wendy-lite-native-apps/tree/main/missing-example", "directory does not exist"),
        ]
        for route, before, after, expected in controls:
            path = copy / route
            original = path.read_text()
            if before not in original:
                raise AssertionError(f"control was not applied: {before}")
            path.write_text(original.replace(before, after, 1))
            try:
                module.check(copy, templates, native)
            except ValueError as error:
                if expected not in str(error):
                    raise AssertionError(f"control failed for wrong reason: {error}") from error
                print(f"PASS negative control: {expected}")
            else:
                raise AssertionError(f"broken rendered example passed: {after}")
            finally:
                path.write_text(original)
    return report


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--templates", type=Path, required=True)
    parser.add_argument("--native-examples", type=Path, required=True)
    args = parser.parse_args()
    verify(args.root, args.templates, args.native_examples)
