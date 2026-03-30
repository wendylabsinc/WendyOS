#!/bin/sh
# wendy-config-lib.sh — shared helpers for wendy-config
# Sourced by wendy-config.sh; not executed directly.

STAMP_FILE="/var/lib/wendy-config.done"
CONF_TMPFS="/run/wendy-config"

# wc_log LEVEL message...
# Writes a line to stdout; systemd captures it into the journal.
wc_log() {
    _wc_level="$1"
    shift
    echo "wendy-config [${_wc_level}]: $*"
}

# wc_stamp
# Creates the stamp file that prevents re-runs on subsequent boots.
wc_stamp() {
    date -u +"%Y-%m-%dT%H:%M:%SZ" > "$STAMP_FILE"
    wc_log INFO "stamp written: $STAMP_FILE"
}

# wc_run_handlers CONF_PATH
# Called with the path to the copied wendy.conf after the original has been
# wiped from the partition.  Stub in Part 2; Part 3 (WiFi) extends this as
# a pure addition without modifying wendy-config.sh.
wc_run_handlers() {
    wc_log INFO "handlers: none registered"
}
