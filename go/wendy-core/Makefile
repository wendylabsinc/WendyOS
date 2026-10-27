PYTHON ?= python3
BUILD_DIR := .build/sdk
PROTO := proto/wendy/core/v1/core.proto

.PHONY: all check generate native wasm javascript python kotlin swift documentation documentation-manifest compare-clean

all: check

$(BUILD_DIR)/wendy-core.pb: $(PROTO)
	$(PYTHON) scripts/tasks.py descriptor

documentation-manifest: $(BUILD_DIR)/wendy-core.pb
	$(PYTHON) scripts/tasks.py manifest

generate: documentation-manifest
	$(PYTHON) scripts/tasks.py generate

native wasm:
	$(PYTHON) scripts/build.py $@

javascript python kotlin: native
	$(PYTHON) scripts/tasks.py $@

# Swift links its own static Go build, not the shared library.
swift:
	$(PYTHON) scripts/tasks.py $@

javascript: wasm

check: generate native
	$(PYTHON) scripts/tasks.py check

compare-clean: documentation-manifest
	$(PYTHON) scripts/tasks.py compare

# Snapshot extraction does not depend on Doxygen; only rendering does.
documentation: documentation-manifest
	$(PYTHON) scripts/tasks.py documentation
