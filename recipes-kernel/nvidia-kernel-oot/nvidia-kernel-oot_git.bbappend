# R38.4.0 / whinlatter: S is correctly auto-set by bitbake.conf to ${UNPACKDIR}/${BP}
# All R38.2.x workarounds (wrong S path, EXTRA_OEMAKE overrides) have been removed.
# See nvidia-kernel-oot_%.bbappend for the do_qa_unpack override needed for multi-git.

# Downgrade license-checksum from error to warning (meta-tegra upstream issue)
ERROR_QA:remove = "license-checksum"
WARN_QA:append = " license-checksum"
