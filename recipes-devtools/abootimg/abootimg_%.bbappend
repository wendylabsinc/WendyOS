# Fix S path for scarthgap - git recipes unpack to ${WORKDIR}/git
S = "${WORKDIR}/git"

# Skip license-checksum QA for scarthgap compatibility
ERROR_QA:remove = "license-checksum"
WARN_QA:append = " license-checksum"
