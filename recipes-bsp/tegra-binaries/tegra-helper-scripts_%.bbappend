# Fix UNPACKDIR variable expansion issue in scarthgap
# The base recipe uses S = "${UNPACKDIR}" which causes expansion errors

S = "${UNPACKDIR}"
