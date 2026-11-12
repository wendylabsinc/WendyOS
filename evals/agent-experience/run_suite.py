"""Run a versioned suite using an operator-supplied lab configuration. Dry-run unless --execute."""
import argparse
import json
from pathlib import Path
import subprocess
import sys

HERE = Path(__file__).resolve().parent


def command(suite, config, wendy, treatment, output, repetitions=10, execute=False):
    catalog = json.loads((HERE / 'suites.json').read_text())
    selected = catalog[suite]
    argv = [sys.executable, str(HERE / 'benchmark.py'), '--config', str(config), '--wendy', str(wendy),
            '--treatment-root', str(treatment), '--output', str(output), '--repetitions', str(repetitions), '--seed', '42']
    for key, flag in (('tasks', '--task'), ('scenarios', '--scenario')):
        for name in selected[key]:
            argv += [flag, name]
    return argv + (['--execute'] if execute else [])


def run_pair(suite, config, baseline_root, candidate_root, output, repetitions=40):
    """Measure both sides even when agents fail; stop before reusing an unsafe lab state."""
    import compare
    runs = []
    for side, root in (("baseline", Path(baseline_root)), ("candidate", Path(candidate_root))):
        destination = Path(output) / side
        code = subprocess.run(command(suite, config, root / "go/bin/wendy", root, destination,
                                      repetitions, execute=True), check=False).returncode
        if code not in (0, 1):
            return code or 3
        manifests = list(destination.glob("*/manifest.json"))
        if len(manifests) != 1:
            print(f"{side}: expected exactly one run manifest")
            return 3
        run = manifests[0].parent
        try:
            rows = [json.loads(line) for line in (run / "results.jsonl").read_text().splitlines()]
        except (OSError, ValueError):
            return 3
        if (run / "halt.json").exists() or any(r.get("cleanup_ok") is False or
                r.get("status") in ("host_state_changed", "mcp_contaminated", "mcp_unverified", "interrupted")
                for r in rows):
            print(f"{side}: lab state requires inspection; refusing another treatment")
            return 3
        runs.append(run)
    return compare.main(["--baseline", str(runs[0]), "--candidate", str(runs[1]),
                         "--output", str(Path(output) / "comparison")])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--suite', choices=('smoke', 'nightly'), default='smoke')
    for name in ('config', 'wendy', 'treatment', 'output'):
        parser.add_argument('--' + name, type=Path, required=True)
    parser.add_argument('--repetitions', type=int, default=10)
    parser.add_argument('--execute', action='store_true')
    args = parser.parse_args()
    if args.repetitions < 1:
        parser.error('repetitions must be positive')
    return subprocess.run(command(**vars(args)), check=False).returncode


if __name__ == '__main__':
    sys.exit(main())
