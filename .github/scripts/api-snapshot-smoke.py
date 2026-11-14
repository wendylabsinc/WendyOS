#!/usr/bin/env python3
"""Build and execute the API detector with the runner's real native dependencies."""

import json
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]


def main():
    with tempfile.TemporaryDirectory(prefix="api-snapshot-") as directory:
        scratch = Path(directory)
        binary = scratch / "snapshot"
        subprocess.run(["go", "build", "-o", str(binary), "./go/cmd/api-surface-snapshot"],
                       cwd=ROOT, check=True, timeout=600)
        output = subprocess.check_output([str(binary)], cwd=ROOT, text=True, timeout=30)
        snapshot = json.loads(output)
        commands = snapshot["commands"]
        assert commands, "CLI discovery returned no commands"
        install = next(c for c in commands if c["path"] == "wendy install")
        assert install["local_flags"], "Install flags were not discovered"
        baseline = scratch / "base.json"
        baseline.write_text(output)
        unchanged = subprocess.check_output([str(binary), "compare", str(baseline), str(baseline)],
                                            text=True, timeout=30).strip()
        assert unchanged == "low", unchanged
        removed = scratch / "removed.json"
        removed.write_text(json.dumps({"commands": [c for c in commands if c is not install]}))
        changed = subprocess.check_output([str(binary), "compare", str(baseline), str(removed)],
                                          text=True, timeout=30).strip()
        assert changed == "high", changed
        print(f"API detector discovered {len(commands)} commands and detected a removed install command")


if __name__ == "__main__":
    main()
