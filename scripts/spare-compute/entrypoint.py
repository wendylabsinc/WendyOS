import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent / "model"))

if os.environ.get("ROLE") == "supervisor":
    from governor import main
    main()
else:
    from lease_runner import main
    review = os.environ.get("ROLE") == "review"
    os.environ["MODES"] = "review" if review else os.environ.get("MODES", "fuzz,mutation,race")
    main([sys.executable, str(Path(__file__).with_name("bootstrap.py"))], "gpu" if review else "cpu")
