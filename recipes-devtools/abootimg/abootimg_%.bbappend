# Whinlatter compatibility: S is automatically set correctly for git recipes
# No longer need to manually set S = "${WORKDIR}/git"

# Skip license-checksum QA for scarthgap compatibility
ERROR_QA:remove = "license-checksum"
WARN_QA:append = " license-checksum"
