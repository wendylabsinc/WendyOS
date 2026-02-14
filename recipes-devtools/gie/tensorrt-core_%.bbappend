# Fix UNPACKDIR compatibility for scarthgap
# The upstream recipe uses subdir=tensorrt, so files unpack to ${WORKDIR}/tensorrt
S = "${WORKDIR}/tensorrt"

# Skip license-checksum QA for scarthgap compatibility
ERROR_QA:remove = "license-checksum"
WARN_QA:append = " license-checksum"
