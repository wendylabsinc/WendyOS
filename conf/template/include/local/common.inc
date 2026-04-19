# Shared build settings — required by every board.

DL_DIR = "${TOPDIR}/../downloads"
SSTATE_DIR = "${TOPDIR}/../sstate-cache"
#TMPDIR = "${TOPDIR}/tmp"
DISTRO = "wendyos"
#EXTRA_IMAGE_FEATURES ?= "debug-tweaks"
USER_CLASSES = "buildstats"
PATCHRESOLVE = "noop"

BB_DISKMON_DIRS ??= "\
    STOPTASKS,${TMPDIR},1G,100K \
    STOPTASKS,${DL_DIR},1G,100K \
    STOPTASKS,${SSTATE_DIR},1G,100K \
    STOPTASKS,/tmp,100M,100K \
    HALT,${TMPDIR},100M,1K \
    HALT,${DL_DIR},100M,1K \
    HALT,${SSTATE_DIR},100M,1K \
    HALT,/tmp,10M,1K"

PACKAGECONFIG:append:pn-qemu-system-native = " sdl"
CONF_VERSION = "2"

# USB gadget function ordering (only effective when WENDYOS_USB_GADGET = "1")
#GADGET_FUNC_ORDER="ecm ncm"

# Debug features - can be overridden in local.conf or machine config
WENDYOS_DEBUG = "1"
WENDYOS_DEBUG_UART = "1"
