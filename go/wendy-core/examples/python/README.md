# Python HelloWorld

Requires Go, a C compiler, and Python 3.9+. From the repository root:

```sh
python3 scripts/build.py native
cd examples/python
python3 -m venv .venv
.venv/bin/python -m pip install ../../distributions/python
.venv/bin/python hello.py
```

On Windows, use `python` for setup and `.venv/Scripts/python.exe` for the last two commands.

Expected output: `WendyHelloWorld() = 42`.

The Core package is installed from disk into an isolated virtual environment, including the staged Go shared library. There is no `PYTHONPATH` shortcut or registry dependency on Wendy Core. Reinstall with `--force-reinstall` after rebuilding Core.
