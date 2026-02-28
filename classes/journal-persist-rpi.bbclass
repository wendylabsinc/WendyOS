
# Set the '/var/log' location, i.e. either on persistent storage
# of on volatile filesystem.
# By default, it uses the volatile storage.
WENDYOS_PERSIST_JOURNAL_LOGS ?= "0"

# Turn the value into an override we can key off of
OVERRIDES:append = ":journal_persist-${@'on' if d.getVar('WENDYOS_PERSIST_JOURNAL_LOGS') == '1' else 'off'}"

# With modern OE-Core, whether '/var/log' is a directory or a symlink,
# it is decided by the FILESYSTEM_PERMS_TABLES during rootfs packaging.
FILESYSTEM_PERMS_TABLES = "files/fs-perms.txt"
FILESYSTEM_PERMS_TABLES:journal_persist-off += "${@'files/fs-perms-volatile-log.txt' \
    if __import__('os').path.exists(__import__('os').path.join(d.getVar('COREBASE'),'meta','files','fs-perms-volatile-log.txt')) else ''}"
FILESYSTEM_PERMS_TABLES:journal_persist-on  += "${@'files/fs-perms-persistent-log.txt' \
    if __import__('os').path.exists(__import__('os').path.join(d.getVar('COREBASE'),'meta','files','fs-perms-persistent-log.txt')) else ''}"
