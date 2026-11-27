"""Load frozen fixture helpers without changing their module globals or files.

The older standalone scripts use short absolute imports. Bind those only while
loading each module; this runner's model/stage modules stay separate afterward.
No helper constructs a runtime on import.
"""
import importlib.util
from pathlib import Path
import sys

HERE = Path(__file__).resolve().parent
if str(HERE.parents[1]) not in sys.path:
    sys.path.insert(0, str(HERE.parents[1]))


def load(name, path, bindings=None):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    prior = {key: sys.modules.get(key) for key in bindings or {}}
    try:
        sys.modules.update(bindings or {})
        spec.loader.exec_module(module)
    finally:
        for key, old in prior.items():
            if old is None:
                sys.modules.pop(key, None)
            else:
                sys.modules[key] = old
    return module


stage = load('people_multimodal_stage', HERE.parent / 'agentmultimodal/stage.py')
configs = load('people_multimodal_configs', HERE.parent / 'agentmultimodal/configs.py', {'stage': stage})
validation = load('people_multimodal_validation', HERE.parent / 'agentmultimodal/validation.py', {'stage': stage})
runtime = load('people_multimodal_runtime', HERE.parent / 'agentmultimodal/run.py',
               {'stage': stage, 'configs': configs, 'validation': validation})
pair = load('people_pair_model', HERE.parent / 'agentpair/model.py')
control = load('people_pair_control', HERE.parent / 'agentpair/control.py', {'model': pair})
startup = load('people_pair_startup', HERE.parent / 'agentpair/startup.py', {'model': pair})
