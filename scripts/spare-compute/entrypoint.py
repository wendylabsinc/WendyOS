import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent / "model"))

from lease_runner import admission_mode

if os.environ.get("ROLE") == "supervisor":
    if admission_mode() == "always":
        import json
        import signal
        print(json.dumps({"event": "admission_mode", "mode": "always", "monitoring": False}), flush=True)
        while True:
            signal.pause()
    else:
        from governor import main
        main()
else:
    from lease_runner import main
    review = os.environ.get("ROLE") == "review"
    os.environ["MODES"] = "review" if review else os.environ.get("MODES", "fuzz,mutation,race")
    main([sys.executable, str(Path(__file__).with_name("bootstrap.py"))], "gpu" if review else "cpu")
