"""Isolated bindings to the qualified ordinary two-Agent lifecycle."""
from pathlib import Path
import sys

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from integration.people import libraries as base
from integration.hazard import libraries as hazard

stage, runtime, pair, startup = base.stage, base.runtime, base.pair, base.startup
people_stage, load = hazard.people_stage, base.load


def load_runtime(model, stage_module, control):
    return load('nativesensorfusion_people_runtime', HERE.parent / 'people/run.py',
                {'libraries': base, 'model': model, 'stage': stage_module, 'control': control})
