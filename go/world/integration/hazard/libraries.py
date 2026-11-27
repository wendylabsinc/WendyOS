"""Reuse the qualified native lifecycle through isolated, explicit module bindings."""
from pathlib import Path
import sys

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from integration.people import libraries as base

stage, runtime, pair, startup = base.stage, base.runtime, base.pair, base.startup
people_stage = base.load('hazard_people_stage', HERE.parent / 'people/stage.py', {'libraries': base})


def load_runtime(model, stage_module, control):
    # Only this new module receives the bindings. Existing people and pair
    # module globals are never changed, and importing starts no runtime.
    return base.load('hazard_people_runtime', HERE.parent / 'people/run.py',
                     {'libraries': base, 'model': model, 'stage': stage_module, 'control': control})
