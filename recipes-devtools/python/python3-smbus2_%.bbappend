# Whinlatter compatibility: Fix S path
#
# The upstream recipe incorrectly overrides S with "${UNPACKDIR}/${BP}"
# which expands to python3-smbus2-0.5.0, but the PyPI tarball unpacks
# to smbus2-0.5.0 (without the python3- prefix).
#
# The pypi.bbclass default is correct: S = "${UNPACKDIR}/${PYPI_PACKAGE}-${PV}"
# We restore that here.

S = "${UNPACKDIR}/${PYPI_PACKAGE}-${PV}"
